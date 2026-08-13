package registry

import "testing"

func TestGetCodexCatalogModelsIncludesGpt56Family(t *testing.T) {
	models := GetCodexCatalogModels()
	if len(models) == 0 {
		t.Fatal("expected Codex catalog models")
	}
	got := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil && model.ID != "" {
			got[model.ID] = struct{}{}
		}
	}
	for _, id := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-image-1.5", "gpt-image-2"} {
		if _, ok := got[id]; !ok {
			t.Errorf("Codex catalog missing %q", id)
		}
	}
}

func TestRetainLastKnownGoodCodexCatalogKeepsEmptyRemoteSections(t *testing.T) {
	old := &staticModelsJSON{
		CodexTeam: []*ModelInfo{{ID: "gpt-5.6-sol", ContextLength: 272000}},
		CodexPro:  []*ModelInfo{{ID: "gpt-5.6-terra", ContextLength: 272000}},
	}
	next := &staticModelsJSON{
		CodexTeam: nil,
		CodexPro:  []*ModelInfo{{ID: "gpt-5.6-luna", ContextLength: 272000}},
	}

	retained := retainLastKnownGoodCodexCatalog(old, next)
	if retained != 1 {
		t.Fatalf("retained = %d, want 1", retained)
	}
	if len(next.CodexTeam) != 1 || next.CodexTeam[0].ID != "gpt-5.6-sol" {
		t.Fatalf("CodexTeam = %#v, want last-known-good gpt-5.6-sol", next.CodexTeam)
	}
	if len(next.CodexPro) != 1 || next.CodexPro[0].ID != "gpt-5.6-luna" {
		t.Fatalf("CodexPro = %#v, want remote gpt-5.6-luna", next.CodexPro)
	}
}
