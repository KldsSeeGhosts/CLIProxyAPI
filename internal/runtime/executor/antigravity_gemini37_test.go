package executor

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestResolveGemini37FlashDynamicModel(t *testing.T) {
	tests := []struct {
		name, model, payload, want string
	}{
		{"other model unchanged", "gemini-3.6-flash-high", `{"reasoning_effort":"low"}`, "gemini-3.6-flash-high"},
		{"virtual high", "gemini-3.7-flash", `{"reasoning":{"effort":"high"}}`, "gemini-3.7-flash-high"},
		{"virtual missing effort defaults high", "gemini-3.7-flash", `{}`, "gemini-3.7-flash-high"},
		{"virtual unsupported effort defaults high", "gemini-3.7-flash", `{"reasoning_effort":"xhigh"}`, "gemini-3.7-flash-high"},
		{"virtual low stays on high when low SKU unpublished", "gemini-3.7-flash", `{"reasoning_effort":"low"}`, "gemini-3.7-flash-high"},
		{"virtual medium stays on high when medium SKU unpublished", "gemini-3.7-flash", `{"reasoning":{"effort":"medium"}}`, "gemini-3.7-flash-high"},
		{"virtual minimal stays on high when minimal SKU unpublished", "gemini-3.7-flash", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"minimal"}}}`, "gemini-3.7-flash-high"},
		{"explicit high SKU unchanged", "gemini-3.7-flash-high", `{"reasoning_effort":"low"}`, "gemini-3.7-flash-high"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveGemini37FlashDynamicModel(test.model, []byte(test.payload)); got != test.want {
				t.Fatalf("resolveGemini37FlashDynamicModel() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWithGemini37FlashVirtualModelHidesTierSKUs(t *testing.T) {
	models := []*registry.ModelInfo{
		{ID: "gemini-3.6-flash-high", DisplayName: "Gemini 3.6 Flash"},
		{ID: "gemini-3.7-flash-high", DisplayName: "Gemini 3.7 Flash (High)", Thinking: &registry.ThinkingSupport{Levels: []string{"high"}}},
		{ID: "gemini-3.7-flash-low", DisplayName: "Gemini 3.7 Flash (Low)"},
	}
	got := WithGemini37FlashVirtualModel(models)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (3.6 + virtual)", len(got))
	}
	if got[0].ID != "gemini-3.6-flash-high" {
		t.Fatalf("first = %q, want gemini-3.6-flash-high", got[0].ID)
	}
	virtual := got[1]
	if virtual.ID != "gemini-3.7-flash" || virtual.DisplayName != "Gemini 3.7 Flash" {
		t.Fatalf("virtual = id %q name %q", virtual.ID, virtual.DisplayName)
	}
	if virtual.Thinking == nil {
		t.Fatal("virtual thinking metadata is nil")
	}
	if gotLevels := strings.Join(virtual.Thinking.Levels, ","); gotLevels != "minimal,low,medium,high" {
		t.Fatalf("virtual thinking levels = %q, want minimal,low,medium,high", gotLevels)
	}
	for _, model := range got {
		if isGemini37FlashTierID(model.ID) {
			t.Fatalf("tier SKU leaked into advertised catalog: %q", model.ID)
		}
	}
	if again := WithGemini37FlashVirtualModel(got); len(again) != 2 {
		t.Fatalf("virtual model was added twice: len=%d", len(again))
	}
}

func TestResolveGemini37FlashDynamicModelUsesPublishedLowSKU(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-gemini37-low-sku"
	t.Cleanup(func() { reg.UnregisterClient(clientID) })
	reg.RegisterClient(clientID, "antigravity", []*registry.ModelInfo{{
		ID:      "gemini-3.7-flash-low",
		Object:  "model",
		OwnedBy: "antigravity",
		Type:    "antigravity",
	}})

	got := resolveGemini37FlashDynamicModel("gemini-3.7-flash", []byte(`{"reasoning_effort":"low"}`))
	if got != "gemini-3.7-flash-low" {
		t.Fatalf("resolveGemini37FlashDynamicModel() = %q, want gemini-3.7-flash-low", got)
	}
	fromHigh := resolveGemini37FlashDynamicModel("gemini-3.7-flash-high", []byte(`{"reasoning_effort":"low"}`))
	if fromHigh != "gemini-3.7-flash-low" {
		t.Fatalf("force-mapped high SKU = %q, want gemini-3.7-flash-low", fromHigh)
	}
}
