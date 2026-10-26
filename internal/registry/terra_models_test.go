package registry

import "testing"

func TestGPT61TerraIsRegisteredForPaidCodexPlans(t *testing.T) {
	plans := map[string][]*ModelInfo{
		"team": GetCodexTeamModels(),
		"plus": GetCodexPlusModels(),
		"pro":  GetCodexProModels(),
	}
	for plan, models := range plans {
		var terra *ModelInfo
		for _, model := range models {
			if model != nil && model.ID == "gpt-6.1-terra" {
				terra = model
			}
		}
		if terra == nil {
			t.Fatalf("%s: gpt-6.1-terra missing", plan)
		}
		if terra.MaxCompletionTokens != 128000 || terra.Thinking == nil || len(terra.Thinking.Levels) != 5 {
			t.Fatalf("%s: unexpected Terra limits %+v", plan, terra)
		}
	}
}

func TestGPT61TerraBuiltinSurvivesRemoteCatalogWithoutIt(t *testing.T) {
	sol := &ModelInfo{ID: "gpt-6.1-sol", ContextLength: 500000, MaxCompletionTokens: 128000, Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}}}
	got := WithCodexBuiltins([]*ModelInfo{sol, {ID: "gpt-6-luna"}})
	var terra *ModelInfo
	for _, model := range got {
		if model.ID == "gpt-6.1-terra" {
			terra = model
		}
	}
	if terra == nil {
		t.Fatal("Terra not injected next to Sol")
	}
	if terra.ContextLength != 272000 || terra.MaxCompletionTokens != 128000 || terra.Thinking == nil || len(terra.Thinking.Levels) != 5 {
		t.Fatalf("unexpected Terra builtin %+v", terra)
	}
	if sol.ContextLength != 500000 {
		t.Fatal("Terra builtin mutated Sol")
	}
	for _, model := range WithCodexBuiltins([]*ModelInfo{{ID: "gpt-6-luna"}}) {
		if model.ID == "gpt-6.1-terra" {
			t.Fatal("Terra injected on a tier without Sol")
		}
	}
}
