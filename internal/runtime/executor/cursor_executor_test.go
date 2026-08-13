package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestResolveCursorDynamicModel(t *testing.T) {
	tests := []struct {
		name, model, payload, want string
	}{
		{"other model unchanged", "composer-2.5", `{"reasoning":{"effort":"low"}}`, "composer-2.5"},
		{"responses medium", "cursor-grok-4.6", `{"reasoning":{"effort":"medium"}}`, "cursor-grok-4.6-medium"},
		{"chat xhigh fast", "cursor-grok-4.6", `{"reasoning_effort":"xhigh","service_tier":"priority"}`, "cursor-grok-4.6-xhigh-fast"},
		{"unsupported effort defaults high", "cursor-grok-4.6", `{"reasoning":{"effort":"max"}}`, "cursor-grok-4.6-high"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveCursorDynamicModel(test.model, []byte(test.payload)); got != test.want {
				t.Fatalf("resolveCursorDynamicModel() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCursorSessionIDUsesCodexExecutionMetadata(t *testing.T) {
	opts := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "codex-session-1",
	}}
	got := cursorSessionID(context.Background(), cliproxyexecutor.Request{}, opts)
	want := helps.ProviderSessionUUID(cursorAuthType, opts.Metadata)
	if got == "" || got != want {
		t.Fatalf("cursorSessionID() = %q, want %q", got, want)
	}
}

func TestCursorSessionIDUsesGenericSessionAffinityHeader(t *testing.T) {
	want := "pi-session-123"
	opts := cliproxyexecutor.Options{
		Headers: map[string][]string{"X-Session-Affinity": {want}},
	}
	if got := cursorSessionID(context.Background(), cliproxyexecutor.Request{}, opts); got != want {
		t.Fatalf("cursorSessionID() = %q, want %q", got, want)
	}
}

func TestSanitizeCursorToolCallIDReplacesControlCharacters(t *testing.T) {
	raw := "call-primary\nfc_secondary"
	got := sanitizeCursorToolCallID(raw)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("sanitized id still contains control characters: %q", got)
	}
	if got != "call-primary_fc_secondary" {
		t.Fatalf("sanitizeCursorToolCallID() = %q, want call-primary_fc_secondary", got)
	}
}

func TestMatchCursorToolResultAcceptsNewlineVariants(t *testing.T) {
	pending := pendingMcpExec{ToolCallId: sanitizeCursorToolCallID("call-primary\nfc_secondary")}
	tests := []struct {
		name    string
		results []toolResultInfo
		want    string
		kind    string
	}{
		{
			name:    "exact sanitized",
			results: []toolResultInfo{{ToolCallId: "call-primary_fc_secondary", Content: "ok"}},
			want:    "ok",
			kind:    "exact",
		},
		{
			name:    "client kept the raw newline",
			results: []toolResultInfo{{ToolCallId: "call-primary\nfc_secondary", Content: "ok"}},
			want:    "ok",
			kind:    "normalized",
		},
		{
			name:    "client stripped the newline",
			results: []toolResultInfo{{ToolCallId: "call-primaryfc_secondary", Content: "ok"}},
			want:    "ok",
			kind:    "normalized",
		},
		{
			name:    "client truncated at newline",
			results: []toolResultInfo{{ToolCallId: "call-primary", Content: "ok"}},
			want:    "ok",
			kind:    "fallback",
		},
		{
			name: "fallback to latest result",
			results: []toolResultInfo{
				{ToolCallId: "older", Content: "stale"},
				{ToolCallId: "unrelated", Content: "latest"},
			},
			want: "latest",
			kind: "fallback",
		},
		{
			name:    "no results",
			results: nil,
			want:    "",
			kind:    "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, kind := matchCursorToolResult(pending, test.results)
			if kind != test.kind {
				t.Fatalf("kind = %q, want %q", kind, test.kind)
			}
			if got.Content != test.want {
				t.Fatalf("content = %q, want %q", got.Content, test.want)
			}
		})
	}
}

func TestParseOpenAIRequestSanitizesToolCallIDs(t *testing.T) {
	payload := []byte(`{
		"model":"cursor-grok-4.6",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call-primary\nfc_secondary","type":"function","function":{"name":"read","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call-primary\nfc_secondary","content":"ok"}
		]
	}`)
	parsed := parseOpenAIRequest(payload)
	if len(parsed.ToolResults) != 1 {
		t.Fatalf("tool results = %d, want 1", len(parsed.ToolResults))
	}
	if got := parsed.ToolResults[0].ToolCallId; got != "call-primary_fc_secondary" {
		t.Fatalf("tool_call_id = %q, want call-primary_fc_secondary", got)
	}
}

func TestWithCursorGrok46VirtualModelAddsMissingVirtualID(t *testing.T) {
	models := []*registry.ModelInfo{
		{ID: "cursor-grok-4.6-xhigh", DisplayName: "Grok 4.6 Extra High"},
		{ID: "cursor-grok-4.6-high", DisplayName: "Grok 4.6 High"},
	}
	got := WithCursorGrok46VirtualModel(models)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	virtual := got[len(got)-1]
	if virtual.ID != "cursor-grok-4.6" || virtual.DisplayName != "Cursor Grok 4.6" {
		t.Fatalf("virtual = id %q name %q", virtual.ID, virtual.DisplayName)
	}
	if again := WithCursorGrok46VirtualModel(got); len(again) != 3 {
		t.Fatalf("virtual model was added twice: len=%d", len(again))
	}
}

func TestBuildRunRequestParamsUsesResolvedCursorModel(t *testing.T) {
	parsed := parseOpenAIRequest([]byte(`{"model":"cursor-grok-4.6","messages":[{"role":"user","content":"hi"}]}`))
	resolved := resolveCursorDynamicModel(parsed.Model, []byte(`{"reasoning_effort":"high"}`))
	params := buildRunRequestParams(parsed, "conv", resolved)
	if params.ModelId != "cursor-grok-4.6-high" {
		t.Fatalf("ModelId = %q, want cursor-grok-4.6-high", params.ModelId)
	}
	if parsed.Model != "cursor-grok-4.6" {
		t.Fatalf("client-facing model mutated: %q", parsed.Model)
	}
}

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

func TestPendingCursorShellExecBridgesToBash(t *testing.T) {
	exec := pendingCursorShellExec(&cursorproto.DecodedServerMessage{
		ExecMsgId:        42,
		ExecId:           "shell-exec",
		Command:          "printf SHELL_OK",
		WorkingDirectory: "/tmp",
	}, true, "bash")

	if exec.ToolName != "bash" || !exec.NativeShell {
		t.Fatalf("tool = %q nativeShell=%t, want bash native shell", exec.ToolName, exec.NativeShell)
	}
	if !exec.NativeShellStream {
		t.Fatal("native shell stream was not preserved")
	}
	if exec.ToolCallId == "" {
		t.Fatal("tool call ID is empty")
	}
	if exec.Command != "printf SHELL_OK" || exec.WorkingDirectory != "/tmp" {
		t.Fatalf("shell metadata = command %q cwd %q", exec.Command, exec.WorkingDirectory)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(exec.Args), &args); err != nil {
		t.Fatalf("unmarshal shell args: %v", err)
	}
	if got := args["command"]; got != "printf SHELL_OK" {
		t.Fatalf("command = %q, want printf SHELL_OK", got)
	}
}

func TestCursorHasTool(t *testing.T) {
	tools := []cursorproto.McpToolDef{{Name: "read"}, {Name: "bash"}}
	if !cursorHasTool(tools, "bash") {
		t.Fatal("bash tool was not found")
	}
	if cursorHasTool(tools, "write") {
		t.Fatal("unexpected write tool match")
	}
}

func TestCursorShellBridgeToolPrefersBashThenCodexNames(t *testing.T) {
	if name, ok := cursorShellBridgeTool([]cursorproto.McpToolDef{{Name: "read"}}); ok {
		t.Fatalf("unexpected shell bridge %q", name)
	}
	if name, ok := cursorShellBridgeTool([]cursorproto.McpToolDef{{Name: "read"}, {Name: "bash"}, {Name: "shell"}}); !ok || name != "bash" {
		t.Fatalf("bridge = %q ok=%t, want bash", name, ok)
	}
	if name, ok := cursorShellBridgeTool([]cursorproto.McpToolDef{{Name: "shell"}}); !ok || name != "shell" {
		t.Fatalf("bridge = %q ok=%t, want shell", name, ok)
	}
	if name, ok := cursorShellBridgeTool([]cursorproto.McpToolDef{{Name: "terminal__exec"}}); !ok || name != "terminal__exec" {
		t.Fatalf("bridge = %q ok=%t, want terminal__exec", name, ok)
	}
}

func TestPendingCursorShellExecUsesClientToolShape(t *testing.T) {
	msg := &cursorproto.DecodedServerMessage{
		Command:          "pwd",
		WorkingDirectory: "/tmp",
	}
	shell := pendingCursorShellExec(msg, false, "shell")
	if shell.ToolName != "shell" || !shell.NativeShell {
		t.Fatalf("tool = %q native=%t, want shell", shell.ToolName, shell.NativeShell)
	}
	if got := gjson.Get(shell.Args, "command").String(); got != "pwd" {
		t.Fatalf("shell command = %q", got)
	}
	if got := gjson.Get(shell.Args, "workdir").String(); got != "/tmp" {
		t.Fatalf("shell workdir = %q", got)
	}

	execCommand := pendingCursorShellExec(msg, false, "exec_command")
	if got := gjson.Get(execCommand.Args, "cmd").String(); got != "pwd" {
		t.Fatalf("exec_command cmd = %q", got)
	}

	exec := pendingCursorShellExec(msg, false, "exec")
	input := gjson.Get(exec.Args, "input").String()
	if !strings.Contains(input, `cmd:"pwd"`) || !strings.Contains(input, `workdir:"/tmp"`) {
		t.Fatalf("exec input = %q", input)
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

// TestMatchCursorToolResultAtDistributesParallelBatch guards the parallel
// tool-call path: Cursor opens one exec per call and every exec must receive a
// distinct result, otherwise the H2 turn stalls and the harness hangs.
func TestMatchCursorToolResultAtDistributesParallelBatch(t *testing.T) {
	pending := []pendingMcpExec{
		{ToolCallId: "call-a", ToolName: "get_weather"},
		{ToolCallId: "call-b", ToolName: "get_population"},
	}
	results := []toolResultInfo{
		{ToolCallId: "call-b", Content: "population"},
		{ToolCallId: "call-a", Content: "weather"},
	}

	used := make([]bool, len(results))
	got := make([]string, 0, len(pending))
	for _, exec := range pending {
		tr, kind, idx := matchCursorToolResultAt(exec, results, used)
		if kind != "exact" {
			t.Fatalf("tool %s matched via %q, want exact", exec.ToolName, kind)
		}
		if idx < 0 {
			t.Fatalf("tool %s did not consume a result", exec.ToolName)
		}
		used[idx] = true
		got = append(got, tr.Content)
	}

	if got[0] != "weather" || got[1] != "population" {
		t.Fatalf("results = %v, want [weather population]", got)
	}
}

// TestMatchCursorToolResultAtDoesNotReuseConsumedResults ensures a second exec
// never silently receives the result already handed to a sibling exec.
func TestMatchCursorToolResultAtDoesNotReuseConsumedResults(t *testing.T) {
	results := []toolResultInfo{{ToolCallId: "call-a", Content: "only"}}
	used := make([]bool, len(results))

	if _, kind, idx := matchCursorToolResultAt(pendingMcpExec{ToolCallId: "call-a"}, results, used); kind != "exact" || idx != 0 {
		t.Fatalf("first match kind=%q idx=%d, want exact/0", kind, idx)
	}
	used[0] = true

	_, kind, idx := matchCursorToolResultAt(pendingMcpExec{ToolCallId: "call-b"}, results, used)
	if kind != "" || idx != -1 {
		t.Fatalf("second match kind=%q idx=%d, want no match so Cursor gets an explicit error", kind, idx)
	}
}
