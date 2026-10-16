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
