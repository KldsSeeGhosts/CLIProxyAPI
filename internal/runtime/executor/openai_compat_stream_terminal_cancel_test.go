package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// newOpenAICompatCancelTestExecutor builds a Responses-source ("codex") request
// against the given upstream so the executor translates chat-completion frames
// into SSE Responses events and emits a response.completed / response.incomplete
// terminal chunk, mirroring the real Qwen accounting false-positive.
func newOpenAICompatCancelTestExecutor(t *testing.T, serverURL, model string) (*OpenAICompatExecutor, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	t.Helper()
	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": serverURL + "/v1",
		"api_key":  "test",
	}}
	req := cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"model":"` + model + `","input":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	}
	return executor, auth, req, opts
}

func terminalChunkType(t *testing.T, chunk []byte) string {
	t.Helper()
	lines := strings.Split(string(chunk), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if gjson.Valid(payload) {
				return gjson.Get(payload, "type").String()
			}
		}
	}
	return ""
}

// A codex client consuming the terminal Responses event closes the downstream
// connection immediately while the upstream socket is still closing; the scan
// then fails with context.Canceled. That must not be recorded as a failure.
func TestOpenAICompatExecutorStreamCancelAfterTerminalCompletedNoFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n"))
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":7,"total_tokens":11}}` + "\n"))
		_, _ = w.Write([]byte(`data: [DONE]` + "\n"))
		w.(http.Flusher).Flush()
		// Keep the socket open. The offset client consumed the terminal event
		// and closed downstream; only context cancellation tears this up.
		<-r.Context().Done()
	}))
	defer server.Close()

	const model = "qwen-cancel-completed"
	plugin := &captureOpenAICompatUsagePlugin{provider: "openai-compatibility", model: model, records: make(chan usage.Record, 2)}
	usage.RegisterPlugin(plugin)

	executor, auth, req, opts := newOpenAICompatCancelTestExecutor(t, server.URL, model)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	terminalDelivered := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error chunk: %v", chunk.Err)
		}
		if openAICompatResponsesTerminalChunk(chunk.Payload) {
			terminalDelivered = true
			if got := terminalChunkType(t, chunk.Payload); got != "response.completed" {
				t.Fatalf("terminal chunk type = %q, want response.completed", got)
			}
			// Downstream consumed the terminal event and closed; cancel the
			// context so the upstream scan ends with context.Canceled.
			cancel()
		}
	}
	if !terminalDelivered {
		t.Fatal("terminal response.completed chunk was never delivered")
	}

	record := waitForOpenAICompatUsageRecord(t, plugin.records)
	if record.Failed {
		t.Fatalf("published failure after terminal event: fail=%+v", record.Fail)
	}
	if record.Detail.OutputTokens != 7 || record.Detail.InputTokens != 4 || record.Detail.TotalTokens != 11 {
		t.Fatalf("usage not preserved after clean cancel: %+v", record.Detail)
	}
	assertNoAdditionalOpenAICompatUsageRecord(t, plugin.records)
}

// Cancellation before any terminal Responses event is a genuine mid-stream
// failure and must keep publishing the failure record.
func TestOpenAICompatExecutorStreamCancelBeforeTerminalStillFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		// No data lines: the terminal event never gets emitted before cancel.
		<-r.Context().Done()
	}))
	defer server.Close()

	const model = "qwen-cancel-before-terminal"
	plugin := &captureOpenAICompatUsagePlugin{provider: "openai-compatibility", model: model, records: make(chan usage.Record, 2)}
	usage.RegisterPlugin(plugin)

	executor, auth, req, opts := newOpenAICompatCancelTestExecutor(t, server.URL, model)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	cancel()
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			break
		}
	}

	record := waitForOpenAICompatUsageRecord(t, plugin.records)
	if !record.Failed {
		t.Fatalf("cancellation before terminal event was not published as failure: %+v", record)
	}
	assertNoAdditionalOpenAICompatUsageRecord(t, plugin.records)
}

// A length-truncated turn ends with response.incomplete; the terminal-event
// suppression must treat it identically to response.completed.
func TestOpenAICompatExecutorStreamCancelAfterTerminalIncompleteNoFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"running out"},"finish_reason":"length"}]}` + "\n"))
		_, _ = w.Write([]byte(`data: [DONE]` + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	const model = "qwen-cancel-incomplete"
	plugin := &captureOpenAICompatUsagePlugin{provider: "openai-compatibility", model: model, records: make(chan usage.Record, 2)}
	usage.RegisterPlugin(plugin)

	executor, auth, req, opts := newOpenAICompatCancelTestExecutor(t, server.URL, model)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	terminalDelivered := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error chunk: %v", chunk.Err)
		}
		if openAICompatResponsesTerminalChunk(chunk.Payload) {
			terminalDelivered = true
			if got := terminalChunkType(t, chunk.Payload); got != "response.incomplete" {
				t.Fatalf("terminal chunk type = %q, want response.incomplete", got)
			}
			cancel()
		}
	}
	if !terminalDelivered {
		t.Fatal("terminal response.incomplete chunk was never delivered")
	}

	record := waitForOpenAICompatUsageRecord(t, plugin.records)
	if record.Failed {
		t.Fatalf("published failure after response.incomplete terminal: fail=%+v", record.Fail)
	}
	assertNoAdditionalOpenAICompatUsageRecord(t, plugin.records)
}

// An upstream data: {"error":{...}} envelope is a real failure regardless of
// any prior content and must keep publishing the failure record.
func TestOpenAICompatExecutorStreamDataLineErrorStillFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n"))
		_, _ = w.Write([]byte(`data: {"error":{"message":"upstream exploded","type":"server_error","code":429}}` + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	const model = "qwen-data-error"
	plugin := &captureOpenAICompatUsagePlugin{provider: "openai-compatibility", model: model, records: make(chan usage.Record, 2)}
	usage.RegisterPlugin(plugin)

	executor, auth, req, opts := newOpenAICompatCancelTestExecutor(t, server.URL, model)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var gotErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
		}
	}
	if gotErr == nil {
		t.Fatal("expected terminal stream error for data: error envelope")
	}
	if status, ok := gotErr.(interface{ StatusCode() int }); !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("stream error status = %v, want %d", gotErr, http.StatusTooManyRequests)
	}

	record := waitForOpenAICompatUsageRecord(t, plugin.records)
	if !record.Failed {
		t.Fatalf("data:error envelope was not published as failure: %+v", record)
	}
	assertNoAdditionalOpenAICompatUsageRecord(t, plugin.records)
}

// The terminal-chunk detector parses the framed SSE output; the event name
// appearing inside payload text or on an unrelated event line must never
// count as a terminal chunk.
func TestOpenAICompatResponsesTerminalChunk(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
		want  bool
	}{
		{name: "completed terminal", chunk: "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}", want: true},
		{name: "incomplete terminal", chunk: "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"sequence_number\":5,\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}", want: true},
		{name: "delta chunk", chunk: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}", want: false},
		{name: "created chunk", chunk: "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}", want: false},
		{name: "event name inside output text", chunk: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"response.completed is not emitted here\"}", want: false},
		{name: "event name in nested content", chunk: "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output\":[{\"type\":\"message\",\"content\":[{\"text\":\"response.incomplete\"}]}]}", want: false},
		{name: "mismatched event line", chunk: "event: response.completed\ndata: {\"type\":\"response.created\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}", want: false},
		{name: "non-json data", chunk: "event: response.completed\ndata: [DONE]", want: false},
		{name: "data missing", chunk: "event: response.completed", want: false},
		{name: "empty", chunk: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := openAICompatResponsesTerminalChunk([]byte(tt.chunk)); got != tt.want {
				t.Fatalf("openAICompatResponsesTerminalChunk(%q) = %v, want %v", tt.chunk, got, tt.want)
			}
		})
	}
}

type captureOpenAICompatUsagePlugin struct {
	provider string
	model    string
	records  chan usage.Record
}

func (p *captureOpenAICompatUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	if p == nil || record.Provider != p.provider || record.Model != p.model {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

func waitForOpenAICompatUsageRecord(t *testing.T, records <-chan usage.Record) usage.Record {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for openai-compat usage record")
		return usage.Record{}
	}
}

func assertNoAdditionalOpenAICompatUsageRecord(t *testing.T, records <-chan usage.Record) {
	t.Helper()
	select {
	case record := <-records:
		t.Fatalf("received additional openai-compat usage record: %+v", record)
	case <-time.After(100 * time.Millisecond):
	}
}