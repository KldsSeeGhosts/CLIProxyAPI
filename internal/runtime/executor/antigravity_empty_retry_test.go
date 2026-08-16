package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestAntigravityResponseHasContent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"empty input", "", false},
		{"wrapped text", `{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`, true},
		{"bare text", `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`, true},
		{"thought text", `{"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"hmm"}]}}]}}`, true},
		{"function call", `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"ls","args":{}}}]}}]}}`, true},
		{"inline data", `{"response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png"}}]}}]}}`, true},
		{"no candidates", `{"response":{"candidates":[],"usageMetadata":{"totalTokenCount":10}}}`, false},
		{"candidate no parts", `{"response":{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"totalTokenCount":10}}}`, false},
		{"empty text part", `{"response":{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}]}}`, false},
		{"blocked prompt", `{"response":{"promptFeedback":{"blockReason":"SAFETY"}}}`, true},
		{"safety finish reason", `{"response":{"candidates":[{"finishReason":"SAFETY"}]}}`, true},
		{"invalid json", `{"response":`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := antigravityResponseHasContent([]byte(tc.body)); got != tc.want {
				t.Fatalf("antigravityResponseHasContent() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPeekAntigravityStreamContent(t *testing.T) {
	t.Run("content after metadata lines", func(t *testing.T) {
		stream := "data: {\"response\":{\"responseId\":\"1\"}}\n\n" +
			"data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}}\n\n"
		buffered, hasContent, err := peekAntigravityStreamContent(context.Background(), strings.NewReader(stream))
		if err != nil {
			t.Fatalf("peek error: %v", err)
		}
		if !hasContent {
			t.Fatal("expected content to be detected")
		}
		if !strings.Contains(string(buffered), "responseId") {
			t.Fatal("expected buffered bytes to replay earlier lines")
		}
	})
	t.Run("empty stream", func(t *testing.T) {
		stream := "data: {\"response\":{\"candidates\":[],\"usageMetadata\":{\"totalTokenCount\":5}}}\n\n"
		_, hasContent, err := peekAntigravityStreamContent(context.Background(), strings.NewReader(stream))
		if err != nil {
			t.Fatalf("peek error: %v", err)
		}
		if hasContent {
			t.Fatal("expected empty stream to be detected")
		}
	})
}

func antigravityEmptyRetryTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "antigravity-empty-retry-auth",
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": baseURL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
}

const antigravityEmptyRetryGeminiPayload = `{"model":"gemini-3.5-flash-low","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

func TestAntigravityExecutorExecuteStreamRetriesEmptyStream(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			// Transient empty completion: zero candidates, then EOF.
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":3,\"totalTokenCount\":3}}}\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1,\"totalTokenCount\":4}}}\n\n"))
	}))
	defer server.Close()

	// RequestRetry: 1 proves empty retries do not depend on the configured budget.
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	result, err := exec.ExecuteStream(context.Background(), antigravityEmptyRetryTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.5-flash-low",
		Payload: []byte(antigravityEmptyRetryGeminiPayload),
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatGemini,
		ResponseFormat:  sdktranslator.FormatGemini,
		Stream:          true,
		OriginalRequest: []byte(antigravityEmptyRetryGeminiPayload),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var sawText bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "ok") {
			sawText = true
		}
	}
	if !sawText {
		t.Fatal("expected retried content to reach the client")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

func TestAntigravityExecutorExecuteStreamEmptyStreamExhaustion(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":3,\"totalTokenCount\":3}}}\n\n"))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	_, err := exec.ExecuteStream(context.Background(), antigravityEmptyRetryTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.5-flash-low",
		Payload: []byte(antigravityEmptyRetryGeminiPayload),
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatGemini,
		ResponseFormat:  sdktranslator.FormatGemini,
		Stream:          true,
		OriginalRequest: []byte(antigravityEmptyRetryGeminiPayload),
	})
	if err == nil {
		t.Fatal("expected an error after exhausting empty retries")
	}
	sErr, ok := err.(statusErr)
	if !ok {
		t.Fatalf("error type = %T, want statusErr (%v)", err, err)
	}
	if sErr.code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", sErr.code)
	}
	wantCalls := int32(1 + antigravityEmptyResponseMaxRetries)
	if got := calls.Load(); got != wantCalls {
		t.Fatalf("upstream calls = %d, want %d", got, wantCalls)
	}
}

func TestAntigravitySemanticContinuationPayload(t *testing.T) {
	const model = "gemini-3.6-flash-high"
	payload := antigravitySemanticContinuationPayload(model)
	if !antigravityResponseHasContent(payload) {
		t.Fatalf("semantic continuation payload should contain reasoning: %s", payload)
	}
	if !strings.Contains(string(payload), model) || !strings.Contains(string(payload), "continue the unfinished turn") {
		t.Fatalf("unexpected semantic continuation payload: %s", payload)
	}
	stream := antigravitySemanticContinuationStream(model)
	if !strings.HasPrefix(string(stream), "data: ") || !strings.HasSuffix(string(stream), "\n\n") {
		t.Fatalf("invalid semantic continuation SSE framing: %q", stream)
	}
}

func TestAntigravityExecutorGemini36ConvertsEmptyStreamToSemanticContinuation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":3,\"totalTokenCount\":3}}}\n\n"))
	}))
	defer server.Close()

	payload := []byte(`{"model":"gemini-3.6-flash-high","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	result, err := exec.ExecuteStream(context.Background(), antigravityEmptyRetryTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.6-flash-high",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatGemini,
		ResponseFormat:  sdktranslator.FormatGemini,
		Stream:          true,
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var output strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !strings.Contains(output.String(), "continue the unfinished turn") {
		t.Fatalf("expected semantic continuation output, got %s", output.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no blind replay)", got)
	}
}

func TestAntigravityExecutorGemini36ConvertsEmptyCompletionToSemanticContinuation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"candidates":[],"usageMetadata":{"promptTokenCount":3,"totalTokenCount":3}}}`))
	}))
	defer server.Close()

	payload := []byte(`{"model":"gemini-3.6-flash-high","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	resp, err := exec.Execute(context.Background(), antigravityEmptyRetryTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.6-flash-high",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatGemini,
		ResponseFormat:  sdktranslator.FormatGemini,
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(string(resp.Payload), "continue the unfinished turn") {
		t.Fatalf("expected semantic continuation response, got %s", resp.Payload)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no blind replay)", got)
	}
}

func TestAntigravityExecutorExecuteRetriesEmptyCompletion(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"response":{"candidates":[],"usageMetadata":{"promptTokenCount":3,"totalTokenCount":3}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	resp, err := exec.Execute(context.Background(), antigravityEmptyRetryTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.5-flash-low",
		Payload: []byte(antigravityEmptyRetryGeminiPayload),
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatGemini,
		ResponseFormat:  sdktranslator.FormatGemini,
		OriginalRequest: []byte(antigravityEmptyRetryGeminiPayload),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(string(resp.Payload), "ok") {
		t.Fatalf("expected retried content in response, got %s", string(resp.Payload))
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}
