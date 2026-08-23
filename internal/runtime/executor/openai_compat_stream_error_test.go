package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorStreamSurfacesDataLineError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n"))
		_, _ = w.Write([]byte(`data: {"error":{"message":"upstream exploded","type":"server_error","code":"rate_limit_exceeded"}}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openrouter-model",
		Payload: []byte(`{"model":"openrouter-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var payloads [][]byte
	var gotErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
			continue
		}
		payloads = append(payloads, chunk.Payload)
	}
	if gotErr == nil {
		t.Fatal("expected terminal stream error for data: error envelope")
	}
	if status, ok := gotErr.(interface{ StatusCode() int }); !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("stream error status = %v, want %d", gotErr, http.StatusTooManyRequests)
	}
	if !strings.Contains(gotErr.Error(), "upstream exploded") {
		t.Fatalf("stream error = %v, want upstream message", gotErr)
	}
	if len(payloads) != 1 || gjson.GetBytes(payloads[0], "choices.0.delta.content").String() != "hi" {
		t.Fatalf("chunks before the error were not preserved: %d payloads", len(payloads))
	}
}

func TestOpenAICompatExecutorStreamIgnoresErrorNullAndKeepsDONE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n"))
		_, _ = w.Write([]byte(`data: {"error":null}` + "\n"))
		_, _ = w.Write([]byte(`data: [DONE]` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openrouter-model",
		Payload: []byte(`{"model":"openrouter-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var payloads [][]byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		payloads = append(payloads, chunk.Payload)
	}
	joined := ""
	for _, payload := range payloads {
		joined += string(payload)
	}
	if !strings.Contains(joined, "hello") {
		t.Fatalf("normal choices chunk was not passed through: %s", joined)
	}
	if !strings.Contains(joined, `"error":null`) {
		t.Fatalf("error:null chunk was not passed through: %s", joined)
	}
	if strings.Contains(joined, "[DONE]") {
		t.Fatalf("[DONE] sentinel must keep flowing to the translator, not the client: %s", joined)
	}
}

func TestOpenAICompatExecutorSynthesizesMuseSparkFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}` + "\n"))
		_, _ = w.Write([]byte(`data: [DONE]` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "opencode-go/muse-spark-1.2-contributor",
		Payload: []byte(`{"model":"opencode-go/muse-spark-1.2-contributor","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var joined string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		joined += string(chunk.Payload)
	}
	if !strings.Contains(joined, `"content":"OK"`) {
		t.Fatalf("content chunk missing: %s", joined)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Fatalf("synthetic finish reason missing: %s", joined)
	}
}

func TestOpenAICompatExecutorSynthesizesMuseSparkResponsesCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "opencode-go/muse-spark-1.2-contributor",
		Payload: []byte(`{"model":"opencode-go/muse-spark-1.2-contributor","input":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var joined string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		joined += string(chunk.Payload)
	}
	if !strings.Contains(joined, `"type":"response.completed"`) {
		t.Fatalf("response.completed missing: %s", joined)
	}
}

func TestOpenAICompatExecutorSynthesizesMuseSparkCodexEOFWithoutPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		Codex: config.CodexConfig{
			SemanticContinuation: config.CodexSemanticContinuationConfig{
				Enabled:          true,
				Models:           []string{"muse-spark-*", "opencode-go/*"},
				MaxContinuations: 1,
			},
		},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "muse-spark-1.2-contributor",
		Payload: []byte(`{"model":"muse-spark-1.2-contributor","input":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"exec_command","description":"run a command","parameters":{"type":"object","properties":{}}}],"stream":true}`),
	}, cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers:        http.Header{"User-Agent": []string{"Codex Desktop/26.7"}},
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "muse-spark-1.2-contributor",
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var joined string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		joined += string(chunk.Payload)
	}
	if !strings.Contains(joined, `"type":"response.completed"`) {
		t.Fatalf("response.completed missing: %s", joined)
	}
}

func TestOpenAICompatExecutorSynthesizesMuseSparkCodexToolCallEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},"finish_reason":null}]}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		Codex: config.CodexConfig{
			SemanticContinuation: config.CodexSemanticContinuationConfig{
				Enabled:          true,
				Models:           []string{"muse-spark-*", "opencode-go/*"},
				MaxContinuations: 1,
			},
		},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "muse-spark-1.2-contributor",
		Payload: []byte(`{"model":"muse-spark-1.2-contributor","input":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"exec_command","description":"run a command","parameters":{"type":"object","properties":{}}}],"stream":true}`),
	}, cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers:        http.Header{"User-Agent": []string{"Codex Desktop/26.7"}},
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "muse-spark-1.2-contributor",
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var joined string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		joined += string(chunk.Payload)
	}
	if !strings.Contains(joined, `"type":"response.completed"`) {
		t.Fatalf("response.completed missing: %s", joined)
	}
	if !strings.Contains(joined, `"name":"exec_command"`) {
		t.Fatalf("tool call missing: %s", joined)
	}
}

func TestOpenAICompatExecutorStreamSurfacesNumericCodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"error":{"message":"rate limited","code":429}}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openrouter-model",
		Payload: []byte(`{"model":"openrouter-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
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
		t.Fatal("expected terminal stream error for numeric code envelope")
	}
	if status, ok := gotErr.(interface{ StatusCode() int }); !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("stream error status = %v, want %d", gotErr, http.StatusTooManyRequests)
	}
}

func TestOpenAICompatStreamErrorPayload(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "error object", body: `{"error":{"message":"boom"}}`, want: true},
		{name: "error with numeric code", body: `{"error":{"message":"boom","code":429}}`, want: true},
		{name: "error with symbolic code", body: `{"error":{"message":"boom","code":"rate_limit_exceeded"}}`, want: true},
		{name: "error null", body: `{"error":null}`, want: false},
		{name: "error missing", body: `{"id":"chatcmpl_1"}`, want: false},
		{name: "error string", body: `{"error":"boom"}`, want: false},
		{name: "normal chunk", body: `{"id":"chatcmpl_1","choices":[{"delta":{"content":"hi"}}]}`, want: false},
		{name: "chunk with choices wins over error", body: `{"choices":[],"error":{"message":"boom"}}`, want: false},
		{name: "done sentinel", body: `[DONE]`, want: false},
		{name: "not json", body: `: keep-alive`, want: false},
		{name: "empty", body: ``, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := openAICompatStreamErrorPayload([]byte(tt.body)); got != tt.want {
				t.Fatalf("openAICompatStreamErrorPayload(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestOpenAICompatStreamErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "numeric code", body: `{"error":{"code":429,"message":"x"}}`, want: http.StatusTooManyRequests},
		{name: "numeric status", body: `{"error":{"status":503,"message":"x"}}`, want: http.StatusServiceUnavailable},
		{name: "numeric status_code", body: `{"error":{"status_code":400,"message":"x"}}`, want: http.StatusBadRequest},
		{name: "rate limit symbolic", body: `{"error":{"code":"rate_limit_exceeded","message":"x"}}`, want: http.StatusTooManyRequests},
		{name: "invalid request type", body: `{"error":{"type":"invalid_request_error","message":"x"}}`, want: http.StatusBadRequest},
		{name: "auth symbolic", body: `{"error":{"code":"invalid_api_key","message":"x"}}`, want: http.StatusUnauthorized},
		{name: "not found symbolic", body: `{"error":{"code":"model_not_found","message":"x"}}`, want: http.StatusNotFound},
		{name: "no status hint", body: `{"error":{"message":"boom"}}`, want: http.StatusBadGateway},
		{name: "out of range code", body: `{"error":{"code":999,"message":"x"}}`, want: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := openAICompatStreamErrorStatus([]byte(tt.body)); got != tt.want {
				t.Fatalf("openAICompatStreamErrorStatus(%q) = %d, want %d", tt.body, got, tt.want)
			}
		})
	}
}

func TestOpenAICompatExecutorStreamSanitizesResponsesReplayOnOrdinaryPath(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	// Codex replay of the reasoning item the gateway synthesized for a Qwen
	// turn: an empty encrypted_content with no GPT reasoning signature.
	payload := []byte(`{"model":"qwen3.8-max-preview","input":[` +
		`{"role":"user","content":"hi"},` +
		`{"id":"rs_chatcmpl-abc_0","type":"reasoning","encrypted_content":"","summary":[{"type":"summary_text","text":"thinking..."}]}` +
		`]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen3.8-max-preview",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
	}

	upstream := string(gotBody)
	if strings.Contains(upstream, "encrypted_content") {
		t.Fatalf("empty encrypted_content reached upstream: %s", upstream)
	}
	if strings.Contains(upstream, "rs_chatcmpl-abc_0") {
		t.Fatalf("orphan reasoning id reached upstream: %s", upstream)
	}
	if !strings.Contains(upstream, `"content":"hi"`) {
		t.Fatalf("user message missing from upstream body: %s", upstream)
	}
}

func TestOpenAICompatNormalizeChatCompletionPayloadConvertsFullCompletion(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl_fpa0o77yva5","object":"chat.completion","created":1787201300,"model":"muse-spark-1.2-contributor","choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":null}]}`)
	got, ok := openAICompatNormalizeChatCompletionPayload(raw)
	if !ok {
		t.Fatal("expected full completion to normalize")
	}
	if gjson.GetBytes(got, "object").String() != "chat.completion.chunk" {
		t.Fatalf("object = %s", got)
	}
	if gjson.GetBytes(got, "choices.0.message").Exists() {
		t.Fatalf("message should be rewritten as delta: %s", got)
	}
	if gjson.GetBytes(got, "choices.0.delta.role").String() != "assistant" {
		t.Fatalf("delta role missing: %s", got)
	}
	if gjson.GetBytes(got, "choices.0.finish_reason").Type != gjson.Null {
		t.Fatalf("empty completion should keep null finish_reason: %s", got)
	}
}

func TestOpenAICompatExecutorAcceptsBareMuseSparkChatCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_fpa0o77yva5","object":"chat.completion","created":1787201300,"model":"muse-spark-1.2-contributor","choices":[{"index":0,"message":{"role":"assistant","content":"ready"},"finish_reason":null}]}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "muse-spark-1.2-contributor",
		Payload: []byte(`{"model":"muse-spark-1.2-contributor","input":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var joined string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		joined += string(chunk.Payload)
	}
	if !strings.Contains(joined, `"ready"`) {
		t.Fatalf("content missing: %s", joined)
	}
	if !strings.Contains(joined, `"type":"response.completed"`) {
		t.Fatalf("response.completed missing: %s", joined)
	}
}

func TestOpenAICompatExecutorStillFailsBareErrorJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"upstream exploded","type":"server_error"}}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openrouter-model",
		Payload: []byte(`{"model":"openrouter-model","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
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
		t.Fatal("expected stream error for bare error JSON")
	}
	if !strings.Contains(gotErr.Error(), "upstream exploded") {
		t.Fatalf("stream error = %v", gotErr)
	}
}

func TestOpenAICompatExecutorAcceptsPrettyPrintedMuseSparkCompletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "chatcmpl_pretty",
  "object": "chat.completion",
  "created": 1787202304,
  "model": "muse-spark-1.2-contributor",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "ready"}, "finish_reason": null}]
}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "muse-spark-1.2-contributor",
		Payload: []byte(`{"model":"muse-spark-1.2-contributor","input":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var joined string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		joined += string(chunk.Payload)
	}
	if !strings.Contains(joined, `"ready"`) {
		t.Fatalf("content missing: %s", joined)
	}
	if !strings.Contains(joined, `"type":"response.completed"`) {
		t.Fatalf("response.completed missing: %s", joined)
	}
}

func TestOpenAICompatExecutorRetriesEmptyMuseSparkCompletion(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := calls.Add(1)
		if n == 1 {
			_, _ = w.Write([]byte(`{"id":"chatcmpl_6s2omd9k2n2","object":"chat.completion","created":1787202304,"model":"muse-spark-1.2-contributor","choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":null}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"chatcmpl_retry","object":"chat.completion","created":1787202305,"model":"muse-spark-1.2-contributor","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":null}]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "muse-spark-1.2-contributor",
		Payload: []byte(`{"model":"muse-spark-1.2-contributor","input":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var joined string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		joined += string(chunk.Payload)
	}
	if calls.Load() < 2 {
		t.Fatalf("calls = %d, want a retry", calls.Load())
	}
	if !strings.Contains(joined, `"hello"`) {
		t.Fatalf("retried content missing: %s", joined)
	}
}

func TestOpenAICompatExecutorFailsEmptyMuseSparkCompletionAfterRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_6s2omd9k2n2","object":"chat.completion","created":1787202304,"model":"muse-spark-1.2-contributor","choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":null}]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "muse-spark-1.2-contributor",
		Payload: []byte(`{"model":"muse-spark-1.2-contributor","input":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	})
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
		t.Fatal("expected empty-completion error after retries")
	}
	if strings.Contains(gotErr.Error(), "chatcmpl_6s2omd9k2n2") {
		t.Fatalf("raw empty completion leaked as error: %v", gotErr)
	}
	if !strings.Contains(gotErr.Error(), "empty completion") {
		t.Fatalf("stream error = %v", gotErr)
	}
}

func TestOpenAICompatRemoveMuseSparkOutputCap(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		payload string
		wantCap bool
	}{
		{
			name:    "removes Muse max tokens",
			model:   "opencode-go/muse-spark-1.2-contributor",
			payload: `{"model":"muse-spark-1.2-contributor","max_tokens":64,"reasoning_effort":"high"}`,
			wantCap: false,
		},
		{
			name:    "removes Muse max completion tokens",
			model:   "muse-spark-1.2",
			payload: `{"max_completion_tokens":64}`,
			wantCap: false,
		},
		{
			name:    "leaves other models alone",
			model:   "deepseek-v4-flash",
			payload: `{"max_tokens":64}`,
			wantCap: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := openAICompatRemoveMuseSparkOutputCap([]byte(tt.payload), tt.model)
			hasCap := gjson.GetBytes(got, "max_tokens").Exists() || gjson.GetBytes(got, "max_completion_tokens").Exists() || gjson.GetBytes(got, "max_output_tokens").Exists()
			if hasCap != tt.wantCap {
				t.Fatalf("has output cap = %v, want %v; payload=%s", hasCap, tt.wantCap, got)
			}
			if effort := gjson.GetBytes(got, "reasoning_effort").String(); tt.model == "opencode-go/muse-spark-1.2-contributor" && effort != "high" {
				t.Fatalf("reasoning_effort = %q, want preserved high; payload=%s", effort, got)
			}
		})
	}
}

func TestOpenAICompatExecutorRewritesAgentMessageForResponsesUpstream(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}` + "\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "opencode-go-codex",
			Models: []config.OpenAICompatibilityModel{{
				Name:        "muse-spark-1.2-contributor",
				Alias:       "codex/muse-spark-1.2-contributor",
				UpstreamAPI: "responses",
			}},
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "opencode-go-codex",
		Attributes: map[string]string{
			"base_url":    server.URL + "/v1",
			"api_key":     "test",
			"compat_name": "opencode-go-codex",
		},
	}
	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "codex/muse-spark-1.2-contributor",
		Payload: []byte(`{"model":"codex/muse-spark-1.2-contributor","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Payload:\n"},{"type":"encrypted_content","encrypted_content":"delegated task"}]}],"stream_options":{"include_usage":true}}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers:        http.Header{"User-Agent": []string{"Codex Desktop/0.147.0"}},
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/responses") {
		t.Fatalf("path = %s, want /responses", gotPath)
	}
	if gjson.GetBytes(gotBody, "input.1.type").String() != "message" {
		t.Fatalf("input.1.type = %s, want message; body=%s", gjson.GetBytes(gotBody, "input.1.type").String(), gotBody)
	}
	if gjson.GetBytes(gotBody, "input.1.role").String() != "user" {
		t.Fatalf("input.1.role = %s, want user; body=%s", gjson.GetBytes(gotBody, "input.1.role").String(), gotBody)
	}
	if gjson.GetBytes(gotBody, "input.1.content.1.type").String() != "input_text" || gjson.GetBytes(gotBody, "input.1.content.1.text").String() != "delegated task" {
		t.Fatalf("encrypted_content was not flattened: %s", gotBody)
	}
	if gjson.GetBytes(gotBody, "stream_options").Exists() {
		t.Fatalf("stream_options leaked to Responses upstream: %s", gotBody)
	}
}
