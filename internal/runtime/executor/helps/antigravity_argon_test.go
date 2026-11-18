package helps

import "testing"

func TestResolveAntigravityArgonModel(t *testing.T) {
	for _, level := range []string{"low", "medium", "high"} {
		payload := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + level + `"}}}}`)
		for _, model := range []string{"gemini-4-argon", "gemini-4-argon-high"} {
			if got := ResolveAntigravityArgonModel(model, payload); got != "gemini-4-argon-"+level {
				t.Fatalf("%s/%s: got %s", model, level, got)
			}
		}
		if got := ResolveAntigravityArgonModel("gemini-3.8-flash-high", payload); got != "gemini-3.8-flash-high" {
			t.Fatal("unrelated model changed")
		}
	}
	if got := ResolveAntigravityArgonModel("gemini-4-argon", nil); got != "gemini-4-argon-high" {
		t.Fatal(got)
	}
}

func TestResolveAntigravityTierModelFamilies(t *testing.T) {
	families := map[string][]string{
		"gemini-4-hydrogen": {"low", "medium", "high", "xhigh"},
		"gemini-4-carbon":   {"low", "medium", "high"},
		"claude-opus-5-5":   {"low", "medium", "high"},
		"claude-sonnet-5-5": {"low", "medium", "high"},
	}
	for base, levels := range families {
		for _, requested := range levels {
			for _, level := range levels {
				payload := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + level + `"}}}}`)
				if got := ResolveAntigravityTierModel(base+"-"+requested, payload); got != base+"-"+level {
					t.Fatalf("%s-%s at %s: got %s", base, requested, level, got)
				}
			}
			if got := ResolveAntigravityTierModel(base+"-"+requested, nil); got != base+"-"+requested {
				t.Fatalf("%s-%s without level: got %s", base, requested, got)
			}
		}
	}
}

func TestResolveAntigravityTierModelKeepsUnpublishedLevels(t *testing.T) {
	payload := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh"}}}}`)
	if got := ResolveAntigravityTierModel("gemini-4-carbon-medium", payload); got != "gemini-4-carbon-medium" {
		t.Fatalf("carbon xhigh: got %s", got)
	}
	if got := ResolveAntigravityTierModel("gemini-4-hydrogen", nil); got != "gemini-4-hydrogen-xhigh" {
		t.Fatalf("bare hydrogen: got %s", got)
	}
	if got := ResolveAntigravityTierModel("claude-opus-5-5", payload); got != "claude-opus-5-5" {
		t.Fatalf("bare opus must stay unchanged: got %s", got)
	}
	if got := ResolveAntigravityTierModel("gemini-4-hydrogen-sonar", payload); got != "gemini-4-hydrogen-sonar" {
		t.Fatalf("sonar: got %s", got)
	}
}
