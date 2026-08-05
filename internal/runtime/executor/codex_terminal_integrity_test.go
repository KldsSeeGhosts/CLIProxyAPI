package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexTerminalArbiterExplicitDoneNarratedCallContinues(t *testing.T) {
	controller := &codexContinuationController{remaining: 1, toolsOffered: true}
	controller.observe([]byte(`data: {"choices":[{"delta":{"content":"Spawning the final dependent task —"},"finish_reason":"stop"}]}`))
	decision := controller.decide(codexContinuationBoundaryExplicitDone, nil)
	if decision.outcome != codexContinuationOutcomeContinue || decision.classification != "narrated_no_tool" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestCodexTerminalArbiterCleanEOFNarratedCallContinues(t *testing.T) {
	controller := &codexContinuationController{remaining: 1, toolsOffered: true}
	controller.observe([]byte(`data: {"choices":[{"delta":{"content":"Spawning the dependent task:"}}]}`))
	decision := controller.decide(codexContinuationBoundaryCleanEOF, nil)
	if decision.outcome != codexContinuationOutcomeContinue {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestCodexTerminalArbiterFinishReasonEOFCompletes(t *testing.T) {
	controller := &codexContinuationController{remaining: 1, toolsOffered: true}
	controller.observe([]byte(`data: {"choices":[{"delta":{"content":"Done."},"finish_reason":"stop"}]}`))
	decision := controller.decide(codexContinuationBoundaryCleanEOF, nil)
	if decision.outcome != codexContinuationOutcomeComplete || decision.classification != "finish_reason_eof" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestCodexTerminalArbiterEOFWithoutTerminalFailsClosed(t *testing.T) {
	controller := &codexContinuationController{remaining: 1, toolsOffered: true}
	controller.observe([]byte(`data: {"choices":[{"delta":{"content":"Plausible but unproven final text."}}]}`))
	decision := controller.decide(codexContinuationBoundaryCleanEOF, nil)
	if decision.outcome != codexContinuationOutcomeFail || decision.classification != "eof_without_terminal" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestCodexTerminalArbiterPendingToolEOFFailsClosed(t *testing.T) {
	controller := &codexContinuationController{remaining: 1, toolsOffered: true}
	controller.observe([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"spawn_agent","arguments":"{\"task\":"}}]}}]}`))
	decision := controller.decide(codexContinuationBoundaryCleanEOF, nil)
	if decision.outcome != codexContinuationOutcomeFail || decision.classification != "eof_pending_tool" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestCodexTerminalArbiterReadErrorNeverCompletes(t *testing.T) {
	controller := &codexContinuationController{remaining: 1, toolsOffered: true}
	controller.observe([]byte(`data: {"choices":[{"delta":{"content":"Done."},"finish_reason":"stop"}]}`))
	decision := controller.decide(codexContinuationBoundaryReadError, errors.New("unexpected EOF"))
	if decision.outcome != codexContinuationOutcomeFail || decision.classification != "scanner_error" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestCodexTerminalArbiterObservesFinalMessageAndLegacyToolCalls(t *testing.T) {
	tests := []string{
		`data: {"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"spawn_agent","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[{"delta":{"function_call":{"name":"exec_command","arguments":"{}"}},"finish_reason":"function_call"}]}`,
	}
	for _, line := range tests {
		controller := &codexContinuationController{remaining: 1, toolsOffered: true}
		controller.observe([]byte(line))
		if !controller.pass.sawToolCall {
			t.Fatalf("tool call was not observed for %s", line)
		}
		if decision := controller.decide(codexContinuationBoundaryCleanEOF, nil); decision.outcome != codexContinuationOutcomeComplete {
			t.Fatalf("decision = %#v for %s", decision, line)
		}
	}
}

func TestCodexTerminalArbiterReplaysReasoningContent(t *testing.T) {
	controller := &codexContinuationController{remaining: 1, toolsOffered: true}
	controller.observe([]byte(`data: {"choices":[{"delta":{"reasoning_content":"I should call spawn_agent. "}}]}`))
	controller.observe([]byte(`data: {"choices":[{"delta":{"content":"Spawning now:"},"finish_reason":"stop"}]}`))
	decision := controller.decide(codexContinuationBoundaryExplicitDone, nil)
	if decision.outcome != codexContinuationOutcomeContinue {
		t.Fatalf("decision = %#v", decision)
	}
	body, ok := controller.continuationBody([]byte(`{"model":"qwen","messages":[{"role":"user","content":"work"}]}`))
	if !ok {
		t.Fatal("continuation body was not built")
	}
	for _, want := range []string{"I should call spawn_agent", "Spawning now", "Emit the intended tool call"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("continuation body missing %q: %s", want, body)
		}
	}
	if got := gjson.GetBytes(body, "messages.1.reasoning_content").String(); got == "" {
		t.Fatalf("reasoning_content missing from continuation body: %s", body)
	}
}

func TestCodexTerminalArbiterContinuationExhaustionFails(t *testing.T) {
	controller := &codexContinuationController{remaining: 1, toolsOffered: true}
	first := controller.decide(codexContinuationBoundaryExplicitDone, nil)
	if first.outcome != codexContinuationOutcomeContinue {
		t.Fatalf("first decision = %#v", first)
	}
	if _, ok := controller.continuationBody([]byte(`{"messages":[]}`)); !ok {
		t.Fatal("first continuation body was not built")
	}
	second := controller.decide(codexContinuationBoundaryExplicitDone, nil)
	if second.outcome != codexContinuationOutcomeFail || second.classification != "continuation_exhausted" {
		t.Fatalf("second decision = %#v", second)
	}
}
