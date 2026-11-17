package thinking

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestGPT62AcceptsUltraEffort(t *testing.T) {
	var sol *registry.ModelInfo
	for _, model := range registry.GetCodexProModels() {
		if model.ID == "gpt-6.2-sol" {
			sol = model
		}
	}
	if sol == nil {
		t.Fatal("gpt-6.2-sol missing")
	}
	got, err := ValidateConfig(ThinkingConfig{Mode: ModeLevel, Level: "ultra"}, sol, "openai-response", "codex", false)
	if err != nil {
		t.Fatalf("ultra rejected: %v", err)
	}
	if got.Level != "ultra" {
		t.Fatalf("ultra rewritten to %q", got.Level)
	}
}
