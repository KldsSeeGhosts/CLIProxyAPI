package helps

import (
	"strings"

	"github.com/tidwall/gjson"
)

// antigravityTierFamily lists the reasoning tiers Google publishes as separate
// model slugs for one Antigravity model family.
type antigravityTierFamily struct {
	levels []string
	// defaultLevel is used for a bare family ID without a reasoning level.
	// An empty value means the bare ID is not an Antigravity route.
	defaultLevel string
}

// antigravityTierFamilies matches fetchAvailableModels on 2026-11-17.
var antigravityTierFamilies = map[string]antigravityTierFamily{
	"gemini-4-argon":    {levels: []string{"low", "medium", "high"}, defaultLevel: "high"},
	"gemini-4-hydrogen": {levels: []string{"low", "medium", "high", "xhigh"}, defaultLevel: "xhigh"},
	"gemini-4-carbon":   {levels: []string{"low", "medium", "high"}, defaultLevel: "high"},
	"claude-opus-5-5":   {levels: []string{"low", "medium", "high"}},
	"claude-sonnet-5-5": {levels: []string{"low", "medium", "high"}},
}

func (f antigravityTierFamily) has(level string) bool {
	for _, candidate := range f.levels {
		if candidate == level {
			return true
		}
	}
	return false
}

// splitAntigravityTierModel returns the family and tier of a tiered model ID.
// The tier is empty for a bare family ID.
func splitAntigravityTierModel(model string) (string, antigravityTierFamily, string, bool) {
	if family, ok := antigravityTierFamilies[model]; ok {
		return model, family, "", true
	}
	cut := strings.LastIndex(model, "-")
	if cut <= 0 {
		return "", antigravityTierFamily{}, "", false
	}
	base, tier := model[:cut], model[cut+1:]
	family, ok := antigravityTierFamilies[base]
	if !ok || !family.has(tier) {
		return "", antigravityTierFamily{}, "", false
	}
	return base, family, tier, true
}

// ResolveAntigravityTierModel maps the canonical reasoning level to Google's
// tier-specific slug before the final payload configuration barrier. A level
// the family does not publish keeps the requested tier.
func ResolveAntigravityTierModel(model string, payload []byte) string {
	base, family, tier, ok := splitAntigravityTierModel(model)
	if !ok {
		return model
	}
	level := strings.ToLower(gjson.GetBytes(payload, "request.generationConfig.thinkingConfig.thinkingLevel").String())
	if family.has(level) {
		return base + "-" + level
	}
	if tier == "" && family.defaultLevel != "" {
		return base + "-" + family.defaultLevel
	}
	return model
}

// ResolveAntigravityArgonModel is kept for existing callers.
func ResolveAntigravityArgonModel(model string, payload []byte) string {
	return ResolveAntigravityTierModel(model, payload)
}
