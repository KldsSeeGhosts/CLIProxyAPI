package registry

import (
	"encoding/json"
	"testing"
)

func TestEnforceOfficialGpt56ContextWindows(t *testing.T) {
	data := &staticModelsJSON{
		CodexPro: []*ModelInfo{
			{ID: "gpt-5.6-sol", ContextLength: 372000},
			{ID: "gpt-5.5", ContextLength: 272000},
			{ID: "gpt-5.6-terra", ContextLength: 372000},
			{ID: "gpt-5.6-luna", ContextLength: 1000000},
		},
	}

	changed := enforceOfficialGpt56ContextWindows(data)
	if changed != 3 {
		t.Fatalf("changed = %d, want 3", changed)
	}
	for _, model := range data.CodexPro {
		switch model.ID {
		case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
			if model.ContextLength != officialGpt56ContextWindow {
				t.Fatalf("%s context_length = %d, want %d", model.ID, model.ContextLength, officialGpt56ContextWindow)
			}
		case "gpt-5.5":
			if model.ContextLength != 272000 {
				t.Fatalf("gpt-5.5 context_length unexpectedly changed to %d", model.ContextLength)
			}
		}
	}
}

func TestEnforceOfficialGpt56CodexClientContextWindows(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"models": []map[string]any{
			testCodexClientModel("gpt-5.5", 1),
			{
				"slug":                       "gpt-5.6-sol",
				"display_name":               "GPT-5.6 Sol",
				"description":                "Test",
				"base_instructions":          "Test",
				"minimal_client_version":     "0.144.0",
				"visibility":                 "list",
				"context_window":             372000,
				"max_context_window":         372000,
				"priority":                   2,
				"default_reasoning_level":    "medium",
				"supported_reasoning_levels": []map[string]any{{"effort": "medium", "description": "Balanced"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rewritten, changed, err := enforceOfficialGpt56CodexClientContextWindows(raw)
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	if changed != 2 {
		t.Fatalf("changed = %d, want 2", changed)
	}

	var payload codexClientModelsPayload
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("unmarshal rewritten: %v", err)
	}
	found := false
	for _, model := range payload.Models {
		slug, _ := model["slug"].(string)
		if slug != "gpt-5.6-sol" {
			continue
		}
		found = true
		for _, field := range []string{"context_window", "max_context_window"} {
			value, ok := model[field].(float64)
			if !ok || int64(value) != officialGpt56ContextWindow {
				t.Fatalf("%s = %v, want %d", field, model[field], officialGpt56ContextWindow)
			}
		}
	}
	if !found {
		t.Fatal("gpt-5.6-sol missing from rewritten catalog")
	}
}
