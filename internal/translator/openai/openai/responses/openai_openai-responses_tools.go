package responses

import (
	"encoding/json"
	"strconv"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// responsesToolDeclaration is one Responses tool declaration paired with the
// Chat Completions function name it produces. Namespace children carry both
// their declared name and the owning namespace, so reverse translation can
// restore the split identity.
type responsesToolDeclaration struct {
	tool      gjson.Result
	chatName  string
	localName string
	namespace string
	custom    bool
}

// walkResponsesToolDeclarations visits the tool declarations of a Responses
// request in one canonical order: the top-level "tools" field first, then
// Codex Desktop (Responses Lite) "additional_tools" input items, namespace
// children in declaration order. Declarations that produce no Chat Completions
// tool are skipped. Visiting stops early once visit returns false.
//
// Request conversion, reverse name resolution and freeform tool classification
// all traverse through here, so they cannot disagree about which declaration
// backs a given Chat Completions tool name.
func walkResponsesToolDeclarations(root gjson.Result, visit func(responsesToolDeclaration) bool) {
	proceed := true
	emit := func(tool gjson.Result, namespaceName string) {
		if !proceed {
			return
		}
		var custom bool
		toolType := strings.TrimSpace(tool.Get("type").String())
		switch toolType {
		case "", "function":
		case "custom":
			custom = true
		case "shell", "local_shell":
		default:
			return
		}
		localName := responsesToolName(tool)
		if localName == "" && (toolType == "shell" || toolType == "local_shell") {
			localName = toolType
		}
		if localName == "" {
			return
		}
		proceed = visit(responsesToolDeclaration{
			tool:      tool,
			chatName:  qualifyResponsesNamespaceToolName(namespaceName, localName),
			localName: localName,
			namespace: namespaceName,
			custom:    custom,
		})
	}
	scan := func(tools gjson.Result) {
		if !proceed || !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			if strings.TrimSpace(tool.Get("type").String()) == "namespace" {
				if children := tool.Get("tools"); children.Exists() && children.IsArray() {
					namespaceName := strings.TrimSpace(tool.Get("name").String())
					children.ForEach(func(_, child gjson.Result) bool {
						emit(child, namespaceName)
						return proceed
					})
				}
				return proceed
			}
			emit(tool, "")
			return proceed
		})
	}

	scan(root.Get("tools"))
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "additional_tools" {
				scan(item.Get("tools"))
			}
			return proceed
		})
	}
}

// mergeResponsesRequestChatTools converts every tool declaration in a Responses
// request into Chat Completions form, merging the top-level "tools" field with
// Codex Desktop (Responses Lite) "additional_tools" input items.
//
// Codex clients may deliver the same tool through both channels, and namespace
// qualification can collapse distinct declarations onto one Chat Completions
// name, so entries are deduplicated by function name. The first occurrence
// wins, which keeps the top-level "tools" definition authoritative over the
// "additional_tools" copy. Chat Completions requires tool names to be unique;
// strict upstreams reject the whole request otherwise.
func mergeResponsesRequestChatTools(root gjson.Result) [][]byte {
	var merged [][]byte
	seenToolNames := make(map[string]struct{})
	walkResponsesToolDeclarations(root, func(declaration responsesToolDeclaration) bool {
		if _, duplicate := seenToolNames[declaration.chatName]; duplicate {
			return true
		}
		convert := convertResponsesFunctionToolToOpenAIChat
		if declaration.custom {
			convert = convertResponsesCustomToolToOpenAIChat
		} else if strings.TrimSpace(declaration.tool.Get("type").String()) == "shell" || strings.TrimSpace(declaration.tool.Get("type").String()) == "local_shell" {
			convert = func(t gjson.Result, name string) ([]byte, bool) {
				return convertResponsesShellToolToOpenAIChat(t, name)
			}
		}
		if chatTool, ok := convert(declaration.tool, declaration.chatName); ok {
			seenToolNames[declaration.chatName] = struct{}{}
			merged = append(merged, chatTool)
		}
		return true
	})
	return merged
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

// convertResponsesShellToolToOpenAIChat maps Responses built-in shell tools
// onto a Chat Completions function. Codex and some Responses clients send
// `{"type":"shell"}` / `{"type":"local_shell"}` with no function wrapper;
// dropping those left Cursor (and other Chat Completions executors) with no
// shell tool at all.
func convertResponsesShellToolToOpenAIChat(tool gjson.Result, defaultName string) ([]byte, bool) {
	name := responsesToolName(tool)
	if name == "" {
		name = strings.TrimSpace(defaultName)
	}
	if name == "" {
		name = "shell"
	}
	if responsesToolParameters(tool).Exists() {
		return convertResponsesFunctionToolToOpenAIChat(tool, name)
	}
	chatTool := []byte(`{"type":"function","function":{"name":"","description":"Run a shell command","parameters":{"type":"object","properties":{"command":{"type":"string"},"workdir":{"type":"string"}},"required":["command"]}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", name)
	if description := responsesToolDescription(tool); description != "" {
		chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	}
	return chatTool, true
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

// responsesCustomToolNames collects the Chat Completions names of the freeform
// ("custom") tools that survive the merge, so response translation only unwraps
// freeform arguments for calls whose winning declaration really was freeform.
//
// Declaration types may differ across the two delivery channels: a top-level
// function and an "additional_tools" custom tool can flatten to the same name.
// Classification therefore follows the same first-wins rule as the merge —
// a discarded custom declaration must not turn a surviving ordinary function
// into a custom_tool_call.
func responsesCustomToolNames(requestRawJSON []byte) map[string]struct{} {
	names := make(map[string]struct{})
	seenToolNames := make(map[string]struct{})
	walkResponsesToolDeclarations(gjson.ParseBytes(requestRawJSON), func(declaration responsesToolDeclaration) bool {
		if _, duplicate := seenToolNames[declaration.chatName]; duplicate {
			return true
		}
		seenToolNames[declaration.chatName] = struct{}{}
		if declaration.custom {
			names[declaration.chatName] = struct{}{}
		}
		return true
	})
	return names
}

func responsesSingleCustomToolName(requestRawJSON []byte) (string, bool) {
	customToolNames := responsesCustomToolNames(requestRawJSON)
	if len(customToolNames) != 1 {
		return "", false
	}

	// Count the tools actually emitted, which are deduplicated by name, so a
	// tool delivered through both "tools" and "additional_tools" still counts
	// once and freeform unwrapping stays enabled.
	toolCount := len(mergeResponsesRequestChatTools(gjson.ParseBytes(requestRawJSON)))
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

// resolveResponsesQualifiedToolIdentity maps an emitted Chat Completions
// function name back to the Responses declaration that produced it.
//
// Declarations are walked in the same order mergeResponsesRequestChatTools
// uses, and the first one producing the name wins, so reverse translation
// reports the identity of the declaration that actually survived the merge. A
// flat top-level tool named "editor__apply_patch" therefore stays flat even
// when a later namespace declares a child qualifying to the same name.
func resolveResponsesQualifiedToolIdentity(root gjson.Result, qualifiedName string) (name, namespace string, found bool) {
	walkResponsesToolDeclarations(root, func(declaration responsesToolDeclaration) bool {
		if declaration.chatName != qualifiedName {
			return true
		}
		name, namespace, found = declaration.localName, declaration.namespace, true
		return false
	})
	return name, namespace, found
}

func splitResponsesQualifiedFunctionCallFromRequest(requestRawJSON []byte, qualifiedName string) (name, namespace string) {
	qualifiedName = strings.TrimSpace(qualifiedName)
	if qualifiedName == "" {
		return "", ""
	}

	if resolvedName, resolvedNamespace, ok := resolveResponsesQualifiedToolIdentity(gjson.ParseBytes(requestRawJSON), qualifiedName); ok {
		return resolvedName, resolvedNamespace
	}
	return qualifiedName, ""
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
	return translatorcommon.SetResponsesToolCallIdentity(item, name, namespace, itemPath)
}
