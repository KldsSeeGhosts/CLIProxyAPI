package middleware

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func decodeWireCompatJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode rewritten JSON: %v", err)
	}
	return out
}

func rewriteWireCompatOrFail(t *testing.T, w wireKind, body string) []byte {
	t.Helper()
	out, _, err := rewriteWireCompatRequestJSON(w, []byte(body))
	if err != nil {
		t.Fatalf("rewriteWireCompatRequestJSON: %v", err)
	}
	return out
}

func TestArgonReplayUsesGeminiSanitization(t *testing.T) {
	for _, model := range []string{"gemini-4-argon", "gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high"} {
		if !isWireCompatGeminiModel(model) {
			t.Fatal(model)
		}
		input := []byte(`{"model":"` + model + `","input":[{"type":"function_call","thoughtSignature":"stale","arguments":"{}"}]}`)
		out, changed, err := rewriteWireCompatRequestJSON(wireKindResponses, input)
		if err != nil || !changed || bytes.Contains(out, []byte("stale")) {
			t.Fatalf("%s: %s, %v", model, out, err)
		}
	}
}

func TestRewriteRequestJSONStripsGeminiReplayFields(t *testing.T) {
	input := []byte(`{
		"model":"gemini-3.8-flash",
		"input":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"keep"}],"encrypted_content":"stale"},
			{"type":"function_call","thoughtSignature":"stale-too","arguments":"{}"},
			{"role":"user","content":"hello"}
		],
		"metadata":{"encrypted_content":"nested-stale"}
	}`)

	out, changed, err := rewriteWireCompatRequestJSON(wireKindResponses, input)
	if err != nil {
		t.Fatalf("rewriteWireCompatRequestJSON() error = %v", err)
	}
	if !changed {
		t.Fatal("rewriteWireCompatRequestJSON() changed = false, want true")
	}
	root := decodeWireCompatJSON(t, out)
	items := root["input"].([]any)
	first := items[0].(map[string]any)
	if _, ok := first["encrypted_content"]; ok {
		t.Fatal("reasoning encrypted_content was not removed")
	}
	if first["summary"].([]any)[0].(map[string]any)["text"] != "keep" {
		t.Fatal("reasoning summary was not preserved")
	}
	second := items[1].(map[string]any)
	if _, ok := second["thoughtSignature"]; ok {
		t.Fatal("thoughtSignature was not removed")
	}
	meta := root["metadata"].(map[string]any)
	if meta["encrypted_content"] != "nested-stale" {
		t.Fatal("non-history encrypted_content was removed")
	}
}

func TestGeminiPreservesOnlyTypedReasoningCarriers(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","input":[
		{"type":"reasoning","encrypted_content":"cpa-gemini-responses-carrier-v1:next:function:QUJD"},
		{"type":"reasoning","encrypted_content":"foreign-signature"},
		{"type":"function_call_output","output":{"type":"reasoning","encrypted_content":"cpa-gemini-responses-carrier-v1:next:function:QUJD"}},
		{"type":"function_call","thoughtSignature":"foreign-signature"}
	]}`)
	out, changed, err := rewriteWireCompatRequestJSON(wireKindResponses, body)
	if err != nil || !changed {
		t.Fatalf("rewrite failed: changed=%v error=%v", changed, err)
	}
	if got := gjson.GetBytes(out, "input.0.encrypted_content").String(); got != "cpa-gemini-responses-carrier-v1:next:function:QUJD" {
		t.Fatalf("typed carrier lost: %s", out)
	}
	for _, field := range []string{"input.1.encrypted_content", "input.2.output.encrypted_content", "input.3.thoughtSignature"} {
		if gjson.GetBytes(out, field).Exists() {
			t.Fatalf("unexpected retained field %s", field)
		}
	}
}

func TestRewriteRequestJSONFlattensOpenCodeAdditionalTools(t *testing.T) {
	input := []byte(`{
		"model":"opencode-go/muse-spark-1.2-contributor",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"namespace","name":"functions","tools":[
					{"type":"function","name":"exec_command","description":"run","parameters":{"type":"object"}},
					{"type":"custom","name":"apply_patch","description":"edit"}
				]},
				{"type":"namespace","name":"tool_search","tools":[
					{"type":"function","name":"search","parameters":{"type":"object"}}
				]}
			]},
			{"role":"user","content":"hello"}
		],
		"tools":[
			{"type":"function","name":"functions__exec_command","description":"authoritative","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}
		],
		"tool_choice":"auto"
	}`)

	out, changed, err := rewriteWireCompatRequestJSON(wireKindResponses, input)
	if err != nil {
		t.Fatalf("rewriteWireCompatRequestJSON() error = %v", err)
	}
	if !changed {
		t.Fatal("rewriteWireCompatRequestJSON() changed = false, want true")
	}
	root := decodeWireCompatJSON(t, out)
	items := root["input"].([]any)
	if len(items) != 1 {
		t.Fatalf("input length = %d, want 1 after removing additional_tools", len(items))
	}
	tools := root["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools length = %d, want 2", len(tools))
	}
	first := tools[0].(map[string]any)
	if first["name"] != "functions__exec_command" || first["description"] != "authoritative" {
		t.Fatalf("top-level tool did not win duplicate: %#v", first)
	}
	second := tools[1].(map[string]any)
	if second["name"] != "functions__apply_patch" || second["type"] != "function" {
		t.Fatalf("custom namespace tool was not converted: %#v", second)
	}
	if _, ok := second["parameters"].(map[string]any); !ok {
		t.Fatalf("custom namespace tool has no parameters: %#v", second)
	}
}

func TestRewriteRequestJSONLeavesOtherModelsUntouched(t *testing.T) {
	input := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","tools":[]}]}`)
	out, changed, err := rewriteWireCompatRequestJSON(wireKindResponses, input)
	if err != nil {
		t.Fatalf("rewriteWireCompatRequestJSON() error = %v", err)
	}
	if changed {
		t.Fatal("rewriteWireCompatRequestJSON() changed a non-target model")
	}
	if string(out) != string(input) {
		t.Fatalf("non-target payload changed: %s", out)
	}
}

func TestRewriteRequestJSONRejectsInvalidJSON(t *testing.T) {
	if _, _, err := rewriteWireCompatRequestJSON(wireKindResponses, []byte(`{"model":`)); err == nil {
		t.Fatal("rewriteWireCompatRequestJSON() error = nil, want invalid JSON error")
	}
}

func TestRewriteRequestJSONRemapsDeveloperRoleForOmenAlpha(t *testing.T) {
	input := []byte(`{
		"model":"opencode-go/omen-alpha",
		"messages":[
			{"role":"developer","content":"You are an expert coding assistant."},
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":"hi"}
		]
	}`)

	out, changed, err := rewriteWireCompatRequestJSON(wireKindChat, input)
	if err != nil {
		t.Fatalf("rewriteWireCompatRequestJSON() error = %v", err)
	}
	if !changed {
		t.Fatal("rewriteWireCompatRequestJSON() changed = false, want true")
	}
	root := decodeWireCompatJSON(t, out)
	messages := root["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer role was not remapped: %#v", messages[0])
	}
	if messages[0].(map[string]any)["content"] != "You are an expert coding assistant." {
		t.Fatal("system prompt content was altered")
	}
	if messages[1].(map[string]any)["role"] != "user" || messages[2].(map[string]any)["role"] != "assistant" {
		t.Fatal("non-developer roles were altered")
	}
}

func TestRewriteRequestJSONLeavesOtherChatModelsUntouched(t *testing.T) {
	input := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"developer","content":"sys"}]}`)
	out, changed, err := rewriteWireCompatRequestJSON(wireKindChat, input)
	if err != nil {
		t.Fatalf("rewriteWireCompatRequestJSON() error = %v", err)
	}
	if changed {
		t.Fatal("rewriteWireCompatRequestJSON() changed a non-OpenCode model")
	}
	if string(out) != string(input) {
		t.Fatalf("non-target payload changed: %s", out)
	}
}

func TestBareMuseSparkAliasNoLongerRoutesToOpenCode(t *testing.T) {
	input := []byte(`{"model":"muse-spark-1.3-contributor","input":[{"type":"additional_tools","tools":[{"type":"custom","name":"apply_patch"}]}]}`)
	out, changed, err := rewriteWireCompatRequestJSON(wireKindResponses, input)
	if err != nil {
		t.Fatalf("rewriteWireCompatRequestJSON() error = %v", err)
	}
	if changed {
		t.Fatalf("retired bare muse-spark alias still matched OpenCode transforms: %s", out)
	}
}

// Each harness speaks one wire format. These cases pin that a transform meant
// for one wire never reshapes another wire's payload.

const weatherSchema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

func TestClaudeCodeMessagesToolsKeepSchema(t *testing.T) {
	out := rewriteWireCompatOrFail(t, wireKindMessages, `{"model":"opencode-go/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],
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
	out := rewriteWireCompatOrFail(t, wireKindChat, `{"model":"omen-alpha","messages":[{"role":"developer","content":"sys"}],
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
	out := rewriteWireCompatOrFail(t, wireKindResponses, `{"model":"opencode-go/omen-alpha","input":"hi",
		"tools":[{"type":"function","name":"get_weather","parameters":`+weatherSchema+`}]}`)
	if gjson.GetBytes(out, "tools.0.description").String() != "get_weather" {
		t.Fatalf("description not filled: %s", out)
	}
}

func TestGeminiKeepsToolSchemaFieldsNamedLikeSignatures(t *testing.T) {
	for _, w := range []wireKind{wireKindResponses, wireKindChat, wireKindMessages} {
		body := `{"model":"gemini-3.8-flash","input":"hi","messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"encrypted_content":{"type":"string"}}}}]}`
		out := rewriteWireCompatOrFail(t, w, body)
		if !gjson.GetBytes(out, "tools.0.parameters.properties.encrypted_content").Exists() {
			t.Fatalf("wire %d: tool schema property stripped: %s", w, out)
		}
	}
}

func TestNonOpenCodeModelsUntouched(t *testing.T) {
	for _, model := range []string{"cursor/muse-spark-1.3-high", "gpt-5.6-luna", "claude-sonnet-4-6", "devin/swe-2"} {
		for _, w := range []wireKind{wireKindResponses, wireKindChat, wireKindMessages} {
			body := `{"model":"` + model + `","messages":[{"role":"developer","content":"x"}],"input":[{"type":"additional_tools","tools":[]}],
				"tools":[{"type":"function","name":"n","function":{"name":"n"},"input_schema":{}}]}`
			if _, changed, err := rewriteWireCompatRequestJSON(w, []byte(body)); err != nil || changed {
				t.Fatalf("%s wire %d: changed=%v err=%v", model, w, changed, err)
			}
		}
	}
}

func TestWireKindForPath(t *testing.T) {
	for path, want := range map[string]wireKind{
		"/v1/responses":                wireKindResponses,
		"/responses":                   wireKindResponses,
		"/backend-api/codex/responses": wireKindResponses,
		"/v1/messages":                 wireKindMessages,
		"/messages":                    wireKindMessages,
		"/v1/chat/completions":         wireKindChat,
		"/chat/completions":            wireKindChat,
		"/v1/responses/compact":        wireKindNone,
		"/v1/messages/count_tokens":    wireKindNone,
		"/v1/models":                   wireKindNone,
		"/v1/realtime":                 wireKindNone,
	} {
		if got := wireKindForPath(path); got != want {
			t.Fatalf("%s: got %d, want %d", path, got, want)
		}
	}
}
