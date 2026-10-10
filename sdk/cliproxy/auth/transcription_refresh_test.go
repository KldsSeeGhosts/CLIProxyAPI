package auth

import (
	"context"
	"errors"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"testing"
)

func TestRefreshLocalAuthAfterUnauthorizedReusesUpdatedSnapshot(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &countingRefreshExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	failed := &Auth{ID: "local-oauth", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "expired", "refresh_token": "refresh"}}
	if _, err := manager.Register(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		refreshed, err := manager.RefreshLocalAuthAfterUnauthorized(context.Background(), failed)
		if err != nil || authAccessToken(refreshed) != "refreshed-token" {
			t.Fatalf("refresh failed: %v", err)
		}
	}
	if executor.refreshCalls.Load() != 1 {
		t.Fatal("same failed snapshot refreshed more than once")
	}
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	if _, err := manager.RefreshLocalAuthAfterUnauthorized(context.Background(), failed); err == nil {
		t.Fatal("Home-owned refresh must be rejected")
	}
}

func TestRefreshLocalAuthAfterUnauthorizedCancelsLockWait(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	failed := &Auth{ID: "local-oauth", Provider: "codex", Metadata: map[string]any{"access_token": "expired", "refresh_token": "refresh"}}
	lock := &authRefreshLock{}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	manager.refreshLocks.Store(failed.ID, lock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.RefreshLocalAuthAfterUnauthorized(ctx, failed); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
