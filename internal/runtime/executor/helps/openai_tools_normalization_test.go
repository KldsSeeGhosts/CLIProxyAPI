package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAITools(t *testing.T) {
	t.Run("converts flattened tools to nested function object", func(t *testing.T) {
		input := []byte(`{
			"model": "omen-alpha",
			"tools": [
				{
					"type": "function",
					"name": "exec_command",
					"description": "Run shell command",
					"parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}}
				}
			]
		}`)

		output := NormalizeOpenAITools(input)
		tool := gjson.GetBytes(output, "tools.0")
		if tool.Get("type").String() != "function" {
			t.Fatalf("expected type function, got %s", tool.Get("type").String())
		}
		fn := tool.Get("function")
		if !fn.Exists() || !fn.IsObject() {
			t.Fatalf("expected nested function object, got %s", fn.Raw)
		}
		if fn.Get("name").String() != "exec_command" {
			t.Fatalf("expected name exec_command, got %s", fn.Get("name").String())
		}
		if fn.Get("description").String() != "Run shell command" {
			t.Fatalf("expected description Run shell command, got %s", fn.Get("description").String())
		}
		if fn.Get("parameters.type").String() != "object" {
			t.Fatalf("expected parameters type object, got %s", fn.Get("parameters.type").String())
		}
		// Top-level "name" should no longer be on tool directly (it is inside function)
		if tool.Get("name").Exists() {
			t.Fatalf("expected name not to exist at tool root, got %s", tool.Get("name").String())
		}
	})

	t.Run("preserves already well-formed nested tools", func(t *testing.T) {
		input := []byte(`{
			"model": "omen-alpha",
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "already_nested",
						"description": "desc",
						"parameters": {"type": "object"}
					}
				}
			]
		}`)

		output := NormalizeOpenAITools(input)
		fn := gjson.GetBytes(output, "tools.0.function")
		if fn.Get("name").String() != "already_nested" {
			t.Fatalf("expected already_nested, got %s", fn.Get("name").String())
		}
	})

	t.Run("converts parametersJsonSchema and input_schema", func(t *testing.T) {
		input := []byte(`{
			"tools": [
				{
					"type": "function",
					"name": "schema_test",
					"parametersJsonSchema": {"type": "object", "properties": {"a": {"type": "number"}}}
				}
			]
		}`)

		output := NormalizeOpenAITools(input)
		paramType := gjson.GetBytes(output, "tools.0.function.parameters.type").String()
		if paramType != "object" {
			t.Fatalf("expected parameters.type object, got %s", paramType)
		}
	})

	t.Run("handles payload with no tools", func(t *testing.T) {
		input := []byte(`{"model": "omen-alpha", "messages": []}`)
		output := NormalizeOpenAITools(input)
		if string(output) != string(input) {
			t.Fatalf("expected payload unchanged, got %s", string(output))
		}
	})
}
