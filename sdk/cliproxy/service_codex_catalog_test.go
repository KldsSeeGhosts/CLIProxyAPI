package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRegisterCodexCatalogModelsRetainsOmittedIDs(t *testing.T) {
	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(codexCatalogClientID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(codexCatalogClientID) })

	service := &Service{}
	service.registerCodexCatalogModels()
	if providers := util.GetProviderName("gpt-5.6-sol"); len(providers) == 0 || providers[0] != constant.Codex {
		t.Fatalf("GetProviderName(gpt-5.6-sol) = %#v, want [codex]", providers)
	}

	modelRegistry.RegisterClient(codexCatalogClientID, constant.Codex, []*internalregistry.ModelInfo{
		{ID: "gpt-5.6-sol"},
		{ID: "kept-after-refresh"},
	})
	retained := retainLastKnownGoodModels(codexCatalogClientID, []*ModelInfo{{ID: "gpt-5.6-sol"}})
	got := codexModelIDSet(retained)
	if _, ok := got["gpt-5.6-sol"]; !ok {
		t.Fatal("expected gpt-5.6-sol to remain after partial refresh")
	}
	if _, ok := got["kept-after-refresh"]; !ok {
		t.Fatal("expected omitted last-known-good model to be retained")
	}
}

func TestRegisterCodexCatalogModelsSurvivesAuthUnregister(t *testing.T) {
	modelRegistry := internalregistry.GetGlobalRegistry()
	authID := "codex-oauth-last-known-good.json"
	modelRegistry.UnregisterClient(codexCatalogClientID)
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(codexCatalogClientID)
		modelRegistry.UnregisterClient(authID)
	})

	service := &Service{}
	service.registerCodexCatalogModels()
	modelRegistry.RegisterClient(authID, constant.Codex, []*internalregistry.ModelInfo{{ID: "gpt-5.6-sol"}})
	modelRegistry.UnregisterClient(authID)

	if providers := util.GetProviderName("gpt-5.6-sol"); len(providers) == 0 || providers[0] != constant.Codex {
		t.Fatalf("GetProviderName(gpt-5.6-sol) after auth unregister = %#v, want [codex]", providers)
	}
}

func TestRegisterModelsForAuthRetainsOAuthCodexSnapshotOnEmptyRefresh(t *testing.T) {
	modelRegistry := internalregistry.GetGlobalRegistry()
	authID := "codex-oauth-empty-refresh.json"
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	modelRegistry.RegisterClient(authID, constant.Codex, []*internalregistry.ModelInfo{{ID: "gpt-5.6-sol"}})
	service := &Service{}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: constant.Codex,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAuthKind: coreauth.AuthKindOAuth,
			"plan_type":                "free",
		},
	}

	// Force an empty resolved model list by using a plan that we then replace
	// with a nil registration through registerResolvedModelsForAuth.
	service.registerResolvedModelsForAuth(auth, constant.Codex, nil)
	got := codexModelIDSet(modelRegistry.GetModelsForClient(authID))
	if _, ok := got["gpt-5.6-sol"]; !ok {
		t.Fatalf("registered model IDs = %#v, want gpt-5.6-sol retained", got)
	}
}

func TestRegisterModelsForAuthAPIKeyEmptyStillUnregisters(t *testing.T) {
	modelRegistry := internalregistry.GetGlobalRegistry()
	authID := "codex-apikey-empty-refresh"
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	modelRegistry.RegisterClient(authID, constant.Codex, []*internalregistry.ModelInfo{{ID: "stale-model"}})
	service := &Service{}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: constant.Codex,
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAPIKey:      "stale-key",
			coreauth.AttributeConfigIndex: "0",
			coreauth.AttributeSource:      "config:codex:stale",
		},
	}

	service.registerModelsForAuth(context.Background(), auth)
	if got := modelRegistry.GetModelsForClient(authID); len(got) != 0 {
		t.Fatalf("API key mismatch retained %#v, want unregister", codexModelIDSet(got))
	}
}
