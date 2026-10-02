package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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

// Exercise both executor refresh and the already-replaced-token shortcut while
// result accounting mutates the manager-owned model map and its entries.
func TestTranscriptionRefreshConcurrentMarkResult(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &countingRefreshExecutor{id: "codex"}
	manager.RegisterExecutor(executor)
	failed := &Auth{ID: "concurrent-oauth", Provider: "codex", Status: StatusActive,
		Metadata:    map[string]any{"access_token": "expired", "refresh_token": "refresh"},
		ModelStates: make(map[string]*ModelState)}
	for i := range 32 {
		failed.ModelStates[fmt.Sprintf("model-%d", i)] = &ModelState{Status: StatusActive}
	}
	if _, err := manager.Register(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := range 1000 {
			manager.MarkResult(context.Background(), Result{AuthID: failed.ID, Provider: "codex", Model: fmt.Sprintf("model-%d", i%32), Success: true})
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range 250 {
			if _, err := manager.ForceRefreshAuth(context.Background(), failed.ID); err != nil {
				t.Errorf("refresh: %v", err)
				return
			}
			if _, err := manager.RefreshLocalAuthAfterUnauthorized(context.Background(), failed); err != nil {
				t.Errorf("reuse refreshed token: %v", err)
				return
			}
		}
	}()
	close(start)
	wg.Wait()
	if executor.refreshCalls.Load() != 250 {
		t.Fatalf("refresh calls=%d, want 250", executor.refreshCalls.Load())
	}
	current, ok := manager.GetByID(failed.ID)
	if !ok || current.Success != 1000 {
		t.Fatalf("result accounting lost: auth=%v", current)
	}
}
