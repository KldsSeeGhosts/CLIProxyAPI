// Command cpa-responses-shim keeps a custom CLIProxyAPI binary in place while
// normalizing the request shapes that strict third-party upstreams reject:
//
//   - OpenCode Go does not understand Codex Desktop's additional_tools input
//     item or namespace declarations.
//   - OpenCode Go (Zen endpoint, e.g. omen-alpha) rejects the OpenAI
//     "developer" message role with "[1214] Incorrect role information";
//     developer messages are remapped to "system", which it accepts.
//   - Older Gemini executors can replay stale/foreign encrypted_content fields
//     and receive "Invalid thought signature" from the upstream API.
//
// The shim normalizes selected provider requests and restores Muse Responses
// tool identities on the return path. Other responses and WebSocket traffic
// are forwarded unchanged to the configured backend.
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

func shouldRewrite(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost {
		return false
	}
	path := strings.TrimSuffix(strings.TrimSpace(r.URL.Path), "/")
	return path == "/v1/responses" || path == "/responses" ||
		path == "/v1/messages" || path == "/messages" ||
		path == "/v1/chat/completions" || path == "/chat/completions"
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
	if isResponsesPath(r.URL.Path) {
		var original map[string]any
		if json.Unmarshal(body, &original) == nil && isMuseModel(stringValue(original["model"])) {
			identities := museToolIdentities(original)
			*r = *r.WithContext(context.WithValue(r.Context(), museIdentityKey{}, identities))
		}
	}
	rewritten, changed, err := rewriteResponseJSON(body)
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

func isResponsesPath(path string) bool {
	trimmed := strings.TrimSuffix(strings.TrimSpace(path), "/")
	return trimmed == "/v1/responses" || trimmed == "/responses"
}

// rewriteResponseJSON rewrites only the request models that need compatibility
// help. The bool reports whether the serialized payload changed.
func rewriteResponseJSON(body []byte) ([]byte, bool, error) {
	if !json.Valid(body) {
		return nil, false, errors.New("request body is not valid JSON")
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, false, err
	}
	model, _ := root["model"].(string)
	model = strings.TrimSpace(model)

	changed := false
	if isGeminiModel(model) {
		changed = sanitizeGeminiReplay(root) || changed
	}
	if isMuseModel(model) {
		changed = sanitizeMuseContextManagement(root) || changed
		changed = flattenMuseReplay(root, museToolIdentities(root)) || changed
		changed = flattenMuseTools(root) || changed
	}
	if isOpenCodeGoChatModel(model) {
		changed = normalizeOpenCodeChatRoles(root) || changed
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

func isMuseModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "muse-spark") ||
		strings.Contains(model, "opencode-go") ||
		strings.Contains(model, "opencode")
}

func isOpenCodeGoChatModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "opencode") || strings.Contains(model, "omen")
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
// foreign signatures still pass through the legacy sanitizer.
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
	changed := stripGeminiReplayFields(root)
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
