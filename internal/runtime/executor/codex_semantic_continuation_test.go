package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func codexContinuationTestConfig(enabled bool, models []string, maxContinuations int) *config.Config {
	return &config.Config{
		Codex: config.CodexConfig{
			SemanticContinuation: config.CodexSemanticContinuationConfig{
				Enabled:          enabled,
				Models:           models,
				MaxContinuations: maxContinuations,
			},
		},
	}
}

func codexContinuationTestHeaders(userAgent string) http.Header {
	headers := http.Header{}
	if userAgent != "" {
		headers.Set("User-Agent", userAgent)
	}
	return headers
}

const codexContinuationTestBody = `{"model":"k3-256k","messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"spawn_agent"}}]}`

func TestCodexContinuationControllerGate(t *testing.T) {
	codexUA := codexContinuationTestHeaders("Codex Desktop/26.7")
	tests := []struct {
		name           string
		cfg            *config.Config
		headers        http.Header
		responseFormat sdktranslator.Format
		body           string
		wantOK         bool
	}{
		{"enabled codex responses", codexContinuationTestConfig(true, []string{"kimi-*"}, 1), codexUA, sdktranslator.FormatOpenAIResponse, codexContinuationTestBody, true},
		{"feature disabled", codexContinuationTestConfig(false, []string{"kimi-*"}, 1), codexUA, sdktranslator.FormatOpenAIResponse, codexContinuationTestBody, false},
		{"model not opted in", codexContinuationTestConfig(true, []string{"gpt-*"}, 1), codexUA, sdktranslator.FormatOpenAIResponse, codexContinuationTestBody, false},
		{"non-codex client", codexContinuationTestConfig(true, []string{"kimi-*"}, 1), codexContinuationTestHeaders("pi/1.0"), sdktranslator.FormatOpenAIResponse, codexContinuationTestBody, false},
		{"chat completions surface", codexContinuationTestConfig(true, []string{"kimi-*"}, 1), codexUA, sdktranslator.FromString("openai"), codexContinuationTestBody, false},
		{"no tools offered", codexContinuationTestConfig(true, []string{"kimi-*"}, 1), codexUA, sdktranslator.FormatOpenAIResponse, `{"model":"k3-256k","messages":[]}`, false},
		{"codex exec client", codexContinuationTestConfig(true, []string{"kimi-*"}, 1), codexContinuationTestHeaders("codex_exec/0.146.0"), sdktranslator.FormatOpenAIResponse, codexContinuationTestBody, true},
		{"codex tui client", codexContinuationTestConfig(true, []string{"kimi-*"}, 1), codexContinuationTestHeaders("codex-tui/0.146.0"), sdktranslator.FormatOpenAIResponse, codexContinuationTestBody, true},
		{"openai compat requested alias", codexContinuationTestConfig(true, []string{"dashscope/*"}, 1), codexUA, sdktranslator.FormatOpenAIResponse, codexContinuationTestBody, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := "kimi-k3-256k"
			if tt.name == "openai compat requested alias" {
				model = "dashscope/qwen3.8-max-preview"
			}
			_, ok := newCodexContinuationController(context.Background(), tt.headers, tt.cfg, model, tt.responseFormat, []byte(tt.body))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestCodexContinuationPassInstruction(t *testing.T) {
	tests := []struct {
		name         string
		lines        []string
		toolsOffered bool
		wantOK       bool
		wantInstr    string
	}{
		{
			name: "announced spawn without call",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"Spawning the final dependent task — C2-03 shadow comparison:"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			toolsOffered: true,
			wantOK:       true,
			wantInstr:    codexContinuationAnnouncedCallInstruction,
		},
		{
			name: "empty turn",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			toolsOffered: true,
			wantOK:       true,
			wantInstr:    codexContinuationEmptyTurnInstruction,
		},
		{
			name: "legitimate final answer",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"All 402 tests pass and the diff is clean."}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			toolsOffered: true,
			wantOK:       false,
		},
		{
			name: "prose colon without tool verb",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"Here are the results:"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			toolsOffered: true,
			wantOK:       false,
		},
		{
			name: "narrated commit without call",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"Gate green (exit 0). Committing ERM-231 and closing it in Linear:"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			toolsOffered: true,
			wantOK:       true,
			wantInstr:    codexContinuationAnnouncedCallInstruction,
		},
		{
			name: "narrated run split across deltas",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"Volta reports complete. Running the parent"}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":" gate: scope, full suite, and a security-focused diff review:"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			toolsOffered: true,
			wantOK:       true,
			wantInstr:    codexContinuationAnnouncedCallInstruction,
		},
		{
			name: "trailing clause without action stays complete",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"Summary of the change. The results:"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			toolsOffered: true,
			wantOK:       false,
		},
		{
			name: "earlier participle with clean ending stays complete",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"The suite is running. All checks pass:"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			toolsOffered: true,
			wantOK:       false,
		},
		{
			name: "tool call emitted",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"spawn_agent"}}]}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			},
			toolsOffered: true,
			wantOK:       false,
		},
		{
			name: "length finish is a real outcome",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"Spawning the final task:"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
			},
			toolsOffered: true,
			wantOK:       false,
		},
		{
			name: "no tools offered",
			lines: []string{
				`data: {"choices":[{"index":0,"delta":{"content":"Spawning the final task:"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			toolsOffered: false,
			wantOK:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pass codexContinuationPass
			for _, line := range tt.lines {
				pass.observe([]byte(line))
			}
			instruction, ok := pass.instruction(tt.toolsOffered)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && instruction != tt.wantInstr {
				t.Fatalf("instruction = %q, want %q", instruction, tt.wantInstr)
			}
		})
	}
}

func TestCodexContinuationBodyAppendsInstructedTurn(t *testing.T) {
	controller, ok := newCodexContinuationController(context.Background(), codexContinuationTestHeaders("Codex Desktop/26.7"), codexContinuationTestConfig(true, []string{"kimi-*"}, 1), "kimi-k3-256k", sdktranslator.FormatOpenAIResponse, []byte(codexContinuationTestBody))
	if !ok {
		t.Fatal("controller disabled")
	}
	controller.observe([]byte(`data: {"choices":[{"index":0,"delta":{"content":"Spawning the final dependent task:"}}]}`))
	controller.observe([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	if !controller.shouldContinue() {
		t.Fatal("shouldContinue = false, want true")
	}
	next, okNext := controller.continuationBody([]byte(codexContinuationTestBody))
	if !okNext {
		t.Fatal("continuationBody ok = false, want true")
	}
	messages := gjson.GetBytes(next, "messages")
	if !messages.IsArray() || len(messages.Array()) != 3 {
		t.Fatalf("messages = %s, want 3 entries", messages.Raw)
	}
	assistant := messages.Array()[1]
	if assistant.Get("role").String() != "assistant" || assistant.Get("content").String() != "Spawning the final dependent task:" {
		t.Fatalf("assistant echo = %s", assistant.Raw)
	}
	user := messages.Array()[2]
	if user.Get("role").String() != "user" || user.Get("content").String() != codexContinuationAnnouncedCallInstruction {
		t.Fatalf("instruction message = %s", user.Raw)
	}
	// Budget exhausted: a second continuation must not be offered.
	controller.observe([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	if controller.shouldContinue() {
		t.Fatal("shouldContinue after budget exhausted = true, want false")
	}
	if _, okAgain := controller.continuationBody([]byte(codexContinuationTestBody)); okAgain {
		t.Fatal("continuationBody after budget exhausted ok = true, want false")
	}
}

func TestCodexContinuationOffsetLine(t *testing.T) {
	controller := &codexContinuationController{}
	line := []byte(`data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`)
	if got := controller.offsetLine(line); string(got) != string(line) {
		t.Fatalf("pass 0 rewrote line: %s", got)
	}
	controller.passIndex = 1
	got := controller.offsetLine(line)
	if index := gjson.GetBytes(got, "choices.0.index"); index.Int() != 1 {
		t.Fatalf("pass 1 choice index = %d, want 1 (%s)", index.Int(), got)
	}
	if !strings.HasPrefix(string(got), "data: ") {
		t.Fatalf("pass 1 lost data prefix: %s", got)
	}
	done := []byte("data: [DONE]")
	if gotDone := controller.offsetLine(done); string(gotDone) != string(done) {
		t.Fatalf("done marker rewritten: %s", gotDone)
	}
}

func TestCodexContinuationIsUpstreamDoneLine(t *testing.T) {
	controller := &codexContinuationController{}
	if !controller.isUpstreamDoneLine([]byte("data: [DONE]")) {
		t.Fatal("data: [DONE] not detected")
	}
	if controller.isUpstreamDoneLine([]byte(`data: {"choices":[]}`)) {
		t.Fatal("chunk misdetected as [DONE]")
	}
	if controller.isUpstreamDoneLine([]byte(": keepalive")) {
		t.Fatal("comment misdetected as [DONE]")
	}
}
