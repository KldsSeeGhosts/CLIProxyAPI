package middleware

// OpenCode Go wire compatibility helpers.
//
// OpenCode Go (Zen) does not understand Codex Desktop's additional_tools
// input items, namespace tool declarations, or custom tools. The request side
// flattens them into plain function tools; the response side restores the
// original names, namespaces, and custom-tool shape using the identities
// recorded from the request.

import (
	"bytes"
	"encoding/json"
	"strings"
)

type openCodeIdentity struct{ Name, Namespace, Kind string }
type openCodeIdentities map[string]openCodeIdentity

// openCodeToolIdentities records declarations before flattening. Namespaces
// are never inferred from unknown response names: MCP names and literal
// double underscores are valid names.
func openCodeToolIdentities(root map[string]any) openCodeIdentities {
	ids := openCodeIdentities{}
	var visit func(any, string)
	visit = func(value any, namespace string) {
		tool, ok := value.(map[string]any)
		if !ok {
			return
		}
		if wireCompatStringValue(tool["type"]) == "namespace" {
			for _, child := range rawToolSlice(tool["tools"]) {
				visit(child, wireCompatStringValue(tool["name"]))
			}
			return
		}
		_, flat, ok := normalizeOpenCodeTool(tool, namespace)
		if !ok {
			return
		}
		name := wireCompatStringValue(tool["name"])
		if name == "" {
			if fn, ok := tool["function"].(map[string]any); ok {
				name = wireCompatStringValue(fn["name"])
			}
		}
		id := openCodeIdentity{name, namespace, wireCompatStringValue(tool["type"])}
		if prior, exists := ids[flat]; !exists || (prior.Namespace == "" && namespace != "") {
			ids[flat] = id
		}
	}
	for _, tool := range rawToolSlice(root["tools"]) {
		visit(tool, "")
	}
	for _, item := range rawToolSlice(root["input"]) {
		if m, ok := item.(map[string]any); ok && wireCompatStringValue(m["type"]) == "additional_tools" {
			for _, tool := range rawToolSlice(m["tools"]) {
				visit(tool, "")
			}
		}
	}
	return ids
}

// flattenOpenCodeReplay rewrites replayed custom_tool_call and
// custom_tool_call_output input items into the function-call shapes OpenCode
// Go accepts, using the identities recorded from the request's declarations.
func flattenOpenCodeReplay(root map[string]any, ids openCodeIdentities) bool {
	changed := false
	for _, value := range rawToolSlice(root["input"]) {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		kind := wireCompatStringValue(item["type"])
		if kind == "custom_tool_call_output" {
			item["type"] = "function_call_output"
			changed = true
			continue
		}
		if kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		for flat, id := range ids {
			if wireCompatStringValue(item["name"]) != id.Name || wireCompatStringValue(item["namespace"]) != id.Namespace {
				continue
			}
			if wireCompatStringValue(item["name"]) != flat || id.Namespace != "" {
				item["name"] = flat
				delete(item, "namespace")
				changed = true
			}
			if kind == "custom_tool_call" && id.Kind == "custom" {
				args, _ := json.Marshal(map[string]any{"input": item["input"]})
				item["type"] = "function_call"
				item["arguments"] = string(args)
				delete(item, "input")
				changed = true
			}
			break
		}
	}
	return changed
}

// flattenOpenCodeTools merges top-level tools and Codex Desktop's
// additional_tools input items into a deduplicated flat function-tool list
// that OpenCode Go accepts.
func flattenOpenCodeTools(root map[string]any) bool {
	input, hasInputArray := root["input"].([]any)

	topTools := rawToolSlice(root["tools"])
	merged := make([]any, 0, len(topTools))
	seen := make(map[string]struct{}, len(topTools))
	for _, tool := range topTools {
		for _, flattened := range flattenOpenCodeTool(tool) {
			normalized, name, ok := normalizeOpenCodeTool(flattened, "")
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
		if !ok || strings.TrimSpace(wireCompatStringValue(itemMap["type"])) != "additional_tools" {
			rewrittenInput = append(rewrittenInput, item)
			continue
		}
		changed = true
		for _, tool := range rawToolSlice(itemMap["tools"]) {
			for _, flattened := range flattenOpenCodeTool(tool) {
				name := wireCompatStringValue(flattened.(map[string]any)["name"])
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

func flattenOpenCodeTool(tool any) []any {
	toolMap, ok := tool.(map[string]any)
	if !ok {
		return nil
	}
	toolType := strings.ToLower(strings.TrimSpace(wireCompatStringValue(toolMap["type"])))
	if toolType == "tool_search" || strings.EqualFold(strings.TrimSpace(wireCompatStringValue(toolMap["name"])), "tool_search") {
		return nil
	}
	if toolType == "namespace" {
		namespace := strings.TrimSpace(wireCompatStringValue(toolMap["name"]))
		if strings.EqualFold(namespace, "tool_search") {
			return nil
		}
		out := make([]any, 0)
		for _, child := range rawToolSlice(toolMap["tools"]) {
			normalized, _, ok := normalizeOpenCodeTool(child, namespace)
			if ok {
				out = append(out, normalized)
			}
		}
		return out
	}
	normalized, _, ok := normalizeOpenCodeTool(tool, "")
	if !ok {
		return nil
	}
	return []any{normalized}
}

func normalizeOpenCodeTool(tool any, namespace string) (map[string]any, string, bool) {
	toolMap, ok := tool.(map[string]any)
	if !ok {
		return nil, "", false
	}
	toolType := strings.ToLower(strings.TrimSpace(wireCompatStringValue(toolMap["type"])))
	if toolType == "tool_search" || strings.EqualFold(strings.TrimSpace(wireCompatStringValue(toolMap["name"])), "tool_search") {
		return nil, "", false
	}
	name := strings.TrimSpace(wireCompatStringValue(toolMap["name"]))
	if name == "" {
		if function, ok := toolMap["function"].(map[string]any); ok {
			name = strings.TrimSpace(wireCompatStringValue(function["name"]))
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

// restoreOpenCodeItem maps a flattened function_call back to the declared
// tool name, namespace, and custom-tool shape.
func restoreOpenCodeItem(item map[string]any, ids openCodeIdentities) (openCodeIdentity, bool) {
	kind := wireCompatStringValue(item["type"])
	if kind != "function_call" && kind != "custom_tool_call" {
		return openCodeIdentity{}, false
	}
	id, ok := ids[wireCompatStringValue(item["name"])]
	if !ok {
		return id, false
	}
	item["name"] = id.Name
	if id.Namespace != "" {
		item["namespace"] = id.Namespace
	} else {
		delete(item, "namespace")
	}
	if id.Kind == "custom" {
		item["type"] = "custom_tool_call"
		if args, ok := item["arguments"].(string); ok {
			var wrapper struct {
				Input string `json:"input"`
			}
			if args == "" {
				item["input"] = ""
			} else if json.Unmarshal([]byte(args), &wrapper) == nil {
				item["input"] = wrapper.Input
			} else {
				item["input"] = args
			}
			delete(item, "arguments")
		}
	}
	return id, true
}

func restoreOpenCodeOutput(root map[string]any, ids openCodeIdentities) {
	for _, value := range rawToolSlice(root["output"]) {
		if item, ok := value.(map[string]any); ok {
			restoreOpenCodeItem(item, ids)
		}
	}
}
