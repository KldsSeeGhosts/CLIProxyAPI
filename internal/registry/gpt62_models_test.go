package registry

import "testing"

func findModel(models []*ModelInfo, id string) *ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}

func TestGPT62BuiltinsArePaidTierOnly(t *testing.T) {
	for plan, paid := range map[string][]*ModelInfo{"team": GetCodexTeamModels(), "plus": GetCodexPlusModels(), "pro": GetCodexProModels()} {
		checkGPT62(t, plan, paid)
	}
	if findModel(GetCodexFreeModels(), "gpt-6.2-sol") != nil || findModel(GetCodexFreeModels(), "gpt-6.2-astra") != nil {
		t.Fatal("free: GPT 6.2 must not be registered")
	}
}

func checkGPT62(t *testing.T, plan string, pro []*ModelInfo) {
	t.Helper()
	for id, ctx := range map[string]int{"gpt-6.2-sol": 500000, "gpt-6.2-astra": 272000} {
		model := findModel(pro, id)
		if model == nil {
			t.Fatalf("%s: %s missing", plan, id)
		}
		if model.ContextLength != ctx || model.MaxCompletionTokens != 128000 {
			t.Fatalf("%s: unexpected limits %+v", id, model)
		}
		if model.Thinking == nil || len(model.Thinking.Levels) != 6 || model.Thinking.Levels[5] != "ultra" {
			t.Fatalf("%s: unexpected levels %+v", id, model.Thinking)
		}
	}
}

func TestGPT62BuiltinKeepsExistingEntry(t *testing.T) {
	existing := &ModelInfo{ID: "gpt-6.2-sol", ContextLength: 123}
	got := withCodexBuiltinGPT62([]*ModelInfo{existing})
	if model := findModel(got, "gpt-6.2-sol"); model == nil || model.ContextLength != 123 {
		t.Fatalf("existing gpt-6.2-sol replaced: %+v", model)
	}
	if findModel(got, "gpt-6.2-astra") == nil {
		t.Fatal("gpt-6.2-astra not injected without a Sol template")
	}
}
