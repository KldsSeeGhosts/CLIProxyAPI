package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
)

// antigravityEmptyResponseMaxRetries bounds how many times a transient empty
// upstream completion is retried independently of the configured request
// retry budget. The Antigravity backend intermittently returns HTTP 200 with
// zero candidates/parts (zero output and zero reasoning tokens); clients such
// as Pi treat the resulting empty "stop" completion as a legitimate end of
// the agent turn, silently halting tool loops (ERM-121).
const antigravityEmptyResponseMaxRetries = 4

// antigravityEmptyRetryDelay returns a short backoff before re-issuing a
// request whose upstream completion came back empty.
func antigravityEmptyRetryDelay(emptyRetries int) time.Duration {
	return time.Duration(emptyRetries) * 400 * time.Millisecond
}

// Gemini 3.6 intermittently ends with either an empty success or a thought-only
// STOP while intending another tool call. Blindly replaying the identical empty
// request increases load and can cool the only Antigravity credential after
// exhaustion. Convert the empty-success case into a semantic continuation STOP;
// Pi's model-specific recovery hook then starts a fresh instructed continuation.
func antigravityUsesSemanticContinuation(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gemini-3.6-flash")
}

func antigravitySemanticContinuationPayload(model string) []byte {
	modelJSON, _ := json.Marshal(model)
	return []byte(fmt.Sprintf(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"The upstream completion was empty; continue the unfinished turn."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":0,"totalTokenCount":0},"modelVersion":%s,"responseId":"cpa-semantic-continuation"}}`, modelJSON))
}

func antigravitySemanticContinuationStream(model string) []byte {
	payload := antigravitySemanticContinuationPayload(model)
	out := make([]byte, 0, len(payload)+8)
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}

// antigravityResponseHasContent reports whether an Antigravity response
// payload (streamed SSE JSON payload or a full non-streaming body, wrapped
// under "response" or bare) carries any user-visible content: text, thought
// text, function calls, inline data, or code execution parts.
//
// Responses that are explicitly blocked (promptFeedback.blockReason) or that
// end for a non-STOP reason (SAFETY, RECITATION, MAX_TOKENS, ...) are
// reported as having content so they are never mistaken for the transient
// empty-completion bug and retried.
func antigravityResponseHasContent(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if !gjson.ValidBytes(data) {
		// Unparsable payloads are not the empty-completion bug; let the
		// normal translation/error path handle them.
		return true
	}
	node := gjson.ParseBytes(data)
	if wrapped := node.Get("response"); wrapped.Exists() {
		node = wrapped
	}
	if node.Get("promptFeedback.blockReason").Exists() {
		return true
	}
	candidates := node.Get("candidates")
	if !candidates.IsArray() || len(candidates.Array()) == 0 {
		return false
	}
	for _, candidate := range candidates.Array() {
		finishReason := strings.ToLower(candidate.Get("finishReason").String())
		if finishReason != "" && finishReason != "stop" {
			return true
		}
		parts := candidate.Get("content.parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			if text := part.Get("text"); text.Exists() && text.String() != "" {
				return true
			}
			if part.Get("functionCall").Exists() || part.Get("function_call").Exists() {
				return true
			}
			if part.Get("inlineData").Exists() || part.Get("inline_data").Exists() {
				return true
			}
			if part.Get("executableCode").Exists() || part.Get("codeExecutionResult").Exists() {
				return true
			}
		}
	}
	return false
}

// peekAntigravityStreamContent scans an upstream Antigravity SSE stream until
// the first payload carrying user-visible content, buffering every raw line
// so the stream can be replayed from the beginning. It returns the buffered
// bytes and whether any content was observed. A stream that ends (EOF)
// without any content payload is the transient empty-completion case and is
// safe to retry, because nothing has been forwarded to the downstream client
// yet.
func peekAntigravityStreamContent(_ context.Context, body io.Reader) (buffered []byte, hasContent bool, err error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, streamScannerBuffer)
	var buf bytes.Buffer
	for scanner.Scan() {
		line := scanner.Bytes()
		buf.Write(line)
		buf.WriteByte('\n')
		payload := helps.JSONPayload(line)
		if payload == nil {
			continue
		}
		if antigravityResponseHasContent(payload) {
			return buf.Bytes(), true, nil
		}
	}
	return buf.Bytes(), false, scanner.Err()
}

// antigravityPrependReadCloser replays buffered bytes before the remaining
// live stream body, preserving the original Close behavior.
type antigravityPrependReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (rc *antigravityPrependReadCloser) Read(p []byte) (int, error) { return rc.reader.Read(p) }
func (rc *antigravityPrependReadCloser) Close() error               { return rc.closer.Close() }
