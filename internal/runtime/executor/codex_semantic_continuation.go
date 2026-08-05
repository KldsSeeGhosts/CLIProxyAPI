package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexSemanticContinuationMaxText bounds the assistant text accumulated per
// pass. A model that produced more than this wrote a full reply, not a stalled
// announcement; continuation is disabled for that pass.
const codexSemanticContinuationMaxText = 32 * 1024

// codexContinuationEmptyTurnInstruction is sent when the model ended its turn
// with neither visible text nor a tool call while tools were offered.
const codexContinuationEmptyTurnInstruction = "Your previous reply was empty. Continue the unfinished turn now: if the next action is a tool call, emit it as an actual tool call with no surrounding prose."

// codexContinuationAnnouncedCallInstruction is sent when the model narrated a
// tool call in prose (for example "Spawning the final task:") but never
// emitted the call, which makes official Codex clients end the turn.
const codexContinuationAnnouncedCallInstruction = "You announced a tool call in prose but did not emit one. Emit the intended tool call now as an actual tool call, with no surrounding prose."

// codexContinuationAnnouncementVerbs are the Codex tool-action verbs that,
// combined with a trailing announcement suffix, mark prose as a narrated tool
// call rather than a legitimate final answer.
var codexContinuationAnnouncementVerbs = []string{
	"spawn", "wait_agent", "list_agents", "send_input", "close_agent",
	"exec_command", "write_stdin", "update_plan", "tool call", "tool_call",
}

// codexContinuationAnnouncementSuffixes are the trailing characters typical of
// a prose tool-call announcement ("Spawning the final task:", "Running the
// tests —"). Matched against the trimmed reply text.
var codexContinuationAnnouncementSuffixes = []string{":", "：", "—", "–", "-"}

// codexContinuationPass tracks one upstream Chat Completions pass for the
// continuation decision.
type codexContinuationPass struct {
	sawToolCall  bool
	finishReason string
	text         strings.Builder
	overflow     bool
}

// observe folds one raw upstream SSE line into the pass state.
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
		delta := choice.Get("delta")
		if content := delta.Get("content"); content.Exists() && content.String() != "" {
			if p.text.Len() >= codexSemanticContinuationMaxText {
				p.overflow = true
			} else {
				p.text.WriteString(content.String())
			}
		}
		if tcs := delta.Get("tool_calls"); tcs.Exists() && tcs.IsArray() && len(tcs.Array()) > 0 {
			p.sawToolCall = true
		}
		if fr := choice.Get("finish_reason"); fr.Exists() && fr.String() != "" {
			p.finishReason = fr.String()
		}
	}
}

// instruction returns the continuation instruction when this pass qualifies,
// or ok=false when the turn ended legitimately.
func (p *codexContinuationPass) instruction(toolsOffered bool) (instruction string, ok bool) {
	if p.sawToolCall || p.overflow || !toolsOffered {
		return "", false
	}
	switch p.finishReason {
	case "", "stop":
	default:
		// length / content_filter / tool_calls endings are real outcomes the
		// client must see unchanged.
		return "", false
	}
	text := strings.TrimSpace(p.text.String())
	if text == "" {
		return codexContinuationEmptyTurnInstruction, true
	}
	if codexContinuationAnnouncementLike(text) {
		return codexContinuationAnnouncedCallInstruction, true
	}
	return "", false
}

// codexContinuationAnnouncementLike reports whether a prose-only reply reads
// as a narrated tool call: a Codex tool-action verb plus a trailing
// announcement suffix.
func codexContinuationAnnouncementLike(text string) bool {
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
	lower := strings.ToLower(text)
	for _, verb := range codexContinuationAnnouncementVerbs {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	return false
}

// codexContinuationController drives server-side semantic continuation for one
// downstream Codex turn. Non-GPT models behind Chat Completions translation
// intermittently end a turn with an empty success or with prose announcing a
// tool call instead of emitting it; official Codex clients treat both as a
// completed turn and silently halt the agent loop. When the route opts in, the
// controller detects the stall at the upstream terminal marker, issues a
// bounded instructed continuation upstream, and the executor folds the extra
// pass into the same downstream Responses stream before the single terminal
// event is emitted.
type codexContinuationController struct {
	remaining    int
	toolsOffered bool
	pass         codexContinuationPass
	passIndex    int
}

// newCodexContinuationController builds the controller for one downstream
// stream. ok=false disables continuation entirely: feature off, route not
// opted in, non-Codex client, non-Responses surface, or a request without
// tools (a plain chat turn has nothing to continue toward).
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
	maxContinuations := cfg.Codex.SemanticContinuation.MaxContinuations
	if maxContinuations <= 0 {
		maxContinuations = 1
	}
	return &codexContinuationController{
		remaining:    maxContinuations,
		toolsOffered: true,
	}, true
}

// observe folds one raw upstream SSE line into the current pass.
func (c *codexContinuationController) observe(line []byte) {
	c.pass.observe(line)
}

// isUpstreamDoneLine reports whether the raw line is the upstream terminal
// [DONE] marker. The executor must consult shouldContinue before forwarding
// this line to the translator, because translating it emits the downstream
// terminal Responses event exactly once. (helps.JSONPayload cannot be used
// here: it intentionally returns nil for the non-JSON [DONE] sentinel.)
func (c *codexContinuationController) isUpstreamDoneLine(line []byte) bool {
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	return bytes.Equal(trimmed, []byte("[DONE]"))
}

// shouldContinue reports whether the just-finished pass stalled and a
// continuation round trip remains.
func (c *codexContinuationController) shouldContinue() bool {
	if c.remaining <= 0 {
		return false
	}
	_, ok := c.pass.instruction(c.toolsOffered)
	return ok
}

// continuationBody builds the next upstream request: the original translated
// Chat Completions body plus the assistant's stalled prose (when any) and an
// instructed user message. The second return value is false when no
// continuation should be attempted.
func (c *codexContinuationController) continuationBody(originalBody []byte) ([]byte, bool) {
	instruction, ok := c.pass.instruction(c.toolsOffered)
	if !ok || c.remaining <= 0 {
		return nil, false
	}
	c.remaining--
	text := strings.TrimSpace(c.pass.text.String())
	out := originalBody
	if text != "" {
		assistant := []byte(`{"role":"assistant","content":""}`)
		assistant, _ = sjson.SetBytes(assistant, "content", text)
		out, _ = sjson.SetRawBytes(out, "messages.-1", assistant)
	}
	user := []byte(`{"role":"user","content":""}`)
	user, _ = sjson.SetBytes(user, "content", instruction)
	out, _ = sjson.SetRawBytes(out, "messages.-1", user)
	c.pass = codexContinuationPass{}
	c.passIndex++
	return out, true
}

// offsetLine rewrites the choice indices of one upstream chunk for the current
// pass. The Responses translator keys message and tool-call items by upstream
// choice index; without an offset, a continuation pass's choice 0 would reuse
// the first pass's already-completed output item. Pass 0 needs no rewrite.
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
