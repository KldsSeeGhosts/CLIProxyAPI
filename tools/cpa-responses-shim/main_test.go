package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decodeTestJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode rewritten JSON: %v", err)
	}
	return out
}

func TestRewriteResponseJSONStripsGeminiReplayFields(t *testing.T) {
	input := []byte(`{
		"model":"gemini-3.8-flash",
		"input":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"keep"}],"encrypted_content":"stale"},
			{"type":"function_call","thoughtSignature":"stale-too","arguments":"{}"},
			{"role":"user","content":"hello"}
		],
		"metadata":{"encrypted_content":"nested-stale"}
	}`)

	out, changed, err := rewriteResponseJSON(input)
	if err != nil {
		t.Fatalf("rewriteResponseJSON() error = %v", err)
	}
	if !changed {
		t.Fatal("rewriteResponseJSON() changed = false, want true")
	}
	root := decodeTestJSON(t, out)
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
	if _, ok := meta["encrypted_content"]; ok {
		t.Fatal("nested encrypted_content was not removed")
	}
}

func TestRewriteResponseJSONFlattensMuseAdditionalTools(t *testing.T) {
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

	out, changed, err := rewriteResponseJSON(input)
	if err != nil {
		t.Fatalf("rewriteResponseJSON() error = %v", err)
	}
	if !changed {
		t.Fatal("rewriteResponseJSON() changed = false, want true")
	}
	root := decodeTestJSON(t, out)
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

func TestRewriteResponseJSONLeavesOtherModelsUntouched(t *testing.T) {
	input := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","tools":[]}]}`)
	out, changed, err := rewriteResponseJSON(input)
	if err != nil {
		t.Fatalf("rewriteResponseJSON() error = %v", err)
	}
	if changed {
		t.Fatal("rewriteResponseJSON() changed a non-target model")
	}
	if string(out) != string(input) {
		t.Fatalf("non-target payload changed: %s", out)
	}
}

func TestRewriteResponseJSONRejectsInvalidJSON(t *testing.T) {
	if _, _, err := rewriteResponseJSON([]byte(`{"model":`)); err == nil {
		t.Fatal("rewriteResponseJSON() error = nil, want invalid JSON error")
	}
}

func TestRewriteResponseJSONRemapsDeveloperRoleForOmenAlpha(t *testing.T) {
	input := []byte(`{
		"model":"opencode-go/omen-alpha",
		"messages":[
			{"role":"developer","content":"You are an expert coding assistant."},
			{"role":"user","content":[{"type":"text","text":"hello"}]},
			{"role":"assistant","content":"hi"}
		]
	}`)

	out, changed, err := rewriteResponseJSON(input)
	if err != nil {
		t.Fatalf("rewriteResponseJSON() error = %v", err)
	}
	if !changed {
		t.Fatal("rewriteResponseJSON() changed = false, want true")
	}
	root := decodeTestJSON(t, out)
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

func TestRewriteResponseJSONLeavesOtherChatModelsUntouched(t *testing.T) {
	input := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"developer","content":"sys"}]}`)
	out, changed, err := rewriteResponseJSON(input)
	if err != nil {
		t.Fatalf("rewriteResponseJSON() error = %v", err)
	}
	if changed {
		t.Fatal("rewriteResponseJSON() changed a non-OpenCode model")
	}
	if string(out) != string(input) {
		t.Fatalf("non-target payload changed: %s", out)
	}
}

func TestMuseResponseTransformation(t *testing.T) {
	reqJSON := []byte(`{
		"model": "opencode-go/muse-spark-1.3-contributor",
		"tools": [
			{
				"type": "namespace",
				"name": "functions",
				"tools": [
					{"type": "function", "name": "exec_command", "description": "run command"},
					{"type": "custom", "name": "apply_patch", "description": "patch file"}
				]
			},
			{
				"type": "namespace",
				"name": "collaboration",
				"tools": [
					{"type": "function", "name": "spawn_agent", "description": "spawn agent"}
				]
			}
		]
	}`)

	var reqRoot map[string]any
	if err := json.Unmarshal(reqJSON, &reqRoot); err != nil {
		t.Fatalf("unmarshal reqJSON: %v", err)
	}
	ids := museToolIdentities(reqRoot)

	// Verify identity extraction
	if id, ok := ids["functions__exec_command"]; !ok || id.Name != "exec_command" || id.Namespace != "functions" {
		t.Fatalf("expected functions__exec_command mapped, got: %+v", id)
	}
	if id, ok := ids["functions__apply_patch"]; !ok || id.Name != "apply_patch" || id.Namespace != "functions" || id.Kind != "custom" {
		t.Fatalf("expected functions__apply_patch custom mapped, got: %+v", id)
	}
	if id, ok := ids["collaboration__spawn_agent"]; !ok || id.Name != "spawn_agent" || id.Namespace != "collaboration" {
		t.Fatalf("expected collaboration__spawn_agent mapped, got: %+v", id)
	}

	// Test JSON non-streaming restoration
	jsonResp := []byte(`{
		"id": "resp-1",
		"output": [
			{
				"type": "function_call",
				"name": "functions__exec_command",
				"arguments": "{\"cmd\":\"ls\"}"
			},
			{
				"type": "function_call",
				"name": "functions__apply_patch",
				"arguments": "{\"input\":\"*** patch ***\"}"
			}
		]
	}`)

	var respRoot map[string]any
	if err := json.Unmarshal(jsonResp, &respRoot); err != nil {
		t.Fatalf("unmarshal jsonResp: %v", err)
	}
	restoreMuseOutput(respRoot, ids)

	outSlice := respRoot["output"].([]any)
	fc := outSlice[0].(map[string]any)
	if fc["name"] != "exec_command" || fc["namespace"] != "functions" {
		t.Fatalf("expected exec_command with namespace functions, got: %+v", fc)
	}

	ctc := outSlice[1].(map[string]any)
	if ctc["name"] != "apply_patch" || ctc["namespace"] != "functions" || ctc["type"] != "custom_tool_call" || ctc["input"] != "*** patch ***" {
		t.Fatalf("expected custom_tool_call apply_patch, got: %+v", ctc)
	}

	// Test SSE streaming translation
	sseInput := "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"functions__exec_command\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.done\",\"name\":\"functions__exec_command\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"functions__exec_command\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"ctc_1\",\"type\":\"function_call\",\"name\":\"functions__apply_patch\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"ctc_1\",\"name\":\"functions__apply_patch\",\"arguments\":\"{\\\"input\\\":\\\"diff\\\"}\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ctc_1\",\"type\":\"function_call\",\"name\":\"functions__apply_patch\",\"arguments\":\"{\\\"input\\\":\\\"diff\\\"}\"}}\n\n"

	var outBuf bytes.Buffer
	if err := translateMuseSSE(strings.NewReader(sseInput), &outBuf, ids); err != nil {
		t.Fatalf("translateMuseSSE error: %v", err)
	}

	outStr := outBuf.String()
	if !strings.Contains(outStr, `"name":"exec_command"`) || !strings.Contains(outStr, `"namespace":"functions"`) {
		t.Fatalf("expected restored exec_command in SSE output: %s", outStr)
	}
	if strings.Contains(outStr, "functions__exec_command") {
		t.Fatalf("found un-restored functions__exec_command in SSE output: %s", outStr)
	}
	if !strings.Contains(outStr, `"type":"custom_tool_call"`) || !strings.Contains(outStr, `"input":"diff"`) {
		t.Fatalf("expected custom_tool_call in SSE output: %s", outStr)
	}
}
