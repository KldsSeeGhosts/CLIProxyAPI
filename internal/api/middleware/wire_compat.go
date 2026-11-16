package middleware

// Wire compatibility middleware.
//
// This middleware folds the behavior that used to live in the standalone
// cpa-responses-shim sidecar directly into the API server. Every transform is
// scoped to both the client wire format (request path) and the target model,
// so one harness's fix can never reshape another harness's payload:
//
//   - Responses (Codex): OpenCode Go does not understand Codex Desktop's
//     additional_tools input item, namespace declarations, or custom tools.
//     They are flattened to plain function tools and restored on the way back.
//   - Chat completions (Pi, OpenCode): the OpenCode Zen endpoint rejects the
//     "developer" role with "[1214] Incorrect role information"; it is
//     remapped to "system".
//   - Messages (Claude Code): OpenCode Go rejects Claude Code's map-shaped
//     context_management with "expected a sequence"; it is dropped.
//   - All wires: some OpenCode Go models reject tools without a description
//     ("function.description is required"); the tool name is filled in.
//   - Gemini: stale or foreign replay signatures in conversation history are
//     removed ("Invalid thought signature"). Tool schemas are never touched.
//
// The middleware only engages on POST requests to the Responses, Messages, and
// chat-completions endpoints. WebSocket upgrades and realtime traffic are
// never touched.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const wireCompatMaxBodyBytes = 64 << 20

// wireCompatContextKey carries the recorded OpenCode Go tool identities used to
// restore flattened declarations on the response.
type wireCompatContextKey string

const (
	openCodeIdentitiesContextKey wireCompatContextKey = "wirecompat.opencode.identities"
	toolSearchEnabledContextKey  wireCompatContextKey = "wirecompat.responses.tool_search"
)

// wireKind identifies the client API format of a request.
type wireKind int

const (
	wireKindNone wireKind = iota
	wireKindResponses
	wireKindMessages
	wireKindChat
)

// wireKindForPath matches the client-facing API endpoints by suffix so the
// same rules cover /v1/... and alias groups such as /backend-api/codex.
func wireKindForPath(path string) wireKind {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	switch {
	case strings.HasSuffix(trimmed, "/responses"):
		return wireKindResponses
	case strings.HasSuffix(trimmed, "/messages"):
		return wireKindMessages
	case strings.HasSuffix(trimmed, "/chat/completions"):
		return wireKindChat
	default:
		return wireKindNone
	}
}

// WireCompatMiddleware returns a Gin middleware that applies the request- and
// response-side wire fixes formerly performed by the cpa-responses-shim
// process. It only acts on POST requests to the Responses, Messages, and
// chat-completions endpoints; every other request passes through untouched.
func WireCompatMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !wireCompatShouldRewrite(c.Request) {
			c.Next()
			return
		}
		rewriteWireCompatRequest(c)

		wire := wireKindForPath(c.Request.URL.Path)
		var writer *wireCompatResponseWriter
		if wire == wireKindResponses {
			if identities, ok := c.Value(string(openCodeIdentitiesContextKey)).(openCodeIdentities); ok && len(identities) > 0 {
				writer = newOpenCodeCompatWriter(c.Writer, identities)
			} else if enabled, _ := c.Value(string(toolSearchEnabledContextKey)).(bool); enabled {
				writer = newToolSearchCompatWriter(c.Writer)
			}
		}
		if writer != nil {
			c.Writer = writer
			defer func() {
				writer.finish()
				c.Writer = writer.ResponseWriter
			}()
		}
		c.Next()
	}
}

func wireCompatShouldRewrite(r *http.Request) bool {
	return r != nil && r.Method == http.MethodPost && wireKindForPath(r.URL.Path) != wireKindNone
}

// rewriteWireCompatRequest buffers, transforms, and re-installs the request
// body. It also records the context flags that gate response-side restores.
//
// The middleware never fails the request: read errors, oversized bodies, and
// invalid JSON are passed through untouched so the handlers keep producing
// their normal error responses (for example the body-size limiter's 413).
func rewriteWireCompatRequest(c *gin.Context) {
	r := c.Request
	if r.Body == nil {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, wireCompatMaxBodyBytes+1))
	if err != nil {
		// Replay the consumed bytes and the original read error. Some readers
		// return an error only once, so reading the remainder could hide it.
		r.Body = &wireCompatReplayBody{
			Reader: io.MultiReader(bytes.NewReader(body), wireCompatErrorReader{err}),
			Closer: r.Body,
		}
		return
	}
	if int64(len(body)) > wireCompatMaxBodyBytes || !json.Valid(body) {
		// A valid handler may accept a larger body than this transform does.
		// Replay the prefix and remaining stream without changing its framing.
		r.Body = &wireCompatReplayBody{
			Reader: io.MultiReader(bytes.NewReader(body), r.Body),
			Closer: r.Body,
		}
		return
	}
	wire := wireKindForPath(r.URL.Path)
	if wire == wireKindResponses {
		var original map[string]any
		if json.Unmarshal(body, &original) == nil {
			if isOpenCodeModel(wireCompatStringValue(original["model"])) {
				identities := openCodeToolIdentities(original)
				ctx := context.WithValue(r.Context(), string(openCodeIdentitiesContextKey), identities)
				c.Request = r.WithContext(ctx)
				c.Set(string(openCodeIdentitiesContextKey), identities)
			} else if declaresClientToolSearch(original) {
				ctx := context.WithValue(r.Context(), string(toolSearchEnabledContextKey), true)
				c.Request = r.WithContext(ctx)
				c.Set(string(toolSearchEnabledContextKey), true)
			}
		}
	}
	rewritten, changed, err := rewriteWireCompatRequestJSON(wire, body)
	r = c.Request
	if err != nil || !changed {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	r.Header.Del("Transfer-Encoding")
}

type wireCompatReplayBody struct {
	io.Reader
	io.Closer
}

type wireCompatErrorReader struct{ err error }

func (r wireCompatErrorReader) Read([]byte) (int, error) { return 0, r.err }

// rewriteWireCompatRequestJSON rewrites only the (wire, model) combinations
// that need compatibility help. The bool reports whether the serialized
// payload changed.
func rewriteWireCompatRequestJSON(w wireKind, body []byte) ([]byte, bool, error) {
	if !json.Valid(body) {
		return nil, false, errors.New("request body is not valid JSON")
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, false, err
	}
	model := strings.TrimSpace(wireCompatStringValue(root["model"]))

	changed := false
	if isWireCompatGeminiModel(model) {
		changed = sanitizeGeminiReplaySignatures(root) || changed
	}
	if w == wireKindResponses && !isOpenCodeModel(model) && emulatesToolSearch(model) && declaresClientToolSearch(root) {
		changed = emulateToolSearchRequest(root) || changed
	}
	if isOpenCodeModel(model) {
		switch w {
		case wireKindResponses:
			changed = flattenOpenCodeReplay(root, openCodeToolIdentities(root)) || changed
			changed = flattenOpenCodeTools(root) || changed
		case wireKindChat:
			changed = normalizeOpenCodeChatRoles(root) || changed
		}
		changed = sanitizeOpenCodeContextManagement(root) || changed
		changed = fillToolDescriptions(w, root) || changed
	}
	if !changed {
		return body, false, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func isWireCompatGeminiModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "gemini-3.8-flash") || model == "gemini-4-argon" || strings.HasPrefix(model, "gemini-4-argon-")
}

// openCodeBareAliases are unprefixed config aliases routed to OpenCode Go.
var openCodeBareAliases = map[string]struct{}{
	"omen-alpha":          {},
	"deepseek-flash":      {},
	"deepseek-v4.1-flash": {},
}

// isOpenCodeModel reports whether a model routes to the OpenCode Go (Zen)
// upstream. Cursor-hosted models that merely share a family name (for example
// cursor/muse-spark-1.3-high) go through the Cursor plugin and are excluded.
func isOpenCodeModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || strings.HasPrefix(model, "cursor/") {
		return false
	}
	if strings.HasPrefix(model, "opencode-go/") || strings.HasPrefix(model, "opencode/") {
		return true
	}
	_, ok := openCodeBareAliases[model]
	return ok
}

// fillToolDescriptions gives every client-defined function tool a non-empty
// description. Some OpenCode Go models reject tools without one with
// "function.description is required". Anthropic server tools (web_search,
// bash, text_editor, ...) carry no input_schema and are left alone.
func fillToolDescriptions(w wireKind, root map[string]any) bool {
	changed := false
	for _, value := range rawToolSlice(root["tools"]) {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		target := tool
		switch w {
		case wireKindChat:
			function, ok := tool["function"].(map[string]any)
			if !ok {
				continue
			}
			target = function
		case wireKindMessages:
			if _, ok := tool["input_schema"]; !ok {
				continue
			}
		case wireKindResponses:
			if kind := wireCompatStringValue(tool["type"]); kind != "" && kind != "function" {
				continue
			}
		}
		name := strings.TrimSpace(wireCompatStringValue(target["name"]))
		if name == "" || strings.TrimSpace(wireCompatStringValue(target["description"])) != "" {
			continue
		}
		target["description"] = name
		changed = true
	}
	return changed
}

// normalizeOpenCodeChatRoles remaps the OpenAI "developer" message role to
// "system" for OpenCode Go chat-completions requests. The Zen endpoint
// strictly validates roles and rejects "developer" with
// "[1214] Incorrect role information". "system" carries the same
// semantics and is accepted by every upstream.
func normalizeOpenCodeChatRoles(root map[string]any) bool {
	messages, ok := root["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(wireCompatStringValue(msg["role"])), "developer") {
			msg["role"] = "system"
			changed = true
		}
	}
	return changed
}

// sanitizeOpenCodeContextManagement removes map-shaped context_management
// objects (such as Claude Code's {"edits":[...]}) which strict third-party
// upstreams like OpenCode Go reject with "invalid type: map, expected a
// sequence".
func sanitizeOpenCodeContextManagement(root map[string]any) bool {
	if cm, exists := root["context_management"]; exists {
		if _, isMap := cm.(map[string]any); isMap {
			delete(root, "context_management")
			return true
		}
	}
	return false
}

// sanitizeGeminiReplaySignatures keeps CPA's typed Gemini carriers on
// top-level reasoning items. The backend validates their envelope, provider,
// and semantic target before replay. Raw foreign signatures in conversation
// history (Responses input, Messages or Chat messages) are removed. Tool
// schemas and other request fields are never touched: a tool parameter may
// legitimately be named encrypted_content.
func sanitizeGeminiReplaySignatures(root map[string]any) bool {
	type carrier struct {
		item  map[string]any
		value string
	}
	var preserved []carrier
	if input, ok := root["input"].([]any); ok {
		for _, value := range input {
			item, ok := value.(map[string]any)
			if !ok || item["type"] != "reasoning" {
				continue
			}
			if value, ok := item["encrypted_content"].(string); ok && strings.HasPrefix(value, "cpa-gemini-responses-carrier-v1:") {
				preserved = append(preserved, carrier{item, value})
				delete(item, "encrypted_content")
			}
		}
	}
	changed := false
	for _, key := range []string{"input", "messages"} {
		if history, ok := root[key].([]any); ok && stripGeminiReplayFields(history) {
			changed = true
		}
	}
	for _, saved := range preserved {
		saved.item["encrypted_content"] = saved.value
	}
	return changed
}

// stripGeminiReplayFields removes untyped replay/signature fields. Summaries,
// messages, function calls, and their outputs remain intact.
func stripGeminiReplayFields(value any) bool {
	changed := false
	switch node := value.(type) {
	case map[string]any:
		for key := range node {
			switch key {
			case "encrypted_content", "thought_signature", "thoughtSignature":
				delete(node, key)
				changed = true
			default:
				if stripGeminiReplayFields(node[key]) {
					changed = true
				}
			}
		}
	case []any:
		for _, item := range node {
			if stripGeminiReplayFields(item) {
				changed = true
			}
		}
	}
	return changed
}

func rawToolSlice(value any) []any {
	tools, _ := value.([]any)
	return tools
}

func wireCompatStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}
