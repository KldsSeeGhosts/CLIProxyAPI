package responses

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// geminiResponsesCustomToolNames returns the freeform tool names declared by
// the original Responses request. Codex Desktop may place these declarations
// either in tools or in an input additional_tools item.
func geminiResponsesCustomToolNames(requestRawJSON []byte) map[string]struct{} {
	names := make(map[string]struct{})
	root := unwrapRequestRoot(gjson.ParseBytes(requestRawJSON))

	var collect func(gjson.Result, string)
	collect = func(tools gjson.Result, namespace string) {
		if !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			typeName := strings.TrimSpace(tool.Get("type").String())
			switch typeName {
			case "custom":
				name := strings.TrimSpace(tool.Get("name").String())
				if name == "" {
					name = strings.TrimSpace(tool.Get("function.name").String())
				}
				if namespace != "" {
					name = geminiQualifyToolName(namespace, name)
				}
				if name != "" {
					names[name] = struct{}{}
				}
			case "namespace":
				childNamespace := strings.TrimSpace(tool.Get("name").String())
				if namespace != "" {
					childNamespace = geminiQualifyToolName(namespace, childNamespace)
				}
				collect(tool.Get("tools"), childNamespace)
			}
			return true
		})
	}

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

func geminiQualifyToolName(namespace, name string) string {
	name = strings.TrimSpace(name)
	namespace = strings.TrimSpace(namespace)
	if name == "" || namespace == "" || strings.HasPrefix(name, "mcp__") || strings.HasPrefix(name, namespace+"__") {
		return name
	}
	return namespace + "__" + name
}

func geminiSplitResponsesQualifiedToolName(requestRawJSON []byte, qualifiedName string) (name, namespace string) {
	qualifiedName = strings.TrimSpace(qualifiedName)
	if qualifiedName == "" {
		return "", ""
	}

	root := unwrapRequestRoot(gjson.ParseBytes(requestRawJSON))
	var bestNamespace string
	var bestChild string
	var collect func(gjson.Result, string)
	collect = func(tools gjson.Result, parentNamespace string) {
		if !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			if strings.TrimSpace(tool.Get("type").String()) != "namespace" {
				return true
			}
			namespaceName := strings.TrimSpace(tool.Get("name").String())
			if parentNamespace != "" {
				namespaceName = geminiQualifyToolName(parentNamespace, namespaceName)
			}
			if namespaceName == "" {
				return true
			}
			children := tool.Get("tools")
			if !children.Exists() || !children.IsArray() {
				return true
			}
			children.ForEach(func(_, child gjson.Result) bool {
				childName := geminiResponsesToolName(child)
				if childName != "" && geminiQualifyToolName(namespaceName, childName) == qualifiedName {
					bestNamespace = namespaceName
					bestChild = childName
				}
				if strings.TrimSpace(child.Get("type").String()) == "namespace" {
					collect(gjson.Result{Type: gjson.JSON, Raw: "[" + child.Raw + "]"}, namespaceName)
				}
				return true
			})
			return true
		})
	}

	collect(root.Get("tools"), "")
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "additional_tools" {
				collect(item.Get("tools"), "")
			}
			return true
		})
	}
	if bestNamespace == "" || bestChild == "" {
		return qualifiedName, ""
	}
	return bestChild, bestNamespace
}

func geminiApplyResponsesToolNamespaceFields(item []byte, requestRawJSON []byte, qualifiedName, itemPath string) []byte {
	name, namespace := geminiSplitResponsesQualifiedToolName(requestRawJSON, qualifiedName)
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

func geminiUnwrapCustomToolInput(arguments string) string {
	input := arguments
	for {
		value := gjson.Get(input, "input")
		if !value.Exists() {
			return input
		}
		if value.Type == gjson.String {
			input = value.String()
		} else {
			input = value.Raw
		}
	}
}

// geminiNormalizeCustomToolInput preserves normal freeform input, while
// adapting the legacy command-shaped payload emitted by some Claude/Gemini
// backends into the JavaScript source expected by Codex's code-mode exec tool.
func geminiNormalizeCustomToolInput(name, arguments string) string {
	input := geminiUnwrapCustomToolInput(arguments)
	if name != "exec" {
		return input
	}
	obj := gjson.Parse(input)
	if !obj.Exists() || !obj.IsObject() {
		if strings.Contains(input, "tools.exec_command") || strings.TrimSpace(input) == "" {
			return input
		}
		return "const r = await tools.exec_command({cmd:" + geminiJSONString(input) + "}); text(r.output);"
	}
	command := strings.TrimSpace(obj.Get("command").String())
	if command == "" {
		command = strings.TrimSpace(obj.Get("cmd").String())
	}
	if command == "" {
		if strings.Contains(input, "tools.exec_command") {
			return input
		}
		if strings.TrimSpace(input) == "" {
			return input
		}
		return "const r = await tools.exec_command({cmd:" + geminiJSONString(input) + "}); text(r.output);"
	}

	fields := []string{"cmd:" + geminiJSONString(command)}
	appendString := func(key string) {
		if value := obj.Get(key); value.Exists() && value.Type == gjson.String && value.String() != "" {
			fields = append(fields, key+":"+geminiJSONString(value.String()))
		}
	}
	appendString("workdir")
	appendString("shell")
	appendString("justification")
	appendString("sandbox_permissions")
	appendString("tty")
	appendString("login")

	if timeout := geminiPositiveInt(obj.Get("timeout_ms")); timeout > 0 {
		if timeout < 250 {
			timeout = 250
		}
		if timeout > 30000 {
			timeout = 30000
		}
		fields = append(fields, "yield_time_ms:"+strconv.Itoa(timeout))
	}
	if maxTokens := geminiPositiveInt(obj.Get("max_output_tokens")); maxTokens > 0 {
		fields = append(fields, "max_output_tokens:"+strconv.Itoa(maxTokens))
	}

	return "const r = await tools.exec_command({" + strings.Join(fields, ", ") + "}); text(r.output);"
}

func geminiPositiveInt(value gjson.Result) int {
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

func geminiJSONString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func geminiResponsesToolName(tool gjson.Result) string {
	if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
		return name
	}
	return strings.TrimSpace(tool.Get("function.name").String())
}

func geminiResponsesToolDescription(tool gjson.Result) string {
	if description := tool.Get("description").String(); description != "" {
		return description
	}
	return tool.Get("function.description").String()
}

func geminiResponsesToolParameters(tool gjson.Result) gjson.Result {
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

func geminiAppendToolDeclarations(tools gjson.Result, namespace string, declarations *[][]byte) {
	if !tools.Exists() || !tools.IsArray() {
		return
	}
	tools.ForEach(func(_, tool gjson.Result) bool {
		typeName := strings.TrimSpace(tool.Get("type").String())
		name := geminiResponsesToolName(tool)
		if namespace != "" {
			name = geminiQualifyToolName(namespace, name)
		}
		switch typeName {
		case "", "function":
			if name == "" {
				return true
			}
			decl := []byte(`{"name":"","description":"","parametersJsonSchema":{}}`)
			decl, _ = sjson.SetBytes(decl, "name", util.SanitizeFunctionName(name))
			if description := geminiResponsesToolDescription(tool); description != "" {
				decl, _ = sjson.SetBytes(decl, "description", description)
			}
			if params := geminiResponsesToolParameters(tool); params.Exists() {
				decl, _ = sjson.SetRawBytes(decl, "parametersJsonSchema", []byte(util.CleanJSONSchemaForGemini(params.Raw)))
			}
			*declarations = append(*declarations, decl)
		case "custom":
			if name == "" {
				return true
			}
			decl := []byte(`{"name":"","description":"","parametersJsonSchema":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}}`)
			decl, _ = sjson.SetBytes(decl, "name", util.SanitizeFunctionName(name))
			if description := geminiResponsesToolDescription(tool); description != "" {
				decl, _ = sjson.SetBytes(decl, "description", description)
			}
			*declarations = append(*declarations, decl)
		case "namespace":
			childNamespace := name
			if childNamespace == "" {
				childNamespace = namespace
			}
			geminiAppendToolDeclarations(tool.Get("tools"), childNamespace, declarations)
		}
		return true
	})
}

func geminiCustomToolCallAsFunctionItem(item gjson.Result) gjson.Result {
	name := item.Get("name").String()
	input := geminiNormalizeCustomToolInput(name, item.Get("input").String())
	arguments, _ := sjson.SetBytes([]byte(`{"input":""}`), "input", input)
	converted := []byte(`{"type":"function_call","name":"","call_id":"","arguments":""}`)
	converted, _ = sjson.SetBytes(converted, "name", name)
	converted, _ = sjson.SetBytes(converted, "call_id", item.Get("call_id").String())
	converted, _ = sjson.SetBytes(converted, "arguments", string(arguments))
	if signature := item.Get("_cpa_reasoning_signature"); signature.Exists() {
		converted, _ = sjson.SetBytes(converted, "_cpa_reasoning_signature", signature.String())
	}
	if summary := item.Get("_cpa_reasoning_summary"); summary.Exists() {
		converted, _ = sjson.SetBytes(converted, "_cpa_reasoning_summary", summary.String())
	}
	return gjson.ParseBytes(converted)
}
