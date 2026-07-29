package executor

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestRequestScopedCursorErrorRetainsExplicitAuthAndQuotaFailures(t *testing.T) {
	tests := []struct {
		name          string
		input         error
		requestScoped bool
	}{
		{name: "transport timeout", input: cursorStatusErr{code: http.StatusGatewayTimeout, msg: "timed out"}, requestScoped: true},
		{name: "quota", input: cursorStatusErr{code: http.StatusTooManyRequests, msg: "quota exceeded"}, requestScoped: false},
		{name: "unauthorized", input: cursorStatusErr{code: http.StatusUnauthorized, msg: "expired"}, requestScoped: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requestScopedCursorError(tt.input)
			var scoped interface{ IsRequestScoped() bool }
			if errors.As(got, &scoped) != tt.requestScoped {
				t.Fatalf("request scoped = %t, want %t", errors.As(got, &scoped), tt.requestScoped)
			}
		})
	}
}

func TestCursorTextDeltaJSONSeparatesThinkingFromVisibleContent(t *testing.T) {
	thinking := cursorTextDeltaJSON("private reasoning", true)
	if !json.Valid([]byte(thinking)) {
		t.Fatalf("thinking delta is invalid JSON: %q", thinking)
	}
	if strings.Contains(thinking, "<think>") || strings.Contains(thinking, `"content"`) {
		t.Fatalf("thinking leaked into visible content: %q", thinking)
	}
	var thinkingDelta map[string]string
	if err := json.Unmarshal([]byte(thinking), &thinkingDelta); err != nil {
		t.Fatalf("unmarshal thinking delta: %v", err)
	}
	if got := thinkingDelta["reasoning_content"]; got != "private reasoning" {
		t.Fatalf("reasoning_content = %q, want private reasoning", got)
	}

	visible := cursorTextDeltaJSON("final answer", false)
	var visibleDelta map[string]string
	if err := json.Unmarshal([]byte(visible), &visibleDelta); err != nil {
		t.Fatalf("unmarshal visible delta: %v", err)
	}
	if got := visibleDelta["content"]; got != "final answer" {
		t.Fatalf("content = %q, want final answer", got)
	}
	if _, exists := visibleDelta["reasoning_content"]; exists {
		t.Fatalf("visible delta unexpectedly contains reasoning_content: %q", visible)
	}
}

func TestCursorToolCallDeltaJSONEscapesCursorIdentifiers(t *testing.T) {
	exec := pendingMcpExec{
		ToolCallId: "call-primary\ncall-secondary",
		ToolName:   "read\nfile",
		Args:       `{"path":"/tmp/cursor_probe.json"}`,
	}

	delta := cursorToolCallDeltaJSON(0, exec)
	if !json.Valid([]byte(delta)) {
		t.Fatalf("tool-call delta is invalid JSON: %q", delta)
	}
	if strings.Contains(delta, "\n") {
		t.Fatalf("tool-call delta contains a literal newline that would split SSE: %q", delta)
	}

	var decoded struct {
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(delta), &decoded); err != nil {
		t.Fatalf("unmarshal tool-call delta: %v", err)
	}
	if len(decoded.ToolCalls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(decoded.ToolCalls))
	}
	if got := decoded.ToolCalls[0].ID; got != exec.ToolCallId {
		t.Fatalf("tool call id = %q, want %q", got, exec.ToolCallId)
	}
	if got := decoded.ToolCalls[0].Function.Name; got != exec.ToolName {
		t.Fatalf("tool name = %q, want %q", got, exec.ToolName)
	}
	if got := decoded.ToolCalls[0].Function.Arguments; got != exec.Args {
		t.Fatalf("tool arguments = %q, want %q", got, exec.Args)
	}
}

func TestSSEChunkEscapesEnvelopeStrings(t *testing.T) {
	chunk := sseChunk("chat\nid", 123, "cursor\nmodel", `{}`, `"stop"`)
	if !json.Valid(chunk.Payload) {
		t.Fatalf("SSE chunk payload is invalid JSON: %q", chunk.Payload)
	}
	if strings.Contains(string(chunk.Payload), "\n") {
		t.Fatalf("SSE chunk payload contains a literal newline: %q", chunk.Payload)
	}
}
