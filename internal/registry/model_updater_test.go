package registry

import "testing"

func TestDetectChangedProviders_KimiAliases(t *testing.T) {
	oldData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}},
	}
	newData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}, {ID: "kimi-k3"}},
	}

	changed := detectChangedProviders(oldData, newData)
	expected := map[string]bool{
		"kimi":     false,
		"kimi-ai":  false,
		"kimi.ai":  false,
		"kimi.com": false,
	}

	for _, p := range changed {
		if _, ok := expected[p]; ok {
			expected[p] = true
		}
	}

	for p, found := range expected {
		if !found {
			t.Errorf("expected changed provider %q to be reported, got %v", p, changed)
		}
	}
}

func TestPreserveEmbeddedClaudeModelsKeepsModelsMissingUpstream(t *testing.T) {
	remote := &staticModelsJSON{Claude: []*ModelInfo{{ID: "claude-opus-5"}}}
	preserveEmbeddedClaudeModels(remote)

	ids := map[string]int{}
	for _, m := range remote.Claude {
		ids[m.ID]++
	}
	if ids["claude-sonnet-5-5"] != 1 {
		t.Fatalf("claude-sonnet-5-5 missing after preserve: %v", ids)
	}
	if ids["claude-opus-5"] != 1 {
		t.Fatalf("remote entry duplicated or lost: %v", ids)
	}
}

func TestPreserveEmbeddedClaudeModelsPrefersRemoteEntry(t *testing.T) {
	remote := &staticModelsJSON{Claude: []*ModelInfo{{ID: "claude-sonnet-5-5", DisplayName: "remote"}}}
	preserveEmbeddedClaudeModels(remote)

	count := 0
	for _, m := range remote.Claude {
		if m.ID == "claude-sonnet-5-5" {
			count++
			if m.DisplayName != "remote" {
				t.Fatalf("embedded entry replaced remote: %q", m.DisplayName)
			}
		}
	}
	if count != 1 {
		t.Fatalf("claude-sonnet-5-5 count = %d, want 1", count)
	}
}
