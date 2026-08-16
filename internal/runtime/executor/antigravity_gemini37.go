package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const gemini37FlashVirtualID = "gemini-3.7-flash"
const gemini37FlashDefaultSKU = "gemini-3.7-flash-high"

// gemini37FlashThinkingLevels are the Antigravity/Google thinking tiers for
// Gemini 3.7 Flash. Harnesses select the virtual ID `gemini-3.7-flash`; the
// executor remaps onto `gemini-3.7-flash-<level>` when that SKU exists.
var gemini37FlashThinkingLevels = []string{"minimal", "low", "medium", "high"}

func isGemini37FlashFamily(id string) bool {
	return id == gemini37FlashVirtualID || strings.HasPrefix(id, gemini37FlashVirtualID+"-")
}

func isGemini37FlashTierID(id string) bool {
	if !strings.HasPrefix(id, gemini37FlashVirtualID+"-") {
		return false
	}
	switch strings.TrimPrefix(id, gemini37FlashVirtualID+"-") {
	case "minimal", "low", "medium", "high":
		return true
	default:
		return false
	}
}

func applyGemini37FlashVirtualMetadata(info *registry.ModelInfo) {
	if info == nil {
		return
	}
	info.DisplayName = "Gemini 3.7 Flash"
	info.Thinking = &registry.ThinkingSupport{
		Min:            1,
		Max:            65535,
		DynamicAllowed: true,
		Levels:         append([]string(nil), gemini37FlashThinkingLevels...),
	}
}

// WithGemini37FlashVirtualModel advertises one Pi/CPA ID `gemini-3.7-flash`
// with thinking.levels, and hides Antigravity per-tier SKUs
// (`gemini-3.7-flash-{minimal,low,medium,high}`) from discovery. Direct
// requests to a hidden SKU still route via GetProviderName fallback.
func WithGemini37FlashVirtualModel(models []*registry.ModelInfo) []*registry.ModelInfo {
	var template *registry.ModelInfo
	var existing *registry.ModelInfo
	advertised := make([]*registry.ModelInfo, 0, len(models)+1)
	for _, model := range models {
		if model == nil {
			continue
		}
		switch {
		case model.ID == gemini37FlashVirtualID:
			existing = model
			advertised = append(advertised, model)
		case isGemini37FlashTierID(model.ID):
			if template == nil || model.ID == gemini37FlashDefaultSKU {
				template = model
			}
		default:
			advertised = append(advertised, model)
		}
	}
	if existing != nil {
		applyGemini37FlashVirtualMetadata(existing)
		return advertised
	}
	if template == nil {
		return advertised
	}
	virtual := *template
	virtual.ID = gemini37FlashVirtualID
	virtual.Name = gemini37FlashVirtualID
	virtual.Description = "Gemini 3.7 Flash"
	applyGemini37FlashVirtualMetadata(&virtual)
	return append(advertised, &virtual)
}

func gemini37FlashThinkingLevel(payloads ...[]byte) string {
	paths := []string{
		"reasoning.effort",
		"reasoning_effort",
		"thinking.thinkingLevel",
		"thinking.thinking_level",
		"generationConfig.thinkingConfig.thinkingLevel",
		"generationConfig.thinkingConfig.thinking_level",
		"generation_config.thinking_level",
		"extra_body.google.thinking_config.thinking_level",
	}
	for _, payload := range payloads {
		if len(payload) == 0 {
			continue
		}
		for _, path := range paths {
			level := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String()))
			switch level {
			case "minimal", "low", "medium", "high":
				return level
			}
		}
	}
	return "high"
}

func gemini37FlashSKUAvailable(id string) bool {
	if id == gemini37FlashDefaultSKU {
		return true
	}
	for _, model := range registry.GetAntigravityModels() {
		if model != nil && model.ID == id {
			return true
		}
	}
	for _, provider := range registry.GetGlobalRegistry().GetModelProviders(id) {
		if provider == "antigravity" {
			return true
		}
	}
	return false
}

// resolveGemini37FlashDynamicModel maps the virtual Pi ID (or a force-mapped
// default High SKU) onto a per-thinking-level Antigravity slug when Google
// publishes one. If only `gemini-3.7-flash-high` exists, every level stays on
// that SKU and thinking_level is left for the translator.
func resolveGemini37FlashDynamicModel(model string, payloads ...[]byte) string {
	if !isGemini37FlashFamily(model) {
		return model
	}
	level := gemini37FlashThinkingLevel(payloads...)
	candidate := gemini37FlashVirtualID + "-" + level
	if gemini37FlashSKUAvailable(candidate) {
		if candidate != model {
			log.Infof("antigravity: remapped model %s -> %s", model, candidate)
		}
		return candidate
	}
	if isGemini37FlashTierID(model) {
		return model
	}
	if candidate != gemini37FlashDefaultSKU {
		log.Infof("antigravity: remapped model %s -> %s (level %s SKU unavailable)", model, gemini37FlashDefaultSKU, level)
	}
	return gemini37FlashDefaultSKU
}
