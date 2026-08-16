package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestResolveCursorDynamicModel(t *testing.T) {
	tests := []struct {
		name, model, payload, want string
	}{
		{"other model unchanged", "composer-2.5", `{"reasoning":{"effort":"low"}}`, "composer-2.5"},
		{"responses medium", "cursor-grok-4.6", `{"reasoning":{"effort":"medium"}}`, "cursor-grok-4.6-medium"},
		{"chat xhigh", "cursor-grok-4.6", `{"reasoning_effort":"xhigh"}`, "cursor-grok-4.6-xhigh"},
		{"chat xhigh fast", "cursor-grok-4.6", `{"reasoning_effort":"xhigh","service_tier":"priority"}`, "cursor-grok-4.6-xhigh-fast"},
		{"explicit low", "cursor-grok-4.6", `{"reasoning_effort":"low"}`, "cursor-grok-4.6-low"},
		{"missing effort defaults high", "cursor-grok-4.6", `{}`, "cursor-grok-4.6-high"},
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
		{ID: "composer-2.5", DisplayName: "Composer 2.5"},
		{ID: "cursor-grok-4.6-xhigh", DisplayName: "Grok 4.6 Extra High"},
		{ID: "cursor-grok-4.6-high", DisplayName: "Grok 4.6 High"},
		{ID: "cursor-grok-4.6-low-fast", DisplayName: "Grok 4.6 Low Fast"},
	}
	got := WithCursorGrok46VirtualModel(models)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (composer + virtual)", len(got))
	}
	if got[0].ID != "composer-2.5" {
		t.Fatalf("first = %q, want composer-2.5", got[0].ID)
	}
	virtual := got[1]
	if virtual.ID != "cursor-grok-4.6" || virtual.DisplayName != "Cursor Grok 4.6" {
		t.Fatalf("virtual = id %q name %q", virtual.ID, virtual.DisplayName)
	}
	if virtual.Thinking == nil {
		t.Fatal("virtual thinking metadata is nil")
	}
	if gotLevels := strings.Join(virtual.Thinking.Levels, ","); gotLevels != "low,medium,high,xhigh" {
		t.Fatalf("virtual thinking levels = %q, want low,medium,high,xhigh", gotLevels)
	}
	for _, model := range got {
		if isCursorGrok46TierID(model.ID) {
			t.Fatalf("tier SKU leaked into advertised catalog: %q", model.ID)
		}
	}
	if again := WithCursorGrok46VirtualModel(got); len(again) != 2 {
		t.Fatalf("virtual model was added twice: len=%d", len(again))
	}
}

func TestWithCursorGrok46VirtualModelAttachesLevelsToExistingVirtualID(t *testing.T) {
	models := []*registry.ModelInfo{
		{ID: "cursor-grok-4.6", DisplayName: "Grok 4.6"},
		{ID: "cursor-grok-4.6-medium", DisplayName: "Grok 4.6 Medium"},
	}
	got := WithCursorGrok46VirtualModel(models)
	if len(got) != 1 || got[0].ID != "cursor-grok-4.6" {
		t.Fatalf("advertised = %#v", got)
	}
	if got[0].DisplayName != "Cursor Grok 4.6" {
		t.Fatalf("display name = %q", got[0].DisplayName)
	}
	if got[0].Thinking == nil || strings.Join(got[0].Thinking.Levels, ",") != "low,medium,high,xhigh" {
		t.Fatalf("thinking = %#v", got[0].Thinking)
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

func TestCursorToolCallBatchMaxAllowsSlowGrokSiblings(t *testing.T) {
	if cursorToolCallBatchMax < 10*time.Second {
		t.Fatalf("cursorToolCallBatchMax = %v, want >=10s so Grok xhigh git/PR siblings are not dropped after 3s", cursorToolCallBatchMax)
	}
	started := time.Unix(0, 0)
	wait, ok := nextCursorToolCallBatchWait(started, started.Add(4*time.Second))
	if !ok || wait != cursorToolCallBatchWindow {
		t.Fatalf("4s sibling wait = (%v, %v), want full quiet window", wait, ok)
	}
}

func TestDrainCursorSideChannelCollectsLateMcpArgs(t *testing.T) {
	buf := bytes.NewBuffer(encodeTestCursorMcpExecFrame(9, "exec-9", "exec_command", "call-late"))
	var collect []pendingMcpExec
	if err := drainCursorSideChannel(cursorproto.NewMemoryH2Stream(), buf, map[string][]byte{}, nil, nil, &collect); err != nil {
		t.Fatalf("drainCursorSideChannel() error = %v", err)
	}
	if len(collect) != 1 {
		t.Fatalf("collected %d execs, want 1", len(collect))
	}
	if collect[0].ToolName != "exec_command" || collect[0].ToolCallId != "call-late" {
		t.Fatalf("collected %+v, want exec_command/call-late", collect[0])
	}
}

func TestDispatchCursorClientToolsQueuesLateExecsOntoNextResume(t *testing.T) {
	stream := cursorproto.NewMemoryH2Stream()
	toolResultCh := make(chan []toolResultInfo, 1)
	first := []pendingMcpExec{{
		ExecMsgId:  1,
		ExecId:     "exec-1",
		ToolCallId: "call-first",
		ToolName:   "exec_command",
		Args:       `{"cmd":"git status"}`,
	}}

	var batches [][]string
	onToolExec := func(batch []pendingMcpExec) {
		ids := make([]string, 0, len(batch))
		for _, exec := range batch {
			ids = append(ids, exec.ToolCallId)
		}
		batches = append(batches, ids)
		if len(batches) == 1 {
			stream.PushData(encodeTestCursorMcpExecFrame(2, "exec-2", "exec_command", "call-late"))
			toolResultCh <- []toolResultInfo{{ToolCallId: "call-first", Content: "on main"}}
			return
		}
		toolResultCh <- []toolResultInfo{{ToolCallId: "call-late", Content: "pr 1"}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dispatchCursorClientTools(ctx, stream, new(bytes.Buffer), map[string][]byte{}, nil, nil, onToolExec, toolResultCh, first); err != nil {
		t.Fatalf("dispatchCursorClientTools() error = %v", err)
	}

	if len(batches) != 2 {
		t.Fatalf("onToolExec batches = %v, want [call-first] then [call-late]", batches)
	}
	if len(batches[0]) != 1 || batches[0][0] != "call-first" {
		t.Fatalf("first batch = %v, want [call-first]", batches[0])
	}
	if len(batches[1]) != 1 || batches[1][0] != "call-late" {
		t.Fatalf("late batch = %v, want [call-late]", batches[1])
	}
}

func encodeTestCursorMcpExecFrame(execMsgId uint32, execId, toolName, toolCallId string) []byte {
	mcp := protowire.AppendTag(nil, cursorproto.MCA_Name, protowire.BytesType)
	mcp = protowire.AppendString(mcp, toolName)
	mcp = protowire.AppendTag(mcp, cursorproto.MCA_ToolCallId, protowire.BytesType)
	mcp = protowire.AppendString(mcp, toolCallId)
	mcp = protowire.AppendTag(mcp, cursorproto.MCA_ToolName, protowire.BytesType)
	mcp = protowire.AppendString(mcp, toolName)

	exec := protowire.AppendTag(nil, cursorproto.ESM_Id, protowire.VarintType)
	exec = protowire.AppendVarint(exec, uint64(execMsgId))
	exec = protowire.AppendTag(exec, cursorproto.ESM_ExecId, protowire.BytesType)
	exec = protowire.AppendString(exec, execId)
	exec = protowire.AppendTag(exec, cursorproto.ESM_McpArgs, protowire.BytesType)
	exec = protowire.AppendBytes(exec, mcp)

	asm := protowire.AppendTag(nil, cursorproto.ASM_ExecServerMessage, protowire.BytesType)
	asm = protowire.AppendBytes(asm, exec)
	return cursorproto.FrameConnectMessage(asm, 0)
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

func TestCursorTokenUsageSnapshotAndReset(t *testing.T) {
	tokens := &cursorTokenUsage{}
	tokens.setInputEstimate(40)
	tokens.addOutput(7)
	input, output := tokens.snapshotAndReset()
	if input != 10 || output != 7 {
		t.Fatalf("snapshot = %d/%d, want 10/7", input, output)
	}
	input, output = tokens.snapshotAndReset()
	if input != 0 || output != 0 {
		t.Fatalf("second snapshot = %d/%d, want 0/0", input, output)
	}
}

func TestCursorUsageDetailUsesSubsetBreakdownWithoutCache(t *testing.T) {
	tokens := &cursorTokenUsage{}
	tokens.setInputEstimate(16)
	tokens.addOutput(3)
	detail := cursorUsageDetail(tokens)
	if detail.InputTokens != 4 || detail.OutputTokens != 3 || detail.TotalTokens != 7 {
		t.Fatalf("detail tokens = %+v", detail)
	}
	if detail.CacheReadTokens != 0 || detail.CachedTokens != 0 {
		t.Fatalf("cold-start cache = %+v", detail)
	}
	if detail.InputTokensIncludesCache == nil || !*detail.InputTokensIncludesCache {
		t.Fatal("expected input to include cache")
	}
	if !detail.TokenBreakdown.Valid() || detail.TokenBreakdown.Input.UncachedTokens != 4 || detail.TokenBreakdown.Output.NonReasoningTokens != 3 {
		t.Fatalf("breakdown = %+v", detail.TokenBreakdown)
	}
}

func TestCursorUsageDetailTreatsCheckpointReuseAsCacheRead(t *testing.T) {
	tokens := &cursorTokenUsage{}
	tokens.setInputEstimate(16)
	tokens.setPriorContext(100)
	tokens.addOutput(5)
	detail := cursorUsageDetail(tokens)
	if detail.InputTokens != 104 || detail.CacheReadTokens != 100 || detail.OutputTokens != 5 || detail.TotalTokens != 109 {
		t.Fatalf("detail = %+v", detail)
	}
	if !detail.TokenBreakdown.Valid() || detail.TokenBreakdown.Input.UncachedTokens != 4 || detail.TokenBreakdown.Input.CacheReadTokens != 100 {
		t.Fatalf("breakdown = %+v", detail.TokenBreakdown)
	}
}

func TestCursorUsageDetailUsesCheckpointContextSize(t *testing.T) {
	tokens := &cursorTokenUsage{}
	tokens.setInputEstimate(16)
	tokens.setPriorContext(80)
	tokens.setContextUsed(120)
	tokens.addOutput(10)
	detail := cursorUsageDetail(tokens)
	if detail.InputTokens != 110 || detail.CacheReadTokens != 80 || detail.OutputTokens != 10 || detail.TotalTokens != 120 {
		t.Fatalf("detail = %+v", detail)
	}
	if !detail.TokenBreakdown.Valid() || detail.TokenBreakdown.Input.UncachedTokens != 30 {
		t.Fatalf("breakdown = %+v", detail.TokenBreakdown)
	}
}

func TestCursorUsageDetailFirstTurnWithContextHasZeroCache(t *testing.T) {
	tokens := &cursorTokenUsage{}
	tokens.setContextUsed(50)
	tokens.addOutput(8)
	detail := cursorUsageDetail(tokens)
	if detail.InputTokens != 42 || detail.CacheReadTokens != 0 || detail.OutputTokens != 8 {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestCursorUsageDetailPromotesContextToPriorOnNextRequest(t *testing.T) {
	tokens := &cursorTokenUsage{}
	tokens.setContextUsed(80)
	tokens.addOutput(6)
	first := cursorUsageDetail(tokens)
	if first.CacheReadTokens != 0 || first.InputTokens != 74 {
		t.Fatalf("first detail = %+v", first)
	}
	tokens.setInputEstimate(8)
	tokens.setContextUsed(95)
	tokens.addOutput(4)
	second := cursorUsageDetail(tokens)
	if second.InputTokens != 91 || second.CacheReadTokens != 80 || second.OutputTokens != 4 {
		t.Fatalf("second detail = %+v", second)
	}
}

func TestStoreCheckpointRecordsUsedTokens(t *testing.T) {
	var inner []byte
	inner = protowire.AppendTag(inner, cursorproto.CTD_UsedTokens, protowire.VarintType)
	inner = protowire.AppendVarint(inner, 321)
	var checkpoint []byte
	checkpoint = protowire.AppendTag(checkpoint, cursorproto.CSS_TokenDetails, protowire.BytesType)
	checkpoint = protowire.AppendBytes(checkpoint, inner)

	e := NewCursorExecutor(&config.Config{})
	tokens := &cursorTokenUsage{}
	e.storeCheckpoint("conv-1", "auth-1", nil, tokens, checkpoint)
	e.mu.Lock()
	saved := e.checkpoints["conv-1"]
	e.mu.Unlock()
	if saved == nil || saved.usedTokens != 321 || saved.authID != "auth-1" {
		t.Fatalf("saved = %+v", saved)
	}
	tokens.addOutput(21)
	detail := cursorUsageDetail(tokens)
	if detail.InputTokens != 300 || detail.CacheReadTokens != 0 || detail.OutputTokens != 21 {
		t.Fatalf("first-turn detail = %+v", detail)
	}
}

type captureCursorUsagePlugin struct {
	records chan usage.Record
}

func (p *captureCursorUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	if p == nil || record.Provider != "cursor" {
		return
	}
	select {
	case p.records <- record:
	default:
	}
}

func waitForCursorUsageRecord(t *testing.T, records <-chan usage.Record) usage.Record {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cursor usage record")
		return usage.Record{}
	}
}

func TestPublishCursorUsageEmitsCursorProviderRecord(t *testing.T) {
	records := make(chan usage.Record, 2)
	usage.RegisterPlugin(&captureCursorUsagePlugin{records: records})

	auth := &cliproxyauth.Auth{
		ID:       "cursor.json",
		Provider: "cursor",
		Attributes: map[string]string{
			"path": "cursor.json",
		},
	}
	reporter := helps.NewExecutorUsageReporter(context.Background(), NewCursorExecutor(&config.Config{}), "cursor-grok-4.6-high", auth)
	tokens := &cursorTokenUsage{}
	tokens.setInputEstimate(40)
	tokens.addOutput(7)
	publishCursorUsage(context.Background(), reporter, tokens)

	record := waitForCursorUsageRecord(t, records)
	if record.Provider != "cursor" {
		t.Fatalf("provider = %q, want cursor", record.Provider)
	}
	if record.ExecutorType != "CursorExecutor" {
		t.Fatalf("executor type = %q, want CursorExecutor", record.ExecutorType)
	}
	if record.Model != "cursor-grok-4.6-high" {
		t.Fatalf("model = %q, want cursor-grok-4.6-high", record.Model)
	}
	if record.Failed {
		t.Fatalf("record failed unexpectedly: %+v", record.Fail)
	}
	if record.Detail.InputTokens != 10 || record.Detail.OutputTokens != 7 {
		t.Fatalf("detail = %+v", record.Detail)
	}
}

func TestPublishCursorUsageCountsRequestWithoutTokens(t *testing.T) {
	records := make(chan usage.Record, 2)
	usage.RegisterPlugin(&captureCursorUsagePlugin{records: records})

	reporter := helps.NewExecutorUsageReporter(context.Background(), NewCursorExecutor(&config.Config{}), "cursor-grok-4.6", nil)
	publishCursorUsage(context.Background(), reporter, nil)

	record := waitForCursorUsageRecord(t, records)
	if record.Provider != "cursor" || record.Model != "cursor-grok-4.6" || record.Failed {
		t.Fatalf("record = %+v", record)
	}
	if record.Detail.TotalTokens != 0 {
		t.Fatalf("zero-token record had tokens %+v", record.Detail)
	}
}

func TestCursorHTTPUsagePublishesOncePerRequest(t *testing.T) {
	records := make(chan usage.Record, 2)
	usage.RegisterPlugin(&captureCursorUsagePlugin{records: records})

	httpUsage := &cursorHTTPUsage{}
	reporter := helps.NewExecutorUsageReporter(context.Background(), NewCursorExecutor(&config.Config{}), "cursor-grok-4.6-medium", nil)
	httpUsage.attach(context.Background(), reporter)
	tokens := &cursorTokenUsage{}
	tokens.setInputEstimate(8)
	tokens.addOutput(1)
	httpUsage.publish(tokens)
	httpUsage.publish(tokens)

	record := waitForCursorUsageRecord(t, records)
	if record.Model != "cursor-grok-4.6-medium" {
		t.Fatalf("model = %q", record.Model)
	}
	select {
	case extra := <-records:
		t.Fatalf("published extra record: %+v", extra)
	case <-time.After(150 * time.Millisecond):
	}
}
