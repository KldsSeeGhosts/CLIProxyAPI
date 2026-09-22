package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutor_OpenCodeSessionInjection(t *testing.T) {
	var capturedSessionHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSessionHeader = r.Header.Get("x-opencode-session")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":123,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "opencode-go",
			BaseURL: server.URL + "/zen/go/v1",
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "openai-compatibility:opencode-go",
		Attributes: map[string]string{
			"base_url":     server.URL + "/zen/go/v1",
			"api_key":      "sk-test",
			"compat_name":  "opencode-go",
			"provider_key": "openai-compatibility:opencode-go",
		},
	}

	t.Run("auto-injects session when missing from client", func(t *testing.T) {
		capturedSessionHeader = ""
		req := cliproxyexecutor.Request{
			Model:   "deepseek-v4-flash",
			Payload: []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`),
		}
		opts := cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAI,
		}

		_, err := executor.Execute(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if capturedSessionHeader == "" {
			t.Fatal("expected x-opencode-session to be injected, got empty")
		}
	})

	t.Run("forwards client explicit session header", func(t *testing.T) {
		capturedSessionHeader = ""
		req := cliproxyexecutor.Request{
			Model:   "deepseek-v4-flash",
			Payload: []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`),
		}
		opts := cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAI,
			Headers:      http.Header{"x-opencode-session": []string{"client-explicit-ses-123"}},
		}

		_, err := executor.Execute(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
		if capturedSessionHeader != "client-explicit-ses-123" {
			t.Fatalf("expected client-explicit-ses-123, got %q", capturedSessionHeader)
		}
	})

	t.Run("auto-injects session in streaming mode", func(t *testing.T) {
		streamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedSessionHeader = r.Header.Get("x-opencode-session")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer streamServer.Close()

		streamAuth := &cliproxyauth.Auth{
			Provider: "openai-compatibility:opencode-go",
			Attributes: map[string]string{
				"base_url":     streamServer.URL + "/zen/go/v1",
				"api_key":      "sk-test",
				"compat_name":  "opencode-go",
				"provider_key": "openai-compatibility:opencode-go",
			},
		}

		capturedSessionHeader = ""
		req := cliproxyexecutor.Request{
			Model:   "deepseek-v4-flash",
			Payload: []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		}
		opts := cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAI,
			Stream:       true,
		}

		streamResult, err := executor.ExecuteStream(context.Background(), streamAuth, req, opts)
		if err != nil {
			t.Fatalf("ExecuteStream failed: %v", err)
		}
		if streamResult != nil {
			for range streamResult.Chunks {
			}
		}
		if capturedSessionHeader == "" {
			t.Fatal("expected x-opencode-session to be injected in streaming, got empty")
		}
	})

	t.Run("normalizes flattened tools for upstream chat completions", func(t *testing.T) {
		var capturedBody []byte
		toolsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			capturedBody = b
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
		}))
		defer toolsServer.Close()

		toolsAuth := &cliproxyauth.Auth{
			Provider: "openai-compatibility:opencode-go",
			Attributes: map[string]string{
				"base_url":     toolsServer.URL + "/zen/go/v1",
				"api_key":      "sk-test",
				"compat_name":  "opencode-go",
				"provider_key": "openai-compatibility:opencode-go",
			},
		}

		req := cliproxyexecutor.Request{
			Model:   "omen-alpha",
			Payload: []byte(`{"model":"omen-alpha","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}`),
		}
		opts := cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAI,
		}

		_, err := executor.Execute(context.Background(), toolsAuth, req, opts)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		fnName := gjson.GetBytes(capturedBody, "tools.0.function.name").String()
		if fnName != "exec" {
			t.Fatalf("expected tools.0.function.name to be 'exec', got %q in %s", fnName, string(capturedBody))
		}
	})
}
