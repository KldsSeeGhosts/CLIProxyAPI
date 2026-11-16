package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type trackedCompatBody struct {
	io.Reader
	closed bool
}

func (b *trackedCompatBody) Close() error { b.closed = true; return nil }

type onceFailingCompatReader struct {
	err  error
	done bool
}

func (r *onceFailingCompatReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, "{}"), r.err
}

func TestWireCompatPreservesOneShotBodyReadError(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	want := errors.New("body read failed")
	c.Request.Body = io.NopCloser(&onceFailingCompatReader{err: want})
	rewriteWireCompatRequest(c)
	body, err := io.ReadAll(c.Request.Body)
	if string(body) != "{}" || !errors.Is(err, want) {
		t.Fatalf("body read failure changed: body=%q err=%v", body, err)
	}
}

func TestWireCompatOversizedBodyPreservesRemainderAndFraming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	size := int64(wireCompatMaxBodyBytes + 17)
	body := &trackedCompatBody{Reader: strings.NewReader(strings.Repeat("x", int(size)))}
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Body = body
	c.Request.ContentLength = size
	c.Request.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	rewriteWireCompatRequest(c)
	n, err := io.Copy(io.Discard, c.Request.Body)
	if err != nil || n != size || c.Request.ContentLength != size {
		t.Fatalf("body truncated or framing changed: bytes=%d length=%d err=%v", n, c.Request.ContentLength, err)
	}
	if errClose := c.Request.Body.Close(); errClose != nil || !body.closed {
		t.Fatalf("original body not closed: closed=%t err=%v", body.closed, errClose)
	}
}

func TestWireCompatWriterPreservesGinAccountingAndLateContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(WireCompatMiddleware())
	const body = `{"output":[{"type":"function_call","name":"functions__exec_command","arguments":"{}"}]}`
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Status(http.StatusCreated)
		if c.Writer.Status() != http.StatusCreated || c.Writer.Written() {
			t.Fatal("status must be staged before the first write")
		}
		c.Header("Content-Type", "application/json")
		c.Header("Content-Length", strconv.Itoa(len(body)))
		if _, err := c.Writer.WriteString(body); err != nil {
			t.Fatal(err)
		}
		if !c.Writer.Written() || c.Writer.Size() != len(body) {
			t.Fatalf("invalid writer accounting: written=%t size=%d", c.Writer.Written(), c.Writer.Size())
		}
		c.Status(http.StatusInternalServerError)
	})
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(openCodeDeclarations)))
	if rr.Code != http.StatusCreated || !strings.Contains(rr.Body.String(), `"namespace":"functions"`) {
		t.Fatalf("status or tool identity lost: status=%d body=%s", rr.Code, rr.Body)
	}
	if rr.Header().Get("Content-Length") != strconv.Itoa(rr.Body.Len()) {
		t.Fatal("transformed JSON has stale Content-Length")
	}
}

func TestWireCompatFragmentedStreamClearsLengthAndRestoresTool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(WireCompatMiddleware())
	event := "event: response.output_item.done\ndata: " +
		`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","name":"functions__exec_command","arguments":"{}"}}` + "\n\n"
	engine.POST("/v1/responses", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Content-Length", strconv.Itoa(len(event)))
		for _, b := range []byte(event) {
			if _, err := c.Writer.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
			c.Writer.Flush()
		}
	})
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(openCodeDeclarations)))
	if !rr.Flushed || rr.Header().Get("Content-Length") != "" {
		t.Fatal("SSE flush or Content-Length handling failed")
	}
	if !strings.Contains(rr.Body.String(), `"namespace":"functions"`) ||
		strings.Contains(rr.Body.String(), "functions__exec_command") {
		t.Fatalf("fragmented SSE tool identity was not restored: %s", rr.Body)
	}
}

type compatFlushFailure struct{ gin.ResponseWriter }

func (w compatFlushFailure) FlushError() error { return io.ErrClosedPipe }

func TestWireCompatFlushErrorStopsFurtherWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	writer := newToolSearchCompatWriter(compatFlushFailure{c.Writer})
	writer.Header().Set("Content-Type", "text/event-stream")
	if err := writer.FlushError(); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("disconnect error lost: %v", err)
	}
	if _, err := writer.Write([]byte("data: {}\n\n")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write continued after disconnect: %v", err)
	}
}
