package registry

import "testing"

func TestAntigravityBuiltinsPublishVerifiedTiers(t *testing.T) {
	remote := []*ModelInfo{{
		ID: "gemini-4-hydrogen-xhigh", OwnedBy: "antigravity", Type: "antigravity",
		ContextLength: 1048576, MaxCompletionTokens: 1048576, SupportsWebSearch: true,
		Thinking: &ThinkingSupport{Min: 1, Max: 65535, Levels: []string{"low", "medium", "high", "xhigh"}},
	}}
	byID := map[string]*ModelInfo{}
	for _, model := range withAntigravityBuiltins(remote) {
		byID[model.ID] = model
	}
	want := map[string]struct {
		output int
		levels int
	}{
		"gemini-4-argon-low":       {65536, 3},
		"gemini-4-hydrogen-low":    {262144, 4},
		"gemini-4-hydrogen-medium": {262144, 4},
		"gemini-4-hydrogen-xhigh":  {262144, 4},
		"gemini-4-carbon-low":      {262144, 3},
		"gemini-4-carbon-high":     {262144, 3},
		"claude-opus-5-5-low":      {128000, 3},
		"claude-sonnet-5-5-medium": {128000, 3},
	}
	for id, expected := range want {
		model := byID[id]
		if model == nil {
			t.Fatalf("%s missing", id)
		}
		if model.MaxCompletionTokens != expected.output || model.MaxCompletionTokens >= model.ContextLength {
			t.Fatalf("%s output %d context %d", id, model.MaxCompletionTokens, model.ContextLength)
		}
		if model.Thinking == nil || len(model.Thinking.Levels) != expected.levels {
			t.Fatalf("%s levels %+v", id, model.Thinking)
		}
	}
	if !byID["gemini-4-hydrogen-medium"].SupportsWebSearch {
		t.Fatal("hydrogen tiers lost the remote template")
	}
	if remote[0].MaxCompletionTokens != 1048576 {
		t.Fatal("remote entry was mutated")
	}
}
