package util

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestGetProviderNameRoutesCursorGrok46VirtualID(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-cursor-grok46-virtual-id"
	t.Cleanup(func() { reg.UnregisterClient(clientID) })
	reg.RegisterClient(clientID, "cursor", []*registry.ModelInfo{{
		ID:      "cursor-grok-4.6-high",
		Object:  "model",
		OwnedBy: "cursor",
		Type:    "cursor",
	}})

	got := GetProviderName("cursor-grok-4.6")
	if len(got) == 0 || got[0] != "cursor" {
		t.Fatalf("GetProviderName(cursor-grok-4.6) = %#v, want [cursor]", got)
	}
}

func TestGetProviderNameRoutesHiddenCursorGrok46SKUThroughVirtualID(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-cursor-grok46-hidden-sku"
	t.Cleanup(func() { reg.UnregisterClient(clientID) })
	reg.RegisterClient(clientID, "cursor", []*registry.ModelInfo{{
		ID:      "cursor-grok-4.6",
		Object:  "model",
		OwnedBy: "cursor",
		Type:    "cursor",
	}})

	got := GetProviderName("cursor-grok-4.6-xhigh")
	if len(got) == 0 || got[0] != "cursor" {
		t.Fatalf("GetProviderName(cursor-grok-4.6-xhigh) = %#v, want [cursor]", got)
	}
}

func TestGetProviderNameRoutesGemini37FlashVirtualID(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-gemini37-virtual-id"
	t.Cleanup(func() { reg.UnregisterClient(clientID) })
	reg.RegisterClient(clientID, "antigravity", []*registry.ModelInfo{{
		ID:      "gemini-3.7-flash-high",
		Object:  "model",
		OwnedBy: "antigravity",
		Type:    "antigravity",
	}})

	got := GetProviderName("gemini-3.7-flash")
	if len(got) == 0 || got[0] != "antigravity" {
		t.Fatalf("GetProviderName(gemini-3.7-flash) = %#v, want [antigravity]", got)
	}
}

func TestGetProviderNameRoutesHiddenGemini37FlashSKUThroughVirtualID(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-gemini37-hidden-sku"
	t.Cleanup(func() { reg.UnregisterClient(clientID) })
	reg.RegisterClient(clientID, "antigravity", []*registry.ModelInfo{{
		ID:      "gemini-3.7-flash",
		Object:  "model",
		OwnedBy: "antigravity",
		Type:    "antigravity",
	}})

	got := GetProviderName("gemini-3.7-flash-low")
	if len(got) == 0 || got[0] != "antigravity" {
		t.Fatalf("GetProviderName(gemini-3.7-flash-low) = %#v, want [antigravity]", got)
	}
}
