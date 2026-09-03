package main

import (
	"encoding/json"
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
