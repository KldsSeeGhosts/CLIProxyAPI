package responses

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func convertResponsesToolToOpenAIChatTools(tool gjson.Result) [][]byte {
	toolType := strings.TrimSpace(tool.Get("type").String())
	switch toolType {
	case "", "function":
		if tJSON, ok := convertResponsesFunctionToolToOpenAIChat(tool, ""); ok {
			return [][]byte{tJSON}
		}
	case "namespace":
		return convertResponsesNamespaceToolToOpenAIChat(tool)
	case "custom":
		if tJSON, ok := convertResponsesCustomToolToOpenAIChat(tool, ""); ok {
			return [][]byte{tJSON}
		}
	default:
		return nil
	}
	return nil
}

// convertResponsesCustomToolToOpenAIChat maps a Responses freeform ("custom")
// tool onto a Chat Completions function tool with a single freeform "input"
// string, mirroring the function-based shape Codex uses for apply_patch.
func convertResponsesCustomToolToOpenAIChat(tool gjson.Result, overrideName string) ([]byte, bool) {
	name := strings.TrimSpace(overrideName)
	if name == "" {
		name = responsesToolName(tool)
	}
	if name == "" {
		return nil, false
	}
	chatTool := []byte(`{"type":"function","function":{"name":"","description":"","parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", name)
	if description := responsesToolDescription(tool); description != "" {
		chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	}
	return chatTool, true
}

func convertResponsesNamespaceToolToOpenAIChat(tool gjson.Result) [][]byte {
	namespaceName := strings.TrimSpace(tool.Get("name").String())
	children := tool.Get("tools")
	if !children.Exists() || !children.IsArray() {
		return nil
	}

	var out [][]byte
	children.ForEach(func(_, child gjson.Result) bool {
		childName := responsesToolName(child)
		qualifiedName := qualifyResponsesNamespaceToolName(namespaceName, childName)
		switch strings.TrimSpace(child.Get("type").String()) {
		case "", "function":
			if tJSON, ok := convertResponsesFunctionToolToOpenAIChat(child, qualifiedName); ok {
				out = append(out, tJSON)
			}
		case "custom":
			if tJSON, ok := convertResponsesCustomToolToOpenAIChat(child, qualifiedName); ok {
				out = append(out, tJSON)
			}
		}
		return true
	})
	return out
}

func convertResponsesFunctionToolToOpenAIChat(tool gjson.Result, overrideName string) ([]byte, bool) {
	name := strings.TrimSpace(overrideName)
	if name == "" {
		name = responsesToolName(tool)
	}
	if name == "" {
		return nil, false
	}

	chatTool := []byte(`{"type":"function","function":{"name":"","description":"","parameters":{"type":"object","properties":{}}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", name)
	if description := responsesToolDescription(tool); description != "" {
		chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	}
	if parameters := responsesToolParameters(tool); parameters.Exists() {
		parameterJSON := []byte(parameters.Raw)
		if parameters.IsObject() {
			// Chat Completions function arguments are always JSON objects. Some
			// strict providers, including Kimi, reject otherwise valid union-root
			// schemas unless this root type is stated explicitly.
			parameterJSON, _ = sjson.SetBytes(parameterJSON, "type", "object")
		} else {
			parameterJSON = []byte(`{"type":"object","properties":{}}`)
		}
		chatTool, _ = sjson.SetRawBytes(chatTool, "function.parameters", parameterJSON)
	}
	return chatTool, true
}

func responsesToolName(tool gjson.Result) string {
	if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
		return name
	}
	return strings.TrimSpace(tool.Get("function.name").String())
}

func responsesToolDescription(tool gjson.Result) string {
	if description := tool.Get("description").String(); description != "" {
		return description
	}
	return tool.Get("function.description").String()
}

func responsesToolParameters(tool gjson.Result) gjson.Result {
	for _, path := range []string{
		"parameters",
		"parametersJsonSchema",
		"input_schema",
		"function.parameters",
		"function.parametersJsonSchema",
	} {
		if parameters := tool.Get(path); parameters.Exists() {
			return parameters
		}
	}
	return gjson.Result{}
}

// responsesToolOutputText flattens a tool output value that may be a plain
// string or an array of content parts ({"type":"input_text","text":...}) into
// a single text payload for a Chat Completions tool message.
func responsesToolOutputText(output gjson.Result) string {
	if output.Type == gjson.String {
		return output.String()
	}
	if output.IsArray() {
		var b strings.Builder
		output.ForEach(func(_, part gjson.Result) bool {
			if part.Type == gjson.String {
				b.WriteString(part.String())
				return true
			}
			if text := part.Get("text"); text.Exists() {
				b.WriteString(text.String())
			}
			return true
		})
		return b.String()
	}
	if output.Exists() {
		return output.Raw
	}
	return ""
}

// responsesCustomToolNames collects the names of freeform ("custom") tools
// declared in the original Responses request, both in the top-level "tools"
// field and in Codex Desktop "additional_tools" input items. Namespace child
// names use the qualified Chat Completions form.
func responsesCustomToolNames(requestRawJSON []byte) map[string]struct{} {
	names := make(map[string]struct{})
	var collect func(gjson.Result, string)
	collect = func(tools gjson.Result, namespaceName string) {
		if !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			switch strings.TrimSpace(tool.Get("type").String()) {
			case "custom":
				name := responsesToolName(tool)
				if namespaceName != "" {
					name = qualifyResponsesNamespaceToolName(namespaceName, name)
				}
				if name != "" {
					names[name] = struct{}{}
				}
			case "namespace":
				collect(tool.Get("tools"), strings.TrimSpace(tool.Get("name").String()))
			}
			return true
		})
	}
	root := gjson.ParseBytes(requestRawJSON)
	collect(root.Get("tools"), "")
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "additional_tools" {
				collect(item.Get("tools"), "")
			}
			return true
		})
	}
	return names
}

func responsesSingleCustomToolName(requestRawJSON []byte) (string, bool) {
	customToolNames := responsesCustomToolNames(requestRawJSON)
	if len(customToolNames) != 1 {
		return "", false
	}

	toolCount := 0
	collect := func(tools gjson.Result) {
		if !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			toolCount += len(convertResponsesToolToOpenAIChatTools(tool))
			return true
		})
	}

	root := gjson.ParseBytes(requestRawJSON)
	collect(root.Get("tools"))
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "additional_tools" {
				collect(item.Get("tools"))
			}
			return true
		})
	}
	for name := range customToolNames {
		return name, toolCount == 1
	}
	return "", false
}

// unwrapCustomToolInput extracts the freeform input from the {"input": "..."}
// function-call arguments produced for a converted custom tool. Some backends
// can repeat that compatibility envelope, so unwrap every nested input layer;
// fall back to the raw arguments when the wrapper is absent.
func unwrapCustomToolInput(arguments string) string {
	input := arguments
	for {
		v := gjson.Get(input, "input")
		if !v.Exists() {
			return input
		}
		if v.Type == gjson.String {
			input = v.String()
		} else {
			input = v.Raw
		}
	}
}

func normalizeCustomToolInput(name, arguments string) string {
	input := unwrapCustomToolInput(arguments)
	if name != "exec" && !strings.HasSuffix(name, "__exec") {
		return input
	}
	obj := gjson.Parse(input)
	if !obj.Exists() || !obj.IsObject() {
		if strings.Contains(input, "tools.exec_command") || strings.TrimSpace(input) == "" {
			return input
		}
		return "const r = await tools.exec_command({cmd:" + jsonString(input) + "}); text(r.output);"
	}
	command := strings.TrimSpace(obj.Get("command").String())
	if command == "" {
		command = strings.TrimSpace(obj.Get("cmd").String())
	}
	if command == "" {
		if strings.Contains(input, "tools.exec_command") || strings.TrimSpace(input) == "" {
			return input
		}
		return "const r = await tools.exec_command({cmd:" + jsonString(input) + "}); text(r.output);"
	}
	fields := []string{"cmd:" + jsonString(command)}
	appendString := func(key string) {
		if value := obj.Get(key); value.Exists() && value.Type == gjson.String && value.String() != "" {
			fields = append(fields, key+":"+jsonString(value.String()))
		}
	}
	appendString("workdir")
	appendString("shell")
	appendString("justification")
	appendString("sandbox_permissions")
	appendString("tty")
	appendString("login")
	if timeout := positiveInt(obj.Get("timeout_ms")); timeout > 0 {
		if timeout < 250 {
			timeout = 250
		}
		if timeout > 30000 {
			timeout = 30000
		}
		fields = append(fields, "yield_time_ms:"+strconv.Itoa(timeout))
	}
	if maxTokens := positiveInt(obj.Get("max_output_tokens")); maxTokens > 0 {
		fields = append(fields, "max_output_tokens:"+strconv.Itoa(maxTokens))
	}
	return "const r = await tools.exec_command({" + strings.Join(fields, ", ") + "}); text(r.output);"
}

func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func positiveInt(value gjson.Result) int {
	if !value.Exists() {
		return 0
	}
	if value.Type == gjson.Number {
		if n := int(value.Int()); n > 0 {
			return n
		}
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(value.String()))
	if n > 0 {
		return n
	}
	return 0
}

func qualifyResponsesNamespaceToolName(namespaceName, childName string) string {
	childName = strings.TrimSpace(childName)
	if childName == "" || namespaceName == "" || strings.HasPrefix(childName, "mcp__") {
		return childName
	}
	if strings.HasPrefix(childName, namespaceName) {
		return childName
	}
	if strings.HasSuffix(namespaceName, "__") {
		return namespaceName + childName
	}
	return namespaceName + "__" + childName
}

func splitResponsesQualifiedFunctionCallFromRequest(requestRawJSON []byte, qualifiedName string) (name, namespace string) {
	qualifiedName = strings.TrimSpace(qualifiedName)
	if qualifiedName == "" {
		return "", ""
	}

	var bestNamespace string
	var bestChild string
	collect := func(tools gjson.Result) {
		if !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			if strings.TrimSpace(tool.Get("type").String()) != "namespace" {
				return true
			}
			namespaceName := strings.TrimSpace(tool.Get("name").String())
			if namespaceName == "" {
				return true
			}
			children := tool.Get("tools")
			if !children.Exists() || !children.IsArray() {
				return true
			}
			children.ForEach(func(_, child gjson.Result) bool {
				childName := responsesToolName(child)
				if childName == "" {
					return true
				}
				if qualifyResponsesNamespaceToolName(namespaceName, childName) == qualifiedName {
					bestNamespace = namespaceName
					bestChild = childName
				}
				return true
			})
			return true
		})
	}

	root := gjson.ParseBytes(requestRawJSON)
	collect(root.Get("tools"))
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "additional_tools" {
				collect(item.Get("tools"))
			}
			return true
		})
	}

	if bestNamespace == "" || bestChild == "" {
		return qualifiedName, ""
	}
	return bestChild, bestNamespace
}

func pickRequestJSON(originalRequestRawJSON, requestRawJSON []byte) []byte {
	if len(originalRequestRawJSON) > 0 && gjson.ValidBytes(originalRequestRawJSON) {
		return originalRequestRawJSON
	}
	if len(requestRawJSON) > 0 && gjson.ValidBytes(requestRawJSON) {
		return requestRawJSON
	}
	return nil
}

func applyResponsesFunctionCallNamespaceFields(item []byte, requestRawJSON []byte, qualifiedName string, itemPath string) []byte {
	name, namespace := splitResponsesQualifiedFunctionCallFromRequest(requestRawJSON, qualifiedName)
	namePath := "name"
	namespacePath := "namespace"
	if itemPath != "" {
		namePath = itemPath + ".name"
		namespacePath = itemPath + ".namespace"
	}
	item, _ = sjson.SetBytes(item, namePath, name)
	if namespace != "" {
		item, _ = sjson.SetBytes(item, namespacePath, namespace)
	} else {
		item, _ = sjson.DeleteBytes(item, namespacePath)
	}
	return item
}
