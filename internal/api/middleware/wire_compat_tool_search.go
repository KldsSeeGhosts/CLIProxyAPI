package middleware

// Codex (0.160+) defers MCP tools behind a native Responses `tool_search` tool
// whose execution is "client": the model emits a `tool_search_call` item,
// Codex runs the BM25 search locally and replays a `tool_search_output` item
// that carries the discovered tool definitions.
//
// Routes that are not wired for that protocol break it in two ways:
//
//   - OpenAI-native Responses Lite routes (gpt-6-luna) understand the tool and
//     the replayed items, but answer with a plain function_call named
//     tool_search_tool in the reserved "tool_search" namespace. Codex has no
//     handler for that and replies "unsupported call". The response is
//     restored to a tool_search_call item. Requests pass through untouched.
//   - Routes translated by the backend (Gemini) never see the tool at all.
//     tool_search is declared to the model as an ordinary function, the
//     model's call is restored the same way, and replayed
//     tool_search_call/tool_search_output history is flattened to a function
//     call plus its result while the discovered tools are declared again, so
//     the model can call them.
//
// Both transforms apply to the Responses wire only and only when the request
// declares a client-executed tool_search tool, so routes that natively return
// tool_search_call items are never reshaped.

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	toolSearchName       = "tool_search"
	toolSearchCallType   = "tool_search_call"
	toolSearchOutType    = "tool_search_output"
	toolSearchIDPrefix   = "tsc_"
	functionCallIDPrefix = "fc_"
)

// emulatesToolSearch reports the models whose upstream translation
// drops the tool_search declaration, so the whole protocol has to be emulated.
func emulatesToolSearch(model string) bool {
	return isWireCompatGeminiModel(model)
}

func isClientToolSearchDeclaration(value any) bool {
	tool, ok := value.(map[string]any)
	if !ok || strings.TrimSpace(wireCompatStringValue(tool["type"])) != toolSearchName {
		return false
	}
	execution := strings.TrimSpace(wireCompatStringValue(tool["execution"]))
	return execution == "" || execution == "client"
}

// declaresClientToolSearch looks at top-level tools and Codex Desktop
// (Responses Lite) additional_tools input items.
func declaresClientToolSearch(root map[string]any) bool {
	for _, tool := range rawToolSlice(root["tools"]) {
		if isClientToolSearchDeclaration(tool) {
			return true
		}
	}
	for _, item := range rawToolSlice(root["input"]) {
		if m, ok := item.(map[string]any); ok && wireCompatStringValue(m["type"]) == "additional_tools" {
			for _, tool := range rawToolSlice(m["tools"]) {
				if isClientToolSearchDeclaration(tool) {
					return true
				}
			}
		}
	}
	return false
}

func toolSearchFunction(tool map[string]any) map[string]any {
	out := map[string]any{
		"type":   "function",
		"name":   toolSearchName,
		"strict": false,
	}
	if description := strings.TrimSpace(wireCompatStringValue(tool["description"])); description != "" {
		out["description"] = description
	} else {
		out["description"] = "Search deferred tools and load the matching ones for the next call."
	}
	if parameters, ok := tool["parameters"]; ok {
		out["parameters"] = parameters
	} else {
		out["parameters"] = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query for deferred tools."},
				"limit": map[string]any{"type": "number", "description": "Maximum number of tools to return."},
			},
			"required": []any{"query"},
		}
	}
	return out
}

// emulateToolSearchRequest rewrites a Responses request for an upstream that
// does not know the tool_search protocol. It reports whether anything changed.
func emulateToolSearchRequest(root map[string]any) bool {
	changed := false
	replaceDeclarations := func(tools []any) {
		for i, tool := range tools {
			if isClientToolSearchDeclaration(tool) {
				tools[i] = toolSearchFunction(tool.(map[string]any))
				changed = true
			}
		}
	}
	replaceDeclarations(rawToolSlice(root["tools"]))

	input, _ := root["input"].([]any)
	var additional map[string]any
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok || wireCompatStringValue(item["type"]) != "additional_tools" {
			continue
		}
		replaceDeclarations(rawToolSlice(item["tools"]))
		if additional == nil {
			additional = item
		}
	}

	var loaded []any
	rewritten := make([]any, 0, len(input))
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok {
			rewritten = append(rewritten, value)
			continue
		}
		switch wireCompatStringValue(item["type"]) {
		case toolSearchCallType:
			if call, ok := flattenToolSearchCall(item); ok {
				rewritten = append(rewritten, call)
				changed = true
				continue
			}
		case toolSearchOutType:
			if result, tools, ok := flattenToolSearchOutput(item); ok {
				rewritten = append(rewritten, result)
				loaded = append(loaded, tools...)
				changed = true
				continue
			}
		}
		rewritten = append(rewritten, value)
	}
	if len(loaded) > 0 {
		if additional != nil {
			additional["tools"] = mergeLoadedTools(rawToolSlice(additional["tools"]), loaded)
		} else {
			root["tools"] = mergeLoadedTools(rawToolSlice(root["tools"]), loaded)
		}
	}
	if changed && input != nil {
		root["input"] = rewritten
	}
	return changed
}

func isClientToolSearchItem(item map[string]any) bool {
	execution := strings.TrimSpace(wireCompatStringValue(item["execution"]))
	return (execution == "" || execution == "client") && strings.TrimSpace(wireCompatStringValue(item["call_id"])) != ""
}

func flattenToolSearchCall(item map[string]any) (map[string]any, bool) {
	if !isClientToolSearchItem(item) {
		return nil, false
	}
	var arguments string
	switch args := item["arguments"].(type) {
	case string:
		arguments = args
	case nil:
		arguments = "{}"
	default:
		encoded, err := json.Marshal(args)
		if err != nil {
			return nil, false
		}
		arguments = string(encoded)
	}
	call := map[string]any{
		"type":      "function_call",
		"name":      toolSearchName,
		"call_id":   item["call_id"],
		"arguments": arguments,
	}
	if id := wireCompatStringValue(item["id"]); strings.HasPrefix(id, toolSearchIDPrefix) {
		call["id"] = functionCallIDPrefix + strings.TrimPrefix(id, toolSearchIDPrefix)
	}
	return call, true
}

func flattenToolSearchOutput(item map[string]any) (map[string]any, []any, bool) {
	if !isClientToolSearchItem(item) {
		return nil, nil, false
	}
	tools := rawToolSlice(item["tools"])
	result := map[string]any{
		"type":    "function_call_output",
		"call_id": item["call_id"],
		"output":  describeLoadedTools(tools),
	}
	return result, tools, true
}

// describeLoadedTools is the function result shown to the model: the matching
// tool names, with the definitions themselves declared on the request.
func describeLoadedTools(tools []any) string {
	var lines []string
	for _, value := range tools {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		namespace := ""
		children := []any{tool}
		if wireCompatStringValue(tool["type"]) == "namespace" {
			namespace = strings.TrimSpace(wireCompatStringValue(tool["name"]))
			children = rawToolSlice(tool["tools"])
		}
		for _, child := range children {
			fn, ok := child.(map[string]any)
			if !ok {
				continue
			}
			name := strings.TrimSpace(wireCompatStringValue(fn["name"]))
			if name == "" {
				continue
			}
			line := "- " + name
			if namespace != "" {
				line = "- " + namespace + "." + name
			}
			if description := strings.TrimSpace(wireCompatStringValue(fn["description"])); description != "" {
				if len(description) > 160 {
					description = description[:160] + "..."
				}
				line += ": " + description
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "No matching tools were found."
	}
	return fmt.Sprintf("Loaded %d tool(s). They are now available to call directly:\n%s", len(lines), strings.Join(lines, "\n"))
}

// mergeLoadedTools appends discovered tool definitions, without duplicating a
// namespace child or function that is already declared. defer_loading is a
// Codex/OpenAI hint that other upstreams do not know.
func mergeLoadedTools(existing []any, loaded []any) []any {
	declared := map[string]struct{}{}
	namespaces := map[string]map[string]any{}
	for _, value := range existing {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if wireCompatStringValue(tool["type"]) == "namespace" {
			namespace := wireCompatStringValue(tool["name"])
			namespaces[namespace] = tool
			for _, child := range rawToolSlice(tool["tools"]) {
				if c, ok := child.(map[string]any); ok {
					declared[namespace+"/"+wireCompatStringValue(c["name"])] = struct{}{}
				}
			}
			continue
		}
		declared["/"+wireCompatStringValue(tool["name"])] = struct{}{}
	}
	out := append([]any(nil), existing...)
	for _, value := range loaded {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if wireCompatStringValue(tool["type"]) != "namespace" {
			key := "/" + wireCompatStringValue(tool["name"])
			if _, dup := declared[key]; dup {
				continue
			}
			declared[key] = struct{}{}
			out = append(out, withoutDeferLoading(tool))
			continue
		}
		namespace := wireCompatStringValue(tool["name"])
		target, exists := namespaces[namespace]
		if !exists {
			target = map[string]any{"type": "namespace", "name": namespace, "tools": []any{}}
			if description := wireCompatStringValue(tool["description"]); description != "" {
				target["description"] = description
			}
			namespaces[namespace] = target
			out = append(out, target)
		}
		children := rawToolSlice(target["tools"])
		for _, child := range rawToolSlice(tool["tools"]) {
			c, ok := child.(map[string]any)
			if !ok {
				continue
			}
			key := namespace + "/" + wireCompatStringValue(c["name"])
			if _, dup := declared[key]; dup {
				continue
			}
			declared[key] = struct{}{}
			children = append(children, withoutDeferLoading(c))
		}
		target["tools"] = children
	}
	return out
}

func withoutDeferLoading(tool map[string]any) map[string]any {
	out := make(map[string]any, len(tool))
	for key, value := range tool {
		if key != "defer_loading" {
			out[key] = value
		}
	}
	return out
}

// isToolSearchFunctionCall matches both spellings of a function-form
// tool_search call: the emulated top-level function, and the reserved
// "tool_search" namespace used by OpenAI Responses Lite.
func isToolSearchFunctionCall(item map[string]any) bool {
	if wireCompatStringValue(item["type"]) != "function_call" {
		return false
	}
	namespace := strings.TrimSpace(wireCompatStringValue(item["namespace"]))
	name := strings.TrimSpace(wireCompatStringValue(item["name"]))
	return (namespace == "" && name == toolSearchName) || namespace == toolSearchName
}

// restoreToolSearchCall converts a function-form call into the tool_search_call
// item Codex executes on the client.
func restoreToolSearchCall(item map[string]any) bool {
	if !isToolSearchFunctionCall(item) {
		return false
	}
	arguments := map[string]any{}
	if raw, ok := item["arguments"].(string); ok && strings.TrimSpace(raw) != "" {
		if json.Unmarshal([]byte(raw), &arguments) != nil || arguments == nil {
			arguments = map[string]any{"query": raw}
		}
	}
	item["type"] = toolSearchCallType
	item["execution"] = "client"
	item["arguments"] = arguments
	delete(item, "name")
	delete(item, "namespace")
	if id := wireCompatStringValue(item["id"]); strings.HasPrefix(id, functionCallIDPrefix) {
		item["id"] = toolSearchIDPrefix + strings.TrimPrefix(id, functionCallIDPrefix)
	}
	return true
}

func restoreToolSearchOutput(root map[string]any) bool {
	changed := false
	for _, value := range rawToolSlice(root["output"]) {
		if item, ok := value.(map[string]any); ok && restoreToolSearchCall(item) {
			changed = true
		}
	}
	return changed
}
