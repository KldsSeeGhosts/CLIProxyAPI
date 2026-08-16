package registry

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAI's current official Codex catalog caps GPT-5.6 Sol/Terra/Luna at 272k.
// router-for-me/models still advertises 372k; clamp both local catalogs and remote
// refreshes so compaction/budgeting clients cannot overstate the window.
const officialGpt56ContextWindow = 272000

func isOfficialGpt56ContextModelID(modelID string) bool {
	switch strings.TrimSpace(modelID) {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return true
	default:
		return false
	}
}

func enforceOfficialGpt56ContextWindows(data *staticModelsJSON) int {
	if data == nil {
		return 0
	}

	changed := 0
	for _, section := range [][]*ModelInfo{
		data.CodexFree,
		data.CodexTeam,
		data.CodexPlus,
		data.CodexPro,
	} {
		for _, model := range section {
			if model == nil || !isOfficialGpt56ContextModelID(model.ID) {
				continue
			}
			if model.ContextLength != officialGpt56ContextWindow {
				model.ContextLength = officialGpt56ContextWindow
				changed++
			}
		}
	}
	return changed
}

func enforceOfficialGpt56CodexClientContextWindows(data []byte) ([]byte, int, error) {
	var payload codexClientModelsPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, 0, fmt.Errorf("decode Codex client model catalog for GPT-5.6 context clamp: %w", err)
	}

	changed := 0
	for _, model := range payload.Models {
		slug, _ := model["slug"].(string)
		if !isOfficialGpt56ContextModelID(slug) {
			continue
		}
		for _, field := range []string{"context_window", "max_context_window"} {
			current, ok := model[field].(float64)
			if !ok || int64(current) != officialGpt56ContextWindow {
				model[field] = float64(officialGpt56ContextWindow)
				changed++
			}
		}
	}
	if changed == 0 {
		return data, 0, nil
	}

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("encode Codex client model catalog after GPT-5.6 context clamp: %w", err)
	}
	return rewritten, changed, nil
}
