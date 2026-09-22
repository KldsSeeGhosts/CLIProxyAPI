package helps

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeOpenAITools ensures that each entry in the tools array conforms to the OpenAI spec:
// {"type":"function", "function":{"name":..., "description":..., "parameters":...}}.
// If an entry is flattened as {"type":"function", "name":..., "parameters":...},
// it wraps the function fields into a nested "function" object.
func NormalizeOpenAITools(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return payload
	}

	out := payload
	toolIndex := 0
	changed := false

	tools.ForEach(func(_, tool gjson.Result) bool {
		idx := toolIndex
		toolIndex++

		// Check if type is function (or empty defaulting to function)
		toolType := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
		if toolType != "function" && toolType != "" {
			return true
		}

		// If "function" is already an object with a name, it's well-formed
		fnObj := tool.Get("function")
		if fnObj.Exists() && fnObj.IsObject() && fnObj.Get("name").Exists() {
			return true
		}

		// If "name" is top-level on the tool, we need to wrap into "function"
		name := tool.Get("name")
		if !name.Exists() {
			return true
		}

		fnMap := make(map[string]any)
		fnMap["name"] = name.Value()

		if desc := tool.Get("description"); desc.Exists() {
			fnMap["description"] = desc.Value()
		}
		if params := tool.Get("parameters"); params.Exists() {
			fnMap["parameters"] = params.Value()
		} else if schema := tool.Get("parametersJsonSchema"); schema.Exists() {
			fnMap["parameters"] = schema.Value()
		} else if schema := tool.Get("input_schema"); schema.Exists() {
			fnMap["parameters"] = schema.Value()
		}
		if strict := tool.Get("strict"); strict.Exists() {
			fnMap["strict"] = strict.Value()
		}

		basePath := fmt.Sprintf("tools.%d", idx)
		newTool := map[string]any{
			"type":     "function",
			"function": fnMap,
		}

		if updated, errSet := sjson.SetBytes(out, basePath, newTool); errSet == nil {
			out = updated
			changed = true
		}
		return true
	})

	if !changed {
		return payload
	}
	return out
}
