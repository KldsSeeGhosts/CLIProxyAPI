package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestApplyOAuthModelAlias_Rename(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", DisplayName: "Configured GPT Five"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5", DisplayName: "Upstream GPT Five"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].ID != "g5" {
		t.Fatalf("expected model id %q, got %q", "g5", out[0].ID)
	}
	if out[0].Name != "models/g5" {
		t.Fatalf("expected model name %q, got %q", "models/g5", out[0].Name)
	}
	if out[0].DisplayName != "Configured GPT Five" {
		t.Fatalf("expected display name %q, got %q", "Configured GPT Five", out[0].DisplayName)
	}
}

func TestApplyOAuthModelAlias_ForkAddsAlias(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true, DisplayName: "Configured GPT Five"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5", DisplayName: "Upstream GPT Five"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 2 {
		t.Fatalf("expected 2 models, got %d", len(out))
	}
	if out[0].ID != "gpt-5" {
		t.Fatalf("expected first model id %q, got %q", "gpt-5", out[0].ID)
	}
	if out[1].ID != "g5" {
		t.Fatalf("expected second model id %q, got %q", "g5", out[1].ID)
	}
	if out[1].Name != "models/g5" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5", out[1].Name)
	}
	if out[0].DisplayName != "Upstream GPT Five" {
		t.Fatalf("expected original display name %q, got %q", "Upstream GPT Five", out[0].DisplayName)
	}
	if out[1].DisplayName != "Configured GPT Five" {
		t.Fatalf("expected alias display name %q, got %q", "Configured GPT Five", out[1].DisplayName)
	}
}

func TestApplyOAuthModelAlias_PreservesUpstreamDisplayNameByDefault(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", DisplayName: "Upstream GPT Five"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].DisplayName != "Upstream GPT Five" {
		t.Fatalf("expected upstream display name %q, got %q", "Upstream GPT Five", out[0].DisplayName)
	}
}

func TestApplyOAuthModelAlias_ForkAddsMultipleAliases(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5", Alias: "g5", Fork: true},
				{Name: "gpt-5", Alias: "g5-2", Fork: true},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5", Name: "models/gpt-5"},
	}

	out := applyOAuthModelAlias(cfg, "codex", "oauth", models)
	if len(out) != 3 {
		t.Fatalf("expected 3 models, got %d", len(out))
	}
	if out[0].ID != "gpt-5" {
		t.Fatalf("expected first model id %q, got %q", "gpt-5", out[0].ID)
	}
	if out[1].ID != "g5" {
		t.Fatalf("expected second model id %q, got %q", "g5", out[1].ID)
	}
	if out[1].Name != "models/g5" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5", out[1].Name)
	}
	if out[2].ID != "g5-2" {
		t.Fatalf("expected third model id %q, got %q", "g5-2", out[2].ID)
	}
	if out[2].Name != "models/g5-2" {
		t.Fatalf("expected forked model name %q, got %q", "models/g5-2", out[2].Name)
	}
}

func TestApplyOAuthModelAlias_PluginProvider(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"sample-provider": {
				{Name: "sample-model-latest", Alias: "sample-latest"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "sample-model-latest", Name: "models/sample-model-latest"},
	}

	out := applyOAuthModelAlias(cfg, "sample-provider", "oauth", models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].ID != "sample-latest" {
		t.Fatalf("expected plugin alias id %q, got %q", "sample-latest", out[0].ID)
	}
	if out[0].Name != "models/sample-latest" {
		t.Fatalf("expected plugin alias name %q, got %q", "models/sample-latest", out[0].Name)
	}
}

func TestApplyOAuthModelAlias_PluginProviderSkipsAPIKey(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"sample-provider": {
				{Name: "sample-model-latest", Alias: "sample-latest"},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "sample-model-latest", Name: "models/sample-model-latest"},
	}

	out := applyOAuthModelAlias(cfg, "sample-provider", "api_key", models)
	if len(out) != 1 || out[0].ID != "sample-model-latest" {
		t.Fatalf("expected API key plugin model to remain unchanged, got %#v", out)
	}
}

func TestApplyOAuthModelAlias_PerAuthAlias(t *testing.T) {
	models := []*ModelInfo{
		{ID: "gpt-5.3-codex-spark", Name: "models/gpt-5.3-codex-spark"},
	}
	attributes := map[string]string{
		"model_aliases": `[{"name":"gpt-5.3-codex-spark","alias":"gpt-5.5","display-name":"Configured GPT Five"}]`,
	}

	out := applyOAuthModelAliasForAuth(nil, "codex", "oauth", attributes, models)
	if len(out) != 1 {
		t.Fatalf("expected 1 model, got %d", len(out))
	}
	if out[0].ID != "gpt-5.5" {
		t.Fatalf("expected per-auth alias id %q, got %q", "gpt-5.5", out[0].ID)
	}
	if out[0].Name != "models/gpt-5.5" {
		t.Fatalf("expected per-auth alias name %q, got %q", "models/gpt-5.5", out[0].Name)
	}
	if out[0].DisplayName != "Configured GPT Five" {
		t.Fatalf("expected per-auth display name %q, got %q", "Configured GPT Five", out[0].DisplayName)
	}
}

func TestApplyOAuthModelAlias_ContextLengthOverride(t *testing.T) {
	const sourceContextLength = 200000
	const configuredContextLength = 1048576

	models := []*ModelInfo{{
		ID:               "gemini-upstream",
		Name:             "models/gemini-upstream",
		ContextLength:    sourceContextLength,
		MaxContextLength: sourceContextLength,
	}}
	aliases := []config.OAuthModelAlias{
		{Name: "gemini-upstream", Alias: "gemini-visible", MaxContextLength: configuredContextLength},
		{Name: "gemini-upstream", Alias: "gemini-fork", Fork: true, MaxContextLength: configuredContextLength},
	}

	out := applyOAuthModelAliasEntries(aliases, models)
	if len(out) != 3 {
		t.Fatalf("expected source model and 2 aliases, got %d", len(out))
	}
	if out[0].ContextLength != sourceContextLength || out[0].MaxContextLength != sourceContextLength {
		t.Errorf("source model context metadata = (%d, %d), want (%d, %d)", out[0].ContextLength, out[0].MaxContextLength, sourceContextLength, sourceContextLength)
	}
	for _, model := range out[1:] {
		if model.ContextLength != configuredContextLength {
			t.Errorf("%s context length = %d, want %d", model.ID, model.ContextLength, configuredContextLength)
		}
		if model.MaxContextLength != configuredContextLength {
			t.Errorf("%s max context length = %d, want %d", model.ID, model.MaxContextLength, configuredContextLength)
		}
	}
}

func TestApplyOAuthModelAlias_ForceMappingPatchesExistingAliasModel(t *testing.T) {
	const templateContextLength = 200000
	const configuredContextLength = 1048576

	// The force-mapped source model is absent from the registry, but a static
	// template with the alias ID is present and must inherit the configured
	// context capacity.
	models := []*ModelInfo{
		{ID: "claude-visible", ContextLength: templateContextLength, MaxContextLength: templateContextLength},
		{ID: "unrelated", ContextLength: templateContextLength},
	}
	aliases := []config.OAuthModelAlias{
		{Name: "gemini-tiered", Alias: "claude-visible", Fork: true, ForceMapping: true, MaxContextLength: configuredContextLength},
	}

	out := applyOAuthModelAliasEntries(aliases, models)
	if len(out) != 2 {
		t.Fatalf("expected 2 models, got %d", len(out))
	}
	if out[0].ID != "claude-visible" {
		t.Fatalf("first model id = %q, want %q", out[0].ID, "claude-visible")
	}
	if out[0].ContextLength != configuredContextLength || out[0].MaxContextLength != configuredContextLength {
		t.Errorf("force-mapped model context metadata = (%d, %d), want (%d, %d)", out[0].ContextLength, out[0].MaxContextLength, configuredContextLength, configuredContextLength)
	}
	if out[1].ContextLength != templateContextLength {
		t.Errorf("unrelated model context length = %d, want %d", out[1].ContextLength, templateContextLength)
	}
}

func TestApplyOAuthModelAlias_PreservesSourceContextLengthWithoutOverride(t *testing.T) {
	const sourceContextLength = 272000

	models := []*ModelInfo{{
		ID:               "gpt-upstream",
		ContextLength:    sourceContextLength,
		MaxContextLength: sourceContextLength,
	}}
	out := applyOAuthModelAliasEntries([]config.OAuthModelAlias{{
		Name: "gpt-upstream", Alias: "gpt-visible",
	}}, models)
	if len(out) != 1 {
		t.Fatalf("expected 1 alias, got %d", len(out))
	}
	if out[0].ContextLength != sourceContextLength || out[0].MaxContextLength != sourceContextLength {
		t.Errorf("alias context metadata = (%d, %d), want (%d, %d)", out[0].ContextLength, out[0].MaxContextLength, sourceContextLength, sourceContextLength)
	}
}
