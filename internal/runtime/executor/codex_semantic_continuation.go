package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexSemanticContinuationMaxText = 32 * 1024

const codexContinuationEmptyTurnInstruction = "Your previous reply was empty. Continue the unfinished turn now: if the next action is a tool call, emit it as an actual tool call with no surrounding prose."
const codexContinuationAnnouncedCallInstruction = "You announced a tool call in prose but did not emit one. Emit the intended tool call now as an actual tool call, with no surrounding prose."

var codexContinuationAnnouncementVerbs = []string{
	"spawn", "wait_agent", "list_agents", "send_input", "close_agent",
	"exec_command", "write_stdin", "update_plan", "apply_patch",
	"launch", "tool call", "tool_call",
}

var codexContinuationAnnouncementSuffixes = []string{":", "：", "—", "–", "-", "…", "..."}

// Strong suffixes mark prose that trails off expecting immediate follow-through.
// A bare strong suffix is still not enough ("Here are the results:" stays a
// legitimate ending), but when the trailing clause also narrates an in-flight
// action ("Committing X:", "Running the parent gate...") the turn stalled
// mid-intent even though no whitelisted tool verb appears in the prose.
var codexContinuationStrongSuffixes = []string{":", "："}

var codexContinuationTrailingActionPattern = regexp.MustCompile(`\b[a-z]+ing\b`)

type codexContinuationBoundary uint8

const (
	codexContinuationBoundaryExplicitDone codexContinuationBoundary = iota + 1
	codexContinuationBoundaryCleanEOF
	codexContinuationBoundaryReadError
)

func (b codexContinuationBoundary) String() string {
	switch b {
	case codexContinuationBoundaryExplicitDone:
		return "explicit_done"
	case codexContinuationBoundaryCleanEOF:
		return "clean_eof"
	case codexContinuationBoundaryReadError:
		return "read_error"
	default:
		return "unknown"
	}
}

type codexContinuationOutcome uint8

const (
	codexContinuationOutcomeComplete codexContinuationOutcome = iota + 1
	codexContinuationOutcomeContinue
	codexContinuationOutcomeFail
)

func (o codexContinuationOutcome) String() string {
	switch o {
	case codexContinuationOutcomeComplete:
		return "complete"
	case codexContinuationOutcomeContinue:
		return "continue"
	case codexContinuationOutcomeFail:
		return "fail"
	default:
		return "unknown"
	}
}

type codexContinuationDecision struct {
	outcome        codexContinuationOutcome
	classification string
	instruction    string
	err            error
}

type codexTerminalIntegrityError struct {
	classification string
	cause          error
}

func (e *codexTerminalIntegrityError) Error() string {
	return fmt.Sprintf("codex terminal integrity [%s]: %v", e.classification, e.cause)
}

func (e *codexTerminalIntegrityError) Unwrap() error { return e.cause }

func newCodexTerminalIntegrityError(classification, message string) error {
	return &codexTerminalIntegrityError{classification: classification, cause: errors.New(message)}
}

type codexContinuationPass struct {
	sawToolCall       bool
	finishReason      string
	text              strings.Builder
	reasoning         strings.Builder
	textOverflow      bool
	reasoningOverflow bool
}

func (p *codexContinuationPass) observe(line []byte) {
	payload := helps.JSONPayload(line)
	if payload == nil {
		return
	}
	choices := gjson.GetBytes(payload, "choices")
	if !choices.IsArray() {
		return
	}
	for _, choice := range choices.Array() {
		p.observeMessage(choice.Get("delta"))
		p.observeMessage(choice.Get("message"))
		if fr := choice.Get("finish_reason"); fr.Exists() && fr.String() != "" {
			p.finishReason = fr.String()
		}
	}
}

func (p *codexContinuationPass) observeMessage(message gjson.Result) {
	if !message.Exists() || !message.IsObject() {
		return
	}
	p.appendString(&p.text, message.Get("content"), &p.textOverflow)
	p.appendString(&p.reasoning, message.Get("reasoning_content"), &p.reasoningOverflow)
	p.appendString(&p.reasoning, message.Get("reasoning"), &p.reasoningOverflow)
	if toolCalls := message.Get("tool_calls"); toolCalls.IsArray() && len(toolCalls.Array()) > 0 {
		p.sawToolCall = true
	}
	if functionCall := message.Get("function_call"); functionCall.Exists() && functionCall.Raw != "null" && functionCall.Raw != "{}" {
		p.sawToolCall = true
	}
}

func (p *codexContinuationPass) appendString(dst *strings.Builder, value gjson.Result, overflow *bool) {
	if *overflow || value.Type != gjson.String || value.String() == "" {
		return
	}
	remaining := codexSemanticContinuationMaxText - dst.Len()
	if remaining <= 0 {
		*overflow = true
		return
	}
	text := value.String()
	if len(text) > remaining {
		dst.WriteString(text[:remaining])
		*overflow = true
		return
	}
	dst.WriteString(text)
}

func (p *codexContinuationPass) stall(toolsOffered bool, toolNameSets ...[]string) (instruction, classification string, ok bool) {
	if p.sawToolCall || p.textOverflow || !toolsOffered {
		return "", "", false
	}
	switch strings.ToLower(strings.TrimSpace(p.finishReason)) {
	case "", "stop", "tool_calls", "function_call", "tool_call":
	default:
		return "", "", false
	}
	var toolNames []string
	if len(toolNameSets) > 0 {
		toolNames = toolNameSets[0]
	}
	text := strings.TrimSpace(p.text.String())
	if text == "" {
		return codexContinuationEmptyTurnInstruction, "empty_no_tool", true
	}
	if codexContinuationAnnouncementLike(text, toolNames) || codexContinuationToolFinishWithoutCall(p.finishReason) {
		return codexContinuationAnnouncedCallInstruction, "narrated_no_tool", true
	}
	return "", "", false
}

func (p *codexContinuationPass) instruction(toolsOffered bool) (instruction string, ok bool) {
	instruction, _, ok = p.stall(toolsOffered)
	return instruction, ok
}

func codexContinuationToolFinishWithoutCall(finishReason string) bool {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "tool_calls", "function_call", "tool_call":
		return true
	default:
		return false
	}
}

func codexContinuationAnnouncementLike(text string, toolNames ...[]string) bool {
	text = strings.TrimSpace(text)
	suffixed := false
	for _, suffix := range codexContinuationAnnouncementSuffixes {
		if strings.HasSuffix(text, suffix) {
			suffixed = true
			break
		}
	}
	if !suffixed {
		return false
	}
	strongSuffix := false
	for _, suffix := range codexContinuationStrongSuffixes {
		if strings.HasSuffix(text, suffix) {
			strongSuffix = true
			break
		}
	}
	lower := strings.ToLower(text)
	for _, verb := range codexContinuationAnnouncementVerbs {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	if len(toolNames) > 0 {
		for _, name := range toolNames[0] {
			if name != "" && strings.Contains(lower, strings.ToLower(name)) {
				return true
			}
		}
	}
	if strongSuffix {
		return codexContinuationTrailingActionPattern.MatchString(codexContinuationTrailingClause(lower))
	}
	return false
}

// codexContinuationTrailingClause returns the text after the last sentence
// terminator so participle detection judges only the trailing announcement,
// not earlier prose in the same message.
func codexContinuationTrailingClause(lower string) string {
	idx := strings.LastIndexAny(lower, ".!?\n")
	if idx < 0 {
		return lower
	}
	return lower[idx+1:]
}

type codexContinuationController struct {
	remaining    int
	toolsOffered bool
	toolNames    []string
	pass         codexContinuationPass
	passIndex    int
}

func newCodexContinuationController(ctx context.Context, headers http.Header, cfg *config.Config, model string, responseFormat sdktranslator.Format, translatedBody []byte) (controller *codexContinuationController, ok bool) {
	if cfg == nil || !cfg.Codex.SemanticContinuation.Enabled {
		return nil, false
	}
	if responseFormat != sdktranslator.FormatOpenAIResponse {
		return nil, false
	}
	if !helps.IsCodexClientRequest(ctx, headers) {
		return nil, false
	}
	matched := false
	for _, pattern := range cfg.Codex.SemanticContinuation.Models {
		if helps.MatchModelPattern(pattern, model) {
			matched = true
			break
		}
	}
	if !matched {
		return nil, false
	}
	tools := gjson.GetBytes(translatedBody, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return nil, false
	}
	toolNames := make([]string, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		name := tool.Get("function.name").String()
		if name == "" {
			name = tool.Get("name").String()
		}
		if name != "" {
			toolNames = append(toolNames, name)
		}
	}
	maxContinuations := cfg.Codex.SemanticContinuation.MaxContinuations
	if maxContinuations <= 0 {
		maxContinuations = 1
	}
	return &codexContinuationController{remaining: maxContinuations, toolsOffered: true, toolNames: toolNames}, true
}

func (c *codexContinuationController) observe(line []byte) { c.pass.observe(line) }

func isCodexUpstreamDoneLine(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	return bytes.Equal(trimmed, []byte("[DONE]"))
}

func (c *codexContinuationController) isUpstreamDoneLine(line []byte) bool {
	return isCodexUpstreamDoneLine(line)
}

// decide is called at every pass boundary, including clean EOF and read error.
// A synthetic downstream terminal event is allowed only after this method has
// classified the pass as complete.
func (c *codexContinuationController) decide(boundary codexContinuationBoundary, readErr error) codexContinuationDecision {
	if boundary == codexContinuationBoundaryReadError || readErr != nil {
		if readErr == nil {
			readErr = errors.New("upstream stream read failed")
		}
		return codexContinuationDecision{
			outcome:        codexContinuationOutcomeFail,
			classification: "scanner_error",
			err: &codexTerminalIntegrityError{
				classification: "scanner_error",
				cause:          fmt.Errorf("upstream stream ended with a read error before a proven terminal boundary: %w", readErr),
			},
		}
	}

	if instruction, classification, stalled := c.pass.stall(c.toolsOffered, c.toolNames); stalled {
		if c.remaining > 0 {
			return codexContinuationDecision{
				outcome:        codexContinuationOutcomeContinue,
				classification: classification,
				instruction:    instruction,
			}
		}
		return codexContinuationDecision{
			outcome:        codexContinuationOutcomeFail,
			classification: "continuation_exhausted",
			err: newCodexTerminalIntegrityError(
				"continuation_exhausted",
				fmt.Sprintf("stalled turn remained unresolved after %d continuation pass(es)", c.passIndex),
			),
		}
	}

	if boundary == codexContinuationBoundaryExplicitDone {
		return codexContinuationDecision{outcome: codexContinuationOutcomeComplete, classification: "explicit_done"}
	}
	if c.pass.finishReason != "" {
		return codexContinuationDecision{outcome: codexContinuationOutcomeComplete, classification: "finish_reason_eof"}
	}
	if c.pass.sawToolCall {
		return codexContinuationDecision{
			outcome:        codexContinuationOutcomeFail,
			classification: "eof_pending_tool",
			err: newCodexTerminalIntegrityError(
				"eof_pending_tool",
				"upstream ended after tool-call output without [DONE] or finish_reason; refusing to fabricate tool completion",
			),
		}
	}
	return codexContinuationDecision{
		outcome:        codexContinuationOutcomeFail,
		classification: "eof_without_terminal",
		err: newCodexTerminalIntegrityError(
			"eof_without_terminal",
			"upstream ended without [DONE] or finish_reason; refusing to synthesize response.completed",
		),
	}
}

// shouldContinue remains for focused unit tests and callers outside the two
// hardened stream loops; new code should use decide at the actual boundary.
func (c *codexContinuationController) shouldContinue() bool {
	decision := c.decide(codexContinuationBoundaryExplicitDone, nil)
	return decision.outcome == codexContinuationOutcomeContinue
}

func (c *codexContinuationController) continuationBody(originalBody []byte) ([]byte, bool) {
	instruction, _, ok := c.pass.stall(c.toolsOffered, c.toolNames)
	if !ok || c.remaining <= 0 {
		return nil, false
	}
	c.remaining--
	text := strings.TrimSpace(c.pass.text.String())
	reasoning := strings.TrimSpace(c.pass.reasoning.String())
	out := originalBody
	if text != "" || reasoning != "" {
		assistant := []byte(`{"role":"assistant","content":""}`)
		assistant, _ = sjson.SetBytes(assistant, "content", text)
		if reasoning != "" {
			assistant, _ = sjson.SetBytes(assistant, "reasoning_content", reasoning)
		}
		out, _ = sjson.SetRawBytes(out, "messages.-1", assistant)
	}
	user := []byte(`{"role":"user","content":""}`)
	user, _ = sjson.SetBytes(user, "content", instruction)
	out, _ = sjson.SetRawBytes(out, "messages.-1", user)
	c.pass = codexContinuationPass{}
	c.passIndex++
	return out, true
}

func (c *codexContinuationController) offsetLine(line []byte) []byte {
	if c.passIndex == 0 {
		return line
	}
	payload := helps.JSONPayload(line)
	if payload == nil {
		return line
	}
	choices := gjson.GetBytes(payload, "choices")
	if !choices.IsArray() {
		return line
	}
	updated := payload
	for i, choice := range choices.Array() {
		index := choice.Get("index")
		if !index.Exists() {
			continue
		}
		updated, _ = sjson.SetBytes(updated, fmt.Sprintf("choices.%d.index", i), index.Int()+int64(c.passIndex))
	}
	if bytes.Equal(updated, payload) {
		return line
	}
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}
