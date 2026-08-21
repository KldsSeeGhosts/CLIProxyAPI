package executor

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestWithCursorGrok46VirtualModel(t *testing.T) {
	models := []*registry.ModelInfo{
		{ID: "cursor-grok-4.6-high", DisplayName: "Cursor Grok 4.6 High"},
		{ID: "cursor-grok-4.6-low", DisplayName: "Cursor Grok 4.6 Low"},
		{ID: "cursor-grok-4.6-medium", DisplayName: "Cursor Grok 4.6 Medium"},
		{ID: "cursor-grok-4.6-xhigh", DisplayName: "Cursor Grok 4.6 Extra High"},
		{ID: "cursor-small", DisplayName: "Cursor Small"},
	}

	result := WithCursorGrok46VirtualModel(models)
	var foundVirtual bool
	for _, m := range result {
		if m.ID == "cursor-grok-4.6" {
			foundVirtual = true
			if m.DisplayName != "Cursor Grok 4.6" {
				t.Fatalf("DisplayName = %q, want Cursor Grok 4.6", m.DisplayName)
			}
			if m.Thinking == nil || len(m.Thinking.Levels) != 4 {
				t.Fatalf("Thinking = %#v, want 4 levels", m.Thinking)
			}
		}
	}
	if !foundVirtual {
		t.Fatal("expected virtual cursor-grok-4.6 model in result")
	}
}
