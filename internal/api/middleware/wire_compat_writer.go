package middleware

// Response-side wire compatibility writer.
//
// The writer only engages on the Responses wire when the request recorded
// OpenCode Go tool identities or a client tool_search declaration. It mirrors
// the shim's ModifyResponse semantics:
//
//   - SSE (text/event-stream): events are translated on the fly and forwarded
//     immediately, preserving streaming behavior and Flush semantics.
//   - JSON (application/json): the body is buffered and transformed after the
//     handler completes, before headers are committed.
//   - Anything else (non-2xx status, compressed body, other content types) is
//     passed through unmodified.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
)

// wireCompatMode selects which response transform applies.
type wireCompatMode int

const (
	wireCompatModeToolSearch wireCompatMode = iota
	wireCompatModeOpenCode
)

// wireCompatResponseWriter wraps gin.ResponseWriter to translate Responses
// output back into the shapes the client harness declared.
type wireCompatResponseWriter struct {
	gin.ResponseWriter
	mode        wireCompatMode
	identities  openCodeIdentities
	translator  *wireCompatSSETranslator
	buffered    *bytes.Buffer
	status      int
	decided     bool
	passthrough bool
	written     bool
	size        int
	writeErr    error
}

func newToolSearchCompatWriter(w gin.ResponseWriter) *wireCompatResponseWriter {
	return &wireCompatResponseWriter{ResponseWriter: w, mode: wireCompatModeToolSearch, status: http.StatusOK}
}

func newOpenCodeCompatWriter(w gin.ResponseWriter, identities openCodeIdentities) *wireCompatResponseWriter {
	return &wireCompatResponseWriter{ResponseWriter: w, mode: wireCompatModeOpenCode, identities: identities, status: http.StatusOK}
}

// decide classifies the response once the status code and content type are
// known. Transforms only engage on 2xx, uncompressed SSE or JSON output.
func (w *wireCompatResponseWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	encoding := strings.ToLower(strings.TrimSpace(w.Header().Get("Content-Encoding")))
	if w.status < 200 || w.status >= 300 || (encoding != "" && encoding != "identity") {
		w.passthrough = true
		return
	}
	ct := strings.ToLower(w.Header().Get("Content-Type"))
	switch {
	case strings.Contains(ct, "text/event-stream"):
		w.translator = newWireCompatSSETranslator(w.mode, w.identities)
	case strings.Contains(ct, "application/json"):
		w.buffered = &bytes.Buffer{}
	default:
		w.passthrough = true
	}
	if w.translator != nil {
		w.Header().Del("Content-Length")
	}
}

func (w *wireCompatResponseWriter) WriteHeader(code int) {
	if w.Written() {
		return
	}
	// Gin stages the status without committing headers until the first write.
	w.ResponseWriter.WriteHeader(code)
	w.status = code
}

func (w *wireCompatResponseWriter) Written() bool {
	return w.written || w.ResponseWriter.Written()
}

func (w *wireCompatResponseWriter) Status() int { return w.status }

func (w *wireCompatResponseWriter) Size() int {
	if !w.Written() {
		return -1
	}
	return w.size
}

func (w *wireCompatResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *wireCompatResponseWriter) Write(data []byte) (n int, err error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if !w.decided {
		w.decide()
	}
	w.written = true
	defer func() {
		w.size += n
		if err != nil {
			w.writeErr = err
		}
	}()
	if w.passthrough {
		return w.ResponseWriter.Write(data)
	}
	if w.translator != nil {
		if err := w.translator.write(data); err != nil {
			return 0, err
		}
		if err := w.flushDownstream(); err != nil {
			return 0, err
		}
		return len(data), nil
	}
	if w.buffered.Len()+len(data) > wireCompatMaxBodyBytes {
		if err = w.passBufferedThrough(); err != nil {
			return 0, err
		}
		return w.ResponseWriter.Write(data)
	}
	return w.buffered.Write(data)
}

func (w *wireCompatResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// WriteHeaderNow commits headers immediately for pass-through responses;
// transformed responses commit on first downstream write or finish.
func (w *wireCompatResponseWriter) WriteHeaderNow() {
	if !w.decided {
		w.decide()
	}
	w.written = true
	if w.passthrough || w.translator != nil {
		w.ResponseWriter.WriteHeaderNow()
	}
}

func (w *wireCompatResponseWriter) Flush() {
	_ = w.FlushError()
}

// FlushError keeps disconnect errors observable by the server's streaming
// handlers instead of reporting a successful flush after the client leaves.
func (w *wireCompatResponseWriter) FlushError() error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if !w.decided {
		w.decide()
	}
	w.written = true
	if w.translator != nil {
		w.writeErr = w.flushDownstream()
	} else if !w.passthrough {
		// A handler explicitly flushing JSON requires streaming passthrough.
		w.writeErr = w.passBufferedThrough()
	}
	if w.writeErr == nil {
		w.writeErr = httpwire.FlushResponse(w.ResponseWriter)
	}
	return w.writeErr
}

func (w *wireCompatResponseWriter) passBufferedThrough() error {
	w.passthrough = true
	if w.buffered == nil {
		return nil
	}
	_, err := w.ResponseWriter.Write(w.buffered.Bytes())
	w.buffered = nil
	return err
}

// flushDownstream commits headers if needed and forwards everything the
// translator has completed so far.
func (w *wireCompatResponseWriter) flushDownstream() error {
	if w.translator == nil {
		return nil
	}
	out := w.translator.drain()
	if len(out) == 0 {
		return nil
	}
	if !w.ResponseWriter.Written() {
		w.ResponseWriter.WriteHeader(w.status)
	}
	_, err := w.ResponseWriter.Write(out)
	return err
}

// finish is called by the middleware after the handler returns. It writes the
// transformed JSON body or any trailing SSE bytes.
func (w *wireCompatResponseWriter) finish() {
	if w.writeErr != nil {
		return
	}
	if !w.decided {
		w.decide()
	}
	if w.passthrough {
		return
	}
	if w.translator != nil {
		w.translator.finish()
		_ = w.flushDownstream()
		return
	}
	body := w.buffered.Bytes()
	if transformed, changed := w.transformJSONBody(body); changed {
		body = transformed
	}
	if existing := w.Header().Get("Content-Length"); existing != "" {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	}
	if !w.ResponseWriter.Written() {
		w.ResponseWriter.WriteHeader(w.status)
	}
	if len(body) > 0 {
		_, _ = w.ResponseWriter.Write(body)
	}
}

// transformJSONBody restores flattened output items in a buffered JSON
// response. Bodies that are not JSON objects are returned unchanged.
func (w *wireCompatResponseWriter) transformJSONBody(body []byte) ([]byte, bool) {
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return body, false
	}
	changed := false
	switch w.mode {
	case wireCompatModeToolSearch:
		changed = restoreToolSearchOutput(root)
	case wireCompatModeOpenCode:
		restoreOpenCodeOutput(root, w.identities)
		changed = true
	}
	if !changed {
		return body, false
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return encoded, true
}

// wireCompatSSETranslator incrementally rewrites a Responses SSE stream. It
// consumes arbitrary byte chunks and emits whole transformed events; partial
// trailing events stay buffered until more data or finish arrives.
type wireCompatSSETranslator struct {
	mode       wireCompatMode
	identities openCodeIdentities
	pending    []byte
	out        bytes.Buffer
	converted  map[string]bool
	custom     map[string]bool
	finished   bool
}

func newWireCompatSSETranslator(mode wireCompatMode, identities openCodeIdentities) *wireCompatSSETranslator {
	return &wireCompatSSETranslator{
		mode:       mode,
		identities: identities,
		converted:  map[string]bool{},
		custom:     map[string]bool{},
	}
}

// write appends bytes and translates every complete event block.
func (t *wireCompatSSETranslator) write(p []byte) error {
	t.pending = append(t.pending, p...)
	for {
		end := sseEventEnd(t.pending)
		if end < 0 {
			return nil
		}
		if err := t.emitBlock(t.pending[:end]); err != nil {
			return err
		}
		t.pending = t.pending[end:]
	}
}

// drain returns translated output ready to write downstream.
func (t *wireCompatSSETranslator) drain() []byte {
	out := t.out.Bytes()
	t.out.Reset()
	return out
}

// finish flushes any trailing partial event after the handler completes.
func (t *wireCompatSSETranslator) finish() {
	if t.finished {
		return
	}
	t.finished = true
	if len(t.pending) == 0 {
		return
	}
	_ = t.emitBlock(t.pending)
	t.pending = nil
}

// sseEventEnd returns the byte length of the first complete SSE event in buf,
// including its terminating blank line, or -1 when the event is incomplete.
func sseEventEnd(buf []byte) int {
	for i := 0; i < len(buf); i++ {
		if buf[i] != '\n' {
			continue
		}
		// "\n\n" or "\r\n\r\n"/"\n\r\n" style blank-line terminator.
		if i+1 < len(buf) && buf[i+1] == '\n' {
			return i + 2
		}
		if i+2 < len(buf) && buf[i+1] == '\r' && buf[i+2] == '\n' {
			return i + 3
		}
	}
	return -1
}

// emitBlock transforms one complete SSE event block (lines plus terminating
// blank line) and queues the result for downstream writing.
func (t *wireCompatSSETranslator) emitBlock(block []byte) error {
	text := strings.TrimRight(string(block), "\r\n")
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}

	if t.mode == wireCompatModeOpenCode {
		return t.emitOpenCode(lines)
	}
	return t.emitToolSearch(lines)
}

func (t *wireCompatSSETranslator) emitRaw(lines []string) {
	if len(lines) == 0 {
		t.out.WriteString("\n")
		return
	}
	t.out.WriteString(strings.Join(lines, "\n"))
	t.out.WriteString("\n\n")
}

// eventData joins the data: payloads of an event block.
func sseEventData(lines []string) string {
	var data []string
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return strings.Join(data, "\n")
}

// writeEvent serializes a transformed event, preserving non-data lines and
// rewriting the event: line when the event type changed.
func (t *wireCompatSSETranslator) writeEvent(lines []string, event map[string]any) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	kind := wireCompatStringValue(event["type"])
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			continue
		}
		if strings.HasPrefix(line, "event:") && kind != "" {
			line = "event: " + kind
		}
		t.out.WriteString(line)
		t.out.WriteByte('\n')
	}
	_, err = fmt.Fprintf(&t.out, "data: %s\n\n", encoded)
	return err
}

// emitToolSearch restores tool_search calls in a Responses event stream.
// Argument delta/done events of a converted call are dropped: a
// tool_search_call carries its complete arguments object on
// response.output_item.done, and Codex has no use for partial JSON. Events
// that need no change are forwarded byte for byte.
func (t *wireCompatSSETranslator) emitToolSearch(lines []string) error {
	if len(lines) == 0 {
		t.emitRaw(lines)
		return nil
	}
	joined := sseEventData(lines)
	if joined == "" || !strings.Contains(joined, "function_call") {
		t.emitRaw(lines)
		return nil
	}
	var event map[string]any
	if json.Unmarshal([]byte(joined), &event) != nil {
		t.emitRaw(lines)
		return nil
	}
	kind := wireCompatStringValue(event["type"])
	changed := false
	switch kind {
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		if t.converted[wireCompatStringValue(event["item_id"])] {
			return nil
		}
	case "response.output_item.added", "response.output_item.done":
		if item, ok := event["item"].(map[string]any); ok {
			id := wireCompatStringValue(item["id"])
			if restoreToolSearchCall(item) {
				if id != "" {
					t.converted[id] = true
				}
				changed = true
			}
		}
	}
	if response, ok := event["response"].(map[string]any); ok && restoreToolSearchOutput(response) {
		changed = true
	}
	if !changed {
		t.emitRaw(lines)
		return nil
	}
	return t.writeEvent(lines, event)
}

// emitOpenCode restores flattened tool identities in a Responses event stream
// for OpenCode Go routes.
func (t *wireCompatSSETranslator) emitOpenCode(lines []string) error {
	if len(lines) == 0 {
		t.emitRaw(lines)
		return nil
	}
	joined := sseEventData(lines)
	var event map[string]any
	if joined == "" || json.Unmarshal([]byte(joined), &event) != nil {
		t.emitRaw(lines)
		return nil
	}
	kind := wireCompatStringValue(event["type"])
	if kind == "response.function_call_arguments.done" {
		if id, ok := t.identities[wireCompatStringValue(event["name"])]; ok {
			event["name"] = id.Name
			if id.Namespace != "" {
				event["namespace"] = id.Namespace
			}
		}
	}
	if item, ok := event["item"].(map[string]any); ok {
		if id, found := restoreOpenCodeItem(item, t.identities); found && id.Kind == "custom" {
			t.custom[wireCompatStringValue(item["id"])] = true
		}
	}
	if response, ok := event["response"].(map[string]any); ok {
		restoreOpenCodeOutput(response, t.identities)
	}
	restoreOpenCodeOutput(event, t.identities)
	if strings.HasPrefix(kind, "response.function_call_arguments.") && t.custom[wireCompatStringValue(event["item_id"])] {
		// Function arguments contain a JSON wrapper, not freeform text. The complete
		// input is emitted on done, avoiding invalid partial JSON in custom deltas.
		if strings.HasSuffix(kind, ".delta") {
			return nil
		}
		if strings.HasSuffix(kind, ".done") {
			var wrapper struct {
				Input string `json:"input"`
			}
			args := wireCompatStringValue(event["arguments"])
			if err := json.Unmarshal([]byte(args), &wrapper); err != nil {
				wrapper.Input = args
			}
			event["type"] = "response.custom_tool_call_input.done"
			event["input"] = wrapper.Input
			delete(event, "arguments")
		}
	}
	return t.writeEvent(lines, event)
}

// wireCompatSSEReadCloser adapts the streaming translator to an io.Reader for
// tests that drive it through a bufio reader.
func readWireCompatSSE(source io.Reader, mode wireCompatMode, identities openCodeIdentities) ([]byte, error) {
	t := newWireCompatSSETranslator(mode, identities)
	reader := bufio.NewReader(source)
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			if werr := t.write(buf[:n]); werr != nil {
				return nil, werr
			}
			out.Write(t.drain())
		}
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			t.finish()
			out.Write(t.drain())
			return out.Bytes(), nil
		}
	}
}
