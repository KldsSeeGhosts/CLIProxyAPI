// Command cpa-responses-shim keeps a custom CLIProxyAPI binary in place while
// normalizing the request shapes that strict third-party upstreams reject.
// Every transform is scoped to both the client wire format (request path) and
// the target model, so one harness's fix can never reshape another harness's
// payload:
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
// Other requests, responses and WebSocket traffic are forwarded unchanged.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	defaultListen  = "127.0.0.1:8317"
	defaultBackend = "http://127.0.0.1:8318"
	maxBodyBytes   = 64 << 20
)

var (
	errBodyTooLarge = errors.New("request body exceeds shim limit")
)

type options struct {
	listen        string
	backend       string
	backendBin    string
	backendConfig string
}

func main() {
	var opts options
	flag.StringVar(&opts.listen, "listen", defaultListen, "address for the shim listener")
	flag.StringVar(&opts.backend, "backend", defaultBackend, "backend CLIProxyAPI URL")
	flag.StringVar(&opts.backendBin, "backend-bin", "", "custom CLIProxyAPI binary to run as a child")
	flag.StringVar(&opts.backendConfig, "backend-config", "", "config path passed to the child CLIProxyAPI")
	flag.Parse()

	if err := run(opts); err != nil {
		log.Fatal(err)
	}
}

func run(opts options) error {
	backendURL, err := url.Parse(strings.TrimSpace(opts.backend))
	if err != nil {
		return fmt.Errorf("parse backend URL: %w", err)
	}
	if backendURL.Scheme == "" || backendURL.Host == "" {
		return fmt.Errorf("backend URL must include scheme and host")
	}

	var child *exec.Cmd
	if strings.TrimSpace(opts.backendBin) != "" {
		args := []string{}
		if strings.TrimSpace(opts.backendConfig) != "" {
			args = append(args, "-config", opts.backendConfig)
		}
		child = exec.Command(opts.backendBin, args...)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			return fmt.Errorf("start backend: %w", err)
		}
		log.Printf("started backend pid=%d", child.Process.Pid)
	}

	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	proxy.ModifyResponse = museModifyResponse
	proxy.ErrorLog = log.New(os.Stderr, "cpa-responses-shim proxy: ", log.LstdFlags)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shouldRewrite(r) {
			if err := rewriteResponsesRequest(r); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	})

	server := &http.Server{
		Addr:              opts.listen,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s, backend %s", opts.listen, backendURL)
		errCh <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	select {
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if child != nil && child.Process != nil {
			_ = child.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- child.Wait() }()
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				_ = child.Process.Kill()
				<-done
			}
		}
		return nil
	case err := <-errCh:
		if child != nil && child.Process != nil {
			_ = child.Process.Signal(syscall.SIGTERM)
			_, _ = child.Process.Wait()
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// wire identifies the client API format of a request.
type wire int

const (
	wireNone wire = iota
	wireResponses
	wireMessages
	wireChat
)

func wireForPath(path string) wire {
	switch strings.TrimSuffix(strings.TrimSpace(path), "/") {
	case "/v1/responses", "/responses":
		return wireResponses
	case "/v1/messages", "/messages":
		return wireMessages
	case "/v1/chat/completions", "/chat/completions":
		return wireChat
	default:
		return wireNone
	}
}

func shouldRewrite(r *http.Request) bool {
	return r != nil && r.Method == http.MethodPost && wireForPath(r.URL.Path) != wireNone
}

func rewriteResponsesRequest(r *http.Request) error {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if int64(len(body)) > maxBodyBytes {
		return errBodyTooLarge
	}
	wire := wireForPath(r.URL.Path)
	if wire == wireResponses {
		var original map[string]any
		if json.Unmarshal(body, &original) == nil && isOpenCodeModel(stringValue(original["model"])) {
			identities := museToolIdentities(original)
			*r = *r.WithContext(context.WithValue(r.Context(), museIdentityKey{}, identities))
		}
	}
	rewritten, changed, err := rewriteRequestJSON(wire, body)
	if err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	if !changed {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	r.Header.Del("Transfer-Encoding")
	return nil
}

// rewriteRequestJSON rewrites only the (wire, model) combinations that need
// compatibility help. The bool reports whether the serialized payload changed.
func rewriteRequestJSON(w wire, body []byte) ([]byte, bool, error) {
	if !json.Valid(body) {
		return nil, false, errors.New("request body is not valid JSON")
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, false, err
	}
	model := strings.TrimSpace(stringValue(root["model"]))

	changed := false
	if isGeminiModel(model) {
		changed = sanitizeGeminiReplay(root) || changed
	}
	if isOpenCodeModel(model) {
		switch w {
		case wireResponses:
			changed = flattenMuseReplay(root, museToolIdentities(root)) || changed
			changed = flattenMuseTools(root) || changed
		case wireChat:
			changed = normalizeOpenCodeChatRoles(root) || changed
		}
		changed = sanitizeMuseContextManagement(root) || changed
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

func isGeminiModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "gemini-3.8-flash")
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
	if strings.Contains(model, "muse-spark") {
		return true
	}
	_, ok := openCodeBareAliases[model]
	return ok
}

// fillToolDescriptions gives every client-defined function tool a non-empty
// description. Some OpenCode Go models reject tools without one with
// "function.description is required". Anthropic server tools (web_search,
// bash, text_editor, ...) carry no input_schema and are left alone.
func fillToolDescriptions(w wire, root map[string]any) bool {
	changed := false
	for _, value := range rawToolSlice(root["tools"]) {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		target := tool
		switch w {
		case wireChat:
			function, ok := tool["function"].(map[string]any)
			if !ok {
				continue
			}
			target = function
		case wireMessages:
			if _, ok := tool["input_schema"]; !ok {
				continue
			}
		case wireResponses:
			if kind := stringValue(tool["type"]); kind != "" && kind != "function" {
				continue
			}
		}
		name := strings.TrimSpace(stringValue(target["name"]))
		if name == "" || strings.TrimSpace(stringValue(target["description"])) != "" {
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
		if strings.EqualFold(strings.TrimSpace(stringValue(msg["role"])), "developer") {
			msg["role"] = "system"
			changed = true
		}
	}
	return changed
}

// sanitizeMuseContextManagement removes map-shaped context_management objects
// (such as Claude Code's {"edits":[...]}) which strict third-party upstreams
// like OpenCode Go reject with "invalid type: map, expected a sequence".
func sanitizeMuseContextManagement(root map[string]any) bool {
	if cm, exists := root["context_management"]; exists {
		if _, isMap := cm.(map[string]any); isMap {
			delete(root, "context_management")
			return true
		}
	}
	return false
}

// Keep CPA's typed Gemini carriers on top-level reasoning items. The backend
// validates their envelope, provider, and semantic target before replay. Raw
// foreign signatures in conversation history (Responses input, Messages or
// Chat messages) are removed. Tool schemas and other request fields are never
// touched: a tool parameter may legitimately be named encrypted_content.
func sanitizeGeminiReplay(root map[string]any) bool {
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

func flattenMuseTools(root map[string]any) bool {
	input, hasInputArray := root["input"].([]any)

	topTools := rawToolSlice(root["tools"])
	merged := make([]any, 0, len(topTools))
	seen := make(map[string]struct{}, len(topTools))
	for _, tool := range topTools {
		for _, flattened := range flattenMuseTool(tool) {
			normalized, name, ok := normalizeMuseTool(flattened, "")
			if !ok {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			merged = append(merged, normalized)
		}
	}

	rewrittenInput := make([]any, 0, len(input))
	beforeTools, _ := json.Marshal(root["tools"])
	afterTools, _ := json.Marshal(merged)
	changed := !bytes.Equal(beforeTools, afterTools) && len(topTools) > 0
	for _, item := range input {
		itemMap, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(stringValue(itemMap["type"])) != "additional_tools" {
			rewrittenInput = append(rewrittenInput, item)
			continue
		}
		changed = true
		for _, tool := range rawToolSlice(itemMap["tools"]) {
			for _, flattened := range flattenMuseTool(tool) {
				name := stringValue(flattened.(map[string]any)["name"])
				if name == "" {
					continue
				}
				if _, exists := seen[name]; exists {
					continue
				}
				seen[name] = struct{}{}
				merged = append(merged, flattened)
			}
		}
	}
	if !changed {
		return false
	}
	if hasInputArray {
		root["input"] = rewrittenInput
	}
	if len(merged) == 0 {
		delete(root, "tools")
	} else {
		root["tools"] = merged
	}
	return true
}

func rawToolSlice(value any) []any {
	tools, _ := value.([]any)
	return tools
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func flattenMuseTool(tool any) []any {
	toolMap, ok := tool.(map[string]any)
	if !ok {
		return nil
	}
	toolType := strings.ToLower(strings.TrimSpace(stringValue(toolMap["type"])))
	if toolType == "tool_search" || strings.EqualFold(strings.TrimSpace(stringValue(toolMap["name"])), "tool_search") {
		return nil
	}
	if toolType == "namespace" {
		namespace := strings.TrimSpace(stringValue(toolMap["name"]))
		if strings.EqualFold(namespace, "tool_search") {
			return nil
		}
		out := make([]any, 0)
		for _, child := range rawToolSlice(toolMap["tools"]) {
			normalized, _, ok := normalizeMuseTool(child, namespace)
			if ok {
				out = append(out, normalized)
			}
		}
		return out
	}
	normalized, _, ok := normalizeMuseTool(tool, "")
	if !ok {
		return nil
	}
	return []any{normalized}
}

func normalizeMuseTool(tool any, namespace string) (map[string]any, string, bool) {
	toolMap, ok := tool.(map[string]any)
	if !ok {
		return nil, "", false
	}
	toolType := strings.ToLower(strings.TrimSpace(stringValue(toolMap["type"])))
	if toolType == "tool_search" || strings.EqualFold(strings.TrimSpace(stringValue(toolMap["name"])), "tool_search") {
		return nil, "", false
	}
	name := strings.TrimSpace(stringValue(toolMap["name"]))
	if name == "" {
		if function, ok := toolMap["function"].(map[string]any); ok {
			name = strings.TrimSpace(stringValue(function["name"]))
		}
	}
	if name == "" {
		return nil, "", false
	}
	if namespace != "" && !strings.HasPrefix(name, "mcp__") &&
		!strings.HasPrefix(name, namespace+"__") && name != namespace {
		name = namespace + "__" + name
	}

	out := make(map[string]any)
	switch toolType {
	case "", "function":
		out["type"] = "function"
		for _, key := range []string{"description", "parameters", "strict"} {
			if value, exists := toolMap[key]; exists {
				out[key] = value
			}
		}
		if _, exists := out["parameters"]; !exists {
			for _, key := range []string{"parametersJsonSchema", "input_schema"} {
				if value, found := toolMap[key]; found {
					out["parameters"] = value
					break
				}
			}
		}
		if function, ok := toolMap["function"].(map[string]any); ok {
			for _, key := range []string{"description", "parameters", "strict"} {
				if _, exists := out[key]; exists {
					continue
				}
				if value, found := function[key]; found {
					out[key] = value
				}
			}
			if _, exists := out["parameters"]; !exists {
				if value, found := function["parametersJsonSchema"]; found {
					out["parameters"] = value
				}
			}
		}
	case "custom":
		out["type"] = "function"
		if value, exists := toolMap["description"]; exists {
			out["description"] = value
		}
		out["parameters"] = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string"},
			},
			"required": []any{"input"},
		}
	default:
		return nil, "", false
	}
	out["name"] = name
	return out, name, true
}
