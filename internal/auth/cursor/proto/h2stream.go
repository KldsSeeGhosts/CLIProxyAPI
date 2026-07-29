package proto

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	defaultInitialWindowSize = 65535 // HTTP/2 default
	maxFramePayload          = 16384 // HTTP/2 default max frame size
)

// H2Stream provides bidirectional HTTP/2 streaming for the Connect protocol.
// Go's net/http does not support full-duplex HTTP/2, so we use the low-level framer.
type H2Stream struct {
	framer   *http2.Framer
	conn     net.Conn
	streamID uint32
	mu       sync.Mutex
	id       string // unique identifier for debugging
	frameNum int64  // sequential frame counter for debugging

	dataCh chan []byte
	doneCh chan struct{}
	err    error

	// Send-side flow control
	sendWindow int32      // available bytes we can send on this stream
	connWindow int32      // available bytes on the connection level
	windowCond *sync.Cond // signaled when window is updated
	windowMu   sync.Mutex // protects sendWindow, connWindow
	closed     bool
}

// BootstrapError records whether a Cursor Run stream might have reached the
// service. Callers must only retry failures before the request is committed.
type BootstrapError struct {
	cause     error
	committed bool
}

func (e *BootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return "cursor h2 bootstrap failed"
	}
	return e.cause.Error()
}

func (e *BootstrapError) Unwrap() error { return e.cause }

// IsPreCommitBootstrapError reports whether err occurred before HTTP/2 request
// headers were written. Retrying any later failure could replay a Cursor turn.
func IsPreCommitBootstrapError(err error) bool {
	var bootstrap *BootstrapError
	return errors.As(err, &bootstrap) && !bootstrap.committed
}

// ID returns the unique identifier for this stream (for logging).
func (s *H2Stream) ID() string { return s.id }

// FrameNum returns the current frame number for debugging.
func (s *H2Stream) FrameNum() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frameNum
}

// DialH2Stream establishes a TLS+HTTP/2 connection and opens a new stream.
func DialH2Stream(host string, headers map[string]string) (*H2Stream, error) {
	return DialH2StreamContext(context.Background(), host, headers)
}

// DialH2StreamContext establishes the H2 connection while respecting ctx. The
// caller owns the deadline; all bootstrap phases are bounded until the stream
// is ready for normal long-running operation.
func DialH2StreamContext(ctx context.Context, host string, headers map[string]string) (*H2Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return nil, &BootstrapError{cause: fmt.Errorf("h2: TCP dial failed: %w", err)}
	}
	tlsConn := tls.Client(conn, &tls.Config{NextProtos: []string{"h2"}, ServerName: host})
	bootstrapDone := make(chan struct{})
	defer close(bootstrapDone)
	go func() {
		select {
		case <-ctx.Done():
			// A context cancellation must interrupt the manual framer's blocking
			// reads as well as the initial TCP dial/TLS handshake.
			_ = tlsConn.Close()
		case <-bootstrapDone:
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := tlsConn.SetDeadline(deadline); err != nil {
			tlsConn.Close()
			return nil, &BootstrapError{cause: fmt.Errorf("h2: set bootstrap deadline failed: %w", err)}
		}
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		tlsConn.Close()
		return nil, &BootstrapError{cause: fmt.Errorf("h2: TLS handshake failed: %w", err)}
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		tlsConn.Close()
		return nil, &BootstrapError{cause: fmt.Errorf("h2: server did not negotiate h2")}
	}

	framer := http2.NewFramer(tlsConn, tlsConn)

	// Client connection preface
	if _, err := tlsConn.Write([]byte(http2.ClientPreface)); err != nil {
		tlsConn.Close()
		return nil, &BootstrapError{cause: fmt.Errorf("h2: preface write failed: %w", err)}
	}

	// Send initial SETTINGS (tell server how much WE can receive)
	if err := framer.WriteSettings(
		http2.Setting{ID: http2.SettingInitialWindowSize, Val: 4 * 1024 * 1024},
		http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 100},
	); err != nil {
		tlsConn.Close()
		return nil, &BootstrapError{cause: fmt.Errorf("h2: settings write failed: %w", err)}
	}

	// Connection-level window update (for receiving)
	if err := framer.WriteWindowUpdate(0, 3*1024*1024); err != nil {
		tlsConn.Close()
		return nil, &BootstrapError{cause: fmt.Errorf("h2: window update failed: %w", err)}
	}

	// Read and handle initial server frames (SETTINGS, WINDOW_UPDATE)
	// Track server's initial window size (how much WE can send)
	serverInitialWindowSize := int32(defaultInitialWindowSize)
	connWindowSize := int32(defaultInitialWindowSize) // connection-level send window
	for i := 0; i < 10; i++ {
		f, err := framer.ReadFrame()
		if err != nil {
			tlsConn.Close()
			return nil, &BootstrapError{cause: fmt.Errorf("h2: initial frame read failed: %w", err)}
		}
		switch sf := f.(type) {
		case *http2.SettingsFrame:
			if !sf.IsAck() {
				sf.ForeachSetting(func(s http2.Setting) error {
					if s.ID == http2.SettingInitialWindowSize {
						serverInitialWindowSize = int32(s.Val)
						log.Debugf("h2: server initial window size: %d", s.Val)
					}
					return nil
				})
				framer.WriteSettingsAck()
			} else {
				goto handshakeDone
			}
		case *http2.WindowUpdateFrame:
			if sf.StreamID == 0 {
				connWindowSize += int32(sf.Increment)
				log.Debugf("h2: initial conn window update: +%d, total=%d", sf.Increment, connWindowSize)
			}
		default:
			// unexpected but continue
		}
	}
handshakeDone:

	// Build HEADERS
	streamID := uint32(1)
	var hdrBuf []byte
	enc := hpack.NewEncoder(&sliceWriter{buf: &hdrBuf})
	enc.WriteField(hpack.HeaderField{Name: ":method", Value: "POST"})
	enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
	enc.WriteField(hpack.HeaderField{Name: ":authority", Value: host})
	if p, ok := headers[":path"]; ok {
		enc.WriteField(hpack.HeaderField{Name: ":path", Value: p})
	}
	for k, v := range headers {
		if len(k) > 0 && k[0] == ':' {
			continue
		}
		enc.WriteField(hpack.HeaderField{Name: k, Value: v})
	}

	// Headers start a server-side request. From this point onward, a retry may
	// duplicate a turn, so even a local write failure is treated as committed.
	if err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: hdrBuf,
		EndStream:     false,
		EndHeaders:    true,
	}); err != nil {
		tlsConn.Close()
		return nil, &BootstrapError{cause: fmt.Errorf("h2: headers write failed: %w", err), committed: true}
	}
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		tlsConn.Close()
		return nil, &BootstrapError{cause: fmt.Errorf("h2: clear bootstrap deadline failed: %w", err), committed: true}
	}

	s := &H2Stream{
		framer:     framer,
		conn:       tlsConn,
		streamID:   streamID,
		dataCh:     make(chan []byte, 256),
		doneCh:     make(chan struct{}),
		id:         fmt.Sprintf("%d-%s", streamID, time.Now().Format("150405.000")),
		frameNum:   0,
		sendWindow: serverInitialWindowSize,
		connWindow: connWindowSize,
	}
	s.windowCond = sync.NewCond(&s.windowMu)
	go s.readLoop()
	return s, nil
}

// Write sends a DATA frame on the stream, respecting flow control.
func (s *H2Stream) Write(data []byte) error {
	for len(data) > 0 {
		chunk := data
		if len(chunk) > maxFramePayload {
			chunk = data[:maxFramePayload]
		}

		// Wait for flow control window
		s.windowMu.Lock()
		for !s.closed && (s.sendWindow <= 0 || s.connWindow <= 0) {
			s.windowCond.Wait()
		}
		if s.closed {
			s.windowMu.Unlock()
			return net.ErrClosed
		}
		// Limit chunk to available window
		allowed := int(s.sendWindow)
		if int(s.connWindow) < allowed {
			allowed = int(s.connWindow)
		}
		if len(chunk) > allowed {
			chunk = chunk[:allowed]
		}
		s.sendWindow -= int32(len(chunk))
		s.connWindow -= int32(len(chunk))
		s.windowMu.Unlock()

		s.mu.Lock()
		err := s.framer.WriteData(s.streamID, false, chunk)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		data = data[len(chunk):]
	}
	return nil
}

// Data returns the channel of received data chunks.
func (s *H2Stream) Data() <-chan []byte { return s.dataCh }

// Done returns a channel closed when the stream ends.
func (s *H2Stream) Done() <-chan struct{} { return s.doneCh }

// Err returns the error (if any) that caused the stream to close.
// Returns nil for a clean shutdown (EOF / StreamEnded).
func (s *H2Stream) Err() error { return s.err }

// Close tears down the connection.
func (s *H2Stream) Close() {
	s.windowMu.Lock()
	s.closed = true
	s.windowCond.Broadcast()
	s.windowMu.Unlock()
	s.conn.Close()
}

func (s *H2Stream) readLoop() {
	defer func() {
		s.windowMu.Lock()
		s.closed = true
		s.windowCond.Broadcast()
		s.windowMu.Unlock()
		close(s.doneCh)
	}()
	defer close(s.dataCh)

	for {
		f, err := s.framer.ReadFrame()
		if err != nil {
			if err != io.EOF {
				s.err = err
				log.Debugf("h2stream[%s]: readLoop error: %v", s.id, err)
			}
			return
		}

		// Increment frame counter
		s.mu.Lock()
		s.frameNum++
		s.mu.Unlock()

		switch frame := f.(type) {
		case *http2.DataFrame:
			if frame.StreamID == s.streamID && len(frame.Data()) > 0 {
				cp := make([]byte, len(frame.Data()))
				copy(cp, frame.Data())
				s.dataCh <- cp

				// Flow control: send WINDOW_UPDATE for received data
				s.mu.Lock()
				s.framer.WriteWindowUpdate(0, uint32(len(cp)))
				s.framer.WriteWindowUpdate(s.streamID, uint32(len(cp)))
				s.mu.Unlock()
			}
			if frame.StreamEnded() {
				return
			}

		case *http2.HeadersFrame:
			if frame.StreamEnded() {
				return
			}

		case *http2.RSTStreamFrame:
			s.err = fmt.Errorf("h2: RST_STREAM code=%d", frame.ErrCode)
			log.Debugf("h2stream[%s]: received RST_STREAM code=%d", s.id, frame.ErrCode)
			return

		case *http2.GoAwayFrame:
			s.err = fmt.Errorf("h2: GOAWAY code=%d", frame.ErrCode)
			return

		case *http2.PingFrame:
			if !frame.IsAck() {
				s.mu.Lock()
				s.framer.WritePing(true, frame.Data)
				s.mu.Unlock()
			}

		case *http2.SettingsFrame:
			if !frame.IsAck() {
				// Check for window size changes
				frame.ForeachSetting(func(setting http2.Setting) error {
					if setting.ID == http2.SettingInitialWindowSize {
						s.windowMu.Lock()
						delta := int32(setting.Val) - s.sendWindow
						s.sendWindow += delta
						s.windowMu.Unlock()
						s.windowCond.Broadcast()
					}
					return nil
				})
				s.mu.Lock()
				s.framer.WriteSettingsAck()
				s.mu.Unlock()
			}

		case *http2.WindowUpdateFrame:
			// Update send-side flow control window
			s.windowMu.Lock()
			if frame.StreamID == 0 {
				s.connWindow += int32(frame.Increment)
			} else if frame.StreamID == s.streamID {
				s.sendWindow += int32(frame.Increment)
			}
			s.windowMu.Unlock()
			s.windowCond.Broadcast()
		}
	}
}

type sliceWriter struct{ buf *[]byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}
