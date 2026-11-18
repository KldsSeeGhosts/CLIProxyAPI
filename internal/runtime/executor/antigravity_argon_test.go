package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestArgonEnvelopeUsesHarnessEffortSlug(t *testing.T) {
	for _, level := range []string{"low", "medium", "high"} {
		payload := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + level + `"}},"contents":[{"role":"user","parts":[{"text":"local fixture"}]}]}}`)
		body := geminiToAntigravity("gemini-4-argon-high", payload, "fixture-project")
		if got := gjson.GetBytes(body, "model").String(); got != "gemini-4-argon-"+level {
			t.Fatalf("effort %s maps to %s", level, got)
		}
		if got := gjson.GetBytes(body, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != level {
			t.Fatalf("effort lost: %s", got)
		}
	}
}

func TestArgonResponsesReasoningEffortSelectsTier(t *testing.T) {
	info := &registry.ModelInfo{ID: "gemini-4-argon-high", Thinking: &registry.ThinkingSupport{
		Min: 1, Max: 65535, DynamicAllowed: true, Levels: []string{"low", "medium", "high"},
	}}
	for _, level := range []string{"low", "medium", "high"} {
		source := []byte(`{"model":"gemini-4-argon","reasoning":{"effort":"` + level + `"},"input":"fixture"}`)
		body, err := thinking.ApplyThinkingWithModelInfo([]byte(`{"request":{}}`), source, info.ID, "openai-response", "antigravity", "antigravity", info)
		if err != nil {
			t.Fatal(err)
		}
		body = geminiToAntigravity(info.ID, body, "fixture-project")
		if got := gjson.GetBytes(body, "model").String(); got != "gemini-4-argon-"+level {
			t.Fatalf("%s: %s", level, body)
		}
	}
}

func TestTierEnvelopeUsesHarnessEffortSlug(t *testing.T) {
	for _, tc := range []struct{ requested, level, want string }{
		{"gemini-4-hydrogen-xhigh", "medium", "gemini-4-hydrogen-medium"},
		{"gemini-4-carbon-high", "low", "gemini-4-carbon-low"},
		{"claude-opus-5-5-high", "medium", "claude-opus-5-5-medium"},
		{"claude-sonnet-5-5-high", "low", "claude-sonnet-5-5-low"},
	} {
		payload := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + tc.level + `"}},"contents":[{"role":"user","parts":[{"text":"local fixture"}]}]}}`)
		body := geminiToAntigravity(tc.requested, payload, "fixture-project")
		if got := gjson.GetBytes(body, "model").String(); got != tc.want {
			t.Fatalf("%s at %s maps to %s", tc.requested, tc.level, got)
		}
	}
}
