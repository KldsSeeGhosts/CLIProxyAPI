package helps

import (
	"strings"

	"github.com/tidwall/gjson"
)

// ResolveAntigravityArgonModel maps the canonical reasoning level to Google's
// tier-specific slug before the final payload configuration barrier.
func ResolveAntigravityArgonModel(model string, payload []byte) string {
	if model != "gemini-4-argon" && model != "gemini-4-argon-low" &&
		model != "gemini-4-argon-medium" && model != "gemini-4-argon-high" {
		return model
	}
	level := strings.ToLower(gjson.GetBytes(payload, "request.generationConfig.thinkingConfig.thinkingLevel").String())
	switch level {
	case "low", "medium", "high":
		return "gemini-4-argon-" + level
	default:
		if model == "gemini-4-argon" {
			return model + "-high"
		}
		return model
	}
}
