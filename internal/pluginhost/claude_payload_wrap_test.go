package pluginhost

import (
	"context"
	"encoding/json"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
)

func TestNormalizeClaudePluginPayloadTranslates(t *testing.T) {
	claudeResp := []byte(`{
		"id": "msg_test_123", "type": "message", "role": "assistant", "model": "zai/glm-5.3-flash",
		"content": [
			{"type": "thinking", "thinking": "reasoning here", "signature": "sig"},
			{"type": "text", "text": "CANARY-OK"}
		],
		"stop_reason": "end_turn", "stop_sequence": null,
		"usage": {"input_tokens": 20, "output_tokens": 118}
	}`)
	wrapped := normalizeClaudePluginPayload(claudeResp)
	if !contains(wrapped, []byte(`"type":"message_start"`)) {
		t.Fatalf("wrapper did not produce message_start: %s", wrapped[:min(200, len(wrapped))])
	}
	out := sdktranslator.TranslateNonStream(context.Background(),
		sdktranslator.FormatClaude, sdktranslator.FormatOpenAI,
		"zai/glm-5.3-flash", claudeResp, claudeResp, wrapped, nil)
	var openaiResp struct {
		Model    string `json:"model"`
		Choices  []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &openaiResp); err != nil {
		t.Fatalf("translated output is not valid openai json: %v: %s", err, out)
	}
	if len(openaiResp.Choices) != 1 || openaiResp.Choices[0].Message.Content != "CANARY-OK" {
		t.Fatalf("content not translated: %+v", openaiResp)
	}
	if openaiResp.Model != "zai/glm-5.3-flash" {
		t.Fatalf("model not carried: %q", openaiResp.Model)
	}
}

func TestNormalizeClaudePluginPayloadPassthrough(t *testing.T) {
	alreadySSE := []byte("data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"message_stop\"}\n\n")
	if got := normalizeClaudePluginPayload(alreadySSE); string(got) != string(alreadySSE) {
		t.Fatal("SSE payloads must pass through unchanged")
	}
	notClaude := []byte(`{"object":"chat.completion"}`)
	if got := normalizeClaudePluginPayload(notClaude); string(got) != string(notClaude) {
		t.Fatal("non-claude payloads must pass through unchanged")
	}
}

func contains(haystack, needle []byte) bool {
	return len(haystack) >= len(needle) && (string(haystack) == string(needle) || indexOf(haystack, needle) >= 0)
}
func indexOf(h, n []byte) int {
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := range n {
			if h[i+j] != n[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
