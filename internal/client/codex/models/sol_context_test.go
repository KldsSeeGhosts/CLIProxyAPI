package models

import "testing"

func TestSolContextDefaultAndExplicitOverride(t *testing.T) {
	for _, limit := range []int{0, 600000} {
		model := map[string]any{"id": "gpt-6.1-sol"}
		if limit > 0 {
			model["max_context_length"] = limit
		}
		response := BuildResponseForClient([]map[string]any{model}, nil, false, "0.162.1")
		got := response["models"].([]map[string]any)[0]
		wantDefault, wantMax := 500000, 872000
		if limit > 0 {
			wantDefault, wantMax = limit, limit
		}
		if intModelValue(got, "context_window") != wantDefault || intModelValue(got, "max_context_window") != wantMax {
			t.Fatalf("override %d: context=%v max=%v", limit, got["context_window"], got["max_context_window"])
		}
	}
}
