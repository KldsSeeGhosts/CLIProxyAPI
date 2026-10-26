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
