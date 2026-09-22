package models

import (
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestCachedTemplatesRecoverFromDifferentSnapshotRevision(t *testing.T) {
	raw, revision := registry.GetCodexClientModelsSnapshot()
	if _, _, err := loadCodexClientModelTemplatesSnapshot(raw, revision+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadCodexClientModelTemplates(); err != nil {
		t.Fatal(err)
	}
	codexClientModelTemplatesMu.Lock()
	defer codexClientModelTemplatesMu.Unlock()
	if codexClientModelTemplatesRevision != revision {
		t.Fatalf("cached revision = %d, want live revision %d", codexClientModelTemplatesRevision, revision)
	}
}

func TestCachedTemplateReadsConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 50 {
				templates, fallback, err := loadCodexClientModelTemplates()
				if err != nil || len(templates) == 0 || fallback == nil {
					t.Errorf("invalid cached templates: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
}
