package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestOpenAICompatSemanticContinuationUsesRequestedAlias(t *testing.T) {
	var mu sync.Mutex
	var requestBodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
			return
		}
		mu.Lock()
		requestBodies = append(requestBodies, body)
		pass := len(requestBodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if pass == 1 {
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Launching the subagent:\"},\"finish_reason\":\"stop\"}]}\n"))
		} else {
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_2\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"spawn_agent\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		Codex: config.CodexConfig{
			SemanticContinuation: config.CodexSemanticContinuationConfig{
				Enabled:          true,
				Models:           []string{"dashscope/*"},
				MaxContinuations: 1,
			},
		},
	})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	result, errExecute := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "qwen3.8-max-preview",
		Payload: []byte(`{"model":"dashscope/qwen3.8-max-preview","input":[{"role":"user","content":"Launch a subagent."}],"tools":[{"type":"function","name":"spawn_agent","description":"launch a subagent","parameters":{"type":"object","properties":{}}}]}`),
	}, cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers:        http.Header{"User-Agent": []string{"Codex Desktop/26.7"}},
		Metadata: map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: "dashscope/qwen3.8-max-preview",
		},
	})
	if errExecute != nil {
		t.Fatalf("ExecuteStream error: %v", errExecute)
	}

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
	}

	mu.Lock()
	gotBodies := append([][]byte(nil), requestBodies...)
	mu.Unlock()
	if len(gotBodies) != 2 {
		t.Fatalf("upstream request count = %d, want 2; bodies=%q", len(gotBodies), gotBodies)
	}
	if !strings.Contains(string(gotBodies[1]), codexContinuationAnnouncedCallInstruction) {
		t.Fatalf("continuation request missing instruction: %s", gotBodies[1])
	}
}
