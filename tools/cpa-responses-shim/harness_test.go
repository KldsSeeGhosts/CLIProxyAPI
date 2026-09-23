package main

import (
	"testing"

	"github.com/tidwall/gjson"
)

// Each harness speaks one wire format. These cases pin that a transform meant
// for one wire never reshapes another wire's payload.

const weatherSchema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

func rewriteOrFail(t *testing.T, w wire, body string) []byte {
	t.Helper()
	out, _, err := rewriteRequestJSON(w, []byte(body))
	if err != nil {
		t.Fatalf("rewriteRequestJSON: %v", err)
	}
	return out
}

func TestClaudeCodeMessagesToolsKeepSchema(t *testing.T) {
	out := rewriteOrFail(t, wireMessages, `{"model":"opencode-go/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],
		"context_management":{"edits":[]},
		"tools":[{"name":"get_weather","input_schema":`+weatherSchema+`},{"type":"web_search_20250305","name":"web_search"}]}`)
	if got := gjson.GetBytes(out, "tools.0.input_schema.required.0").String(); got != "city" {
		t.Fatalf("input_schema lost: %s", out)
	}
	if gjson.GetBytes(out, "tools.0.type").Exists() || gjson.GetBytes(out, "tools.0.parameters").Exists() {
		t.Fatalf("messages tool reshaped into responses format: %s", out)
	}
	if gjson.GetBytes(out, "tools.0.description").String() != "get_weather" {
		t.Fatalf("description not filled: %s", out)
	}
	if gjson.GetBytes(out, "tools.1.type").String() != "web_search_20250305" || gjson.GetBytes(out, "tools.1.description").Exists() {
		t.Fatalf("server tool changed: %s", out)
	}
	if gjson.GetBytes(out, "context_management").Exists() {
		t.Fatalf("map context_management kept: %s", out)
	}
}

func TestChatToolsFillDescriptionAndRemapDeveloper(t *testing.T) {
	out := rewriteOrFail(t, wireChat, `{"model":"omen-alpha","messages":[{"role":"developer","content":"sys"}],
		"tools":[{"type":"function","function":{"name":"get_weather","parameters":`+weatherSchema+`}},
		{"type":"function","function":{"name":"kept","description":"mine","parameters":{}}}]}`)
	if gjson.GetBytes(out, "messages.0.role").String() != "system" {
		t.Fatalf("developer not remapped: %s", out)
	}
	if gjson.GetBytes(out, "tools.0.function.description").String() != "get_weather" ||
		gjson.GetBytes(out, "tools.1.function.description").String() != "mine" ||
		gjson.GetBytes(out, "tools.0.function.parameters.required.0").String() != "city" {
		t.Fatalf("chat tools wrong: %s", out)
	}
}

func TestResponsesToolsFillDescription(t *testing.T) {
	out := rewriteOrFail(t, wireResponses, `{"model":"opencode-go/omen-alpha","input":"hi",
		"tools":[{"type":"function","name":"get_weather","parameters":`+weatherSchema+`}]}`)
	if gjson.GetBytes(out, "tools.0.description").String() != "get_weather" {
		t.Fatalf("description not filled: %s", out)
	}
}

func TestGeminiKeepsToolSchemaFieldsNamedLikeSignatures(t *testing.T) {
	for _, w := range []wire{wireResponses, wireChat, wireMessages} {
		body := `{"model":"gemini-3.8-flash","input":"hi","messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"encrypted_content":{"type":"string"}}}}]}`
		out := rewriteOrFail(t, w, body)
		if !gjson.GetBytes(out, "tools.0.parameters.properties.encrypted_content").Exists() {
			t.Fatalf("wire %d: tool schema property stripped: %s", w, out)
		}
	}
}

func TestNonOpenCodeModelsUntouched(t *testing.T) {
	for _, model := range []string{"cursor/muse-spark-1.3-high", "gpt-5.6-luna", "claude-sonnet-4-6", "devin/swe-2"} {
		for _, w := range []wire{wireResponses, wireChat, wireMessages} {
			body := `{"model":"` + model + `","messages":[{"role":"developer","content":"x"}],"input":[{"type":"additional_tools","tools":[]}],
				"tools":[{"type":"function","name":"n","function":{"name":"n"},"input_schema":{}}]}`
			if _, changed, err := rewriteRequestJSON(w, []byte(body)); err != nil || changed {
				t.Fatalf("%s wire %d: changed=%v err=%v", model, w, changed, err)
			}
		}
	}
}
