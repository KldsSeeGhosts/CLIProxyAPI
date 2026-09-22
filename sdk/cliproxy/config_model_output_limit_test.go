package cliproxy

import (
	"encoding/json"
	"testing"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"gopkg.in/yaml.v3"
)

func TestConfiguredOutputLimitReachesClientCatalogs(t *testing.T) {
	const modelID = "configured-output-limit-test"
	const source = `name: catalog-test
models:
  - name: upstream-model
    alias: configured-output-limit-test
    max-context-length: 262000
    max-completion-tokens: 128000
    input-modalities: [text]
    thinking:
      levels: [medium, high, max]
`
	var provider config.OpenAICompatibility
	if err := yaml.Unmarshal([]byte(source), &provider); err != nil {
		t.Fatal(err)
	}
	// Management APIs serialize configuration as JSON. Preserve the limit there too.
	raw, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &provider); err != nil {
		t.Fatal(err)
	}
	models := buildOpenAICompatibilityConfigModels(&provider)
	if len(models) != 1 || models[0].OutputTokenLimit != 128000 {
		t.Fatalf("configured models = %+v", models)
	}
	r := registry.GetGlobalRegistry()
	r.RegisterClient(modelID, provider.Name, models)
	t.Cleanup(func() { r.UnregisterClient(modelID) })

	for _, format := range []string{"openai", "claude", "gemini"} {
		found := false
		for _, entry := range r.GetAvailableModels(format) {
			if entry["id"] != modelID {
				continue
			}
			found = true
			field := map[string]string{"openai": "max_completion_tokens", "claude": "max_tokens", "gemini": "outputTokenLimit"}[format]
			if entry[field] != 128000 {
				t.Errorf("%s %s = %v, want 128000", format, field, entry[field])
			}
		}
		if format != "gemini" && !found {
			t.Errorf("model missing from %s catalog", format)
		}
	}
	response := codexmodels.BuildResponseForClient(r.GetAvailableModels("openai"), nil, false, "0.155.1")
	for _, entry := range response["models"].([]map[string]any) {
		if entry["slug"] != modelID {
			continue
		}
		if entry["max_tokens"] != 128000 || entry["context_window"] != 262000 {
			t.Fatalf("Codex limits: output=%v context=%v", entry["max_tokens"], entry["context_window"])
		}
		return
	}
	t.Fatal("model missing from Codex catalog")
}

func TestConfiguredOutputLimitIgnoresNonPositiveValues(t *testing.T) {
	for _, limit := range []int{0, -1} {
		models := buildOpenAICompatibilityConfigModels(&config.OpenAICompatibility{
			Models: []config.OpenAICompatibilityModel{{Name: "test", MaxCompletionTokens: limit}},
		})
		if models[0].MaxCompletionTokens != 0 || models[0].OutputTokenLimit != 0 {
			t.Fatalf("non-positive limit %d was advertised", limit)
		}
	}
}
