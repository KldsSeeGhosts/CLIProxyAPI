package registry

import "testing"

// TestApplyEmbeddedZAIFallback verifies that a remote catalog which omits the zai
// section does not wipe the embedded GLM definitions, while a remote that does
// provide zai keeps its own.
func TestApplyEmbeddedZAIFallback(t *testing.T) {
	saved := embeddedZAIModels
	t.Cleanup(func() { embeddedZAIModels = saved })
	embeddedZAIModels = []*ModelInfo{{ID: "glm-5.2"}}

	// Remote omits zai -> embedded definitions are preserved.
	remote := &staticModelsJSON{}
	if !applyEmbeddedZAIFallback(remote) || len(remote.ZAI) != 1 || remote.ZAI[0].ID != "glm-5.2" {
		t.Fatalf("embedded zai not preserved: %+v", remote.ZAI)
	}

	// Remote provides zai -> its definitions are kept as-is.
	remote2 := &staticModelsJSON{ZAI: []*ModelInfo{{ID: "glm-remote"}}}
	if applyEmbeddedZAIFallback(remote2) || len(remote2.ZAI) != 1 || remote2.ZAI[0].ID != "glm-remote" {
		t.Fatalf("remote zai overwritten: %+v", remote2.ZAI)
	}

	// No embedded fallback available -> no-op.
	embeddedZAIModels = nil
	remote3 := &staticModelsJSON{}
	if applyEmbeddedZAIFallback(remote3) || len(remote3.ZAI) != 0 {
		t.Fatalf("unexpected fallback applied: %+v", remote3.ZAI)
	}
}

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
