package registry

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	modelsFetchTimeout    = 30 * time.Second
	modelsRefreshInterval = 3 * time.Hour
)

var modelsURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json",
	"https://models.router-for.me/models.json",
}

//go:embed models/models.json
var embeddedModelsJSON []byte

type modelStore struct {
	mu   sync.RWMutex
	data *staticModelsJSON
}

var modelsCatalogStore = &modelStore{}

var updaterOnce sync.Once

// ModelRefreshCallback is invoked when startup or periodic model refresh detects changes.
// changedProviders contains the provider names whose model definitions changed.
type ModelRefreshCallback func(changedProviders []string)

var (
	refreshCallbackMu     sync.Mutex
	refreshCallback       ModelRefreshCallback
	pendingRefreshChanges []string
)

// SetModelRefreshCallback registers a callback that is invoked when startup or
// periodic model refresh detects changes. Only one callback is supported;
// subsequent calls replace the previous callback.
func SetModelRefreshCallback(cb ModelRefreshCallback) {
	refreshCallbackMu.Lock()
	refreshCallback = cb
	var pending []string
	if cb != nil && len(pendingRefreshChanges) > 0 {
		pending = append([]string(nil), pendingRefreshChanges...)
		pendingRefreshChanges = nil
	}
	refreshCallbackMu.Unlock()

	if cb != nil && len(pending) > 0 {
		cb(pending)
	}
}

func init() {
	// Load embedded data as fallback on startup.
	if err := loadModelsFromBytes(embeddedModelsJSON, "embed"); err != nil {
		log.Warnf("registry: failed to parse embedded models.json (embedded catalog may be incomplete or invalid; continuing startup and will rely on remote model refresh): %v", err)
	}
}

// StartModelsUpdater starts a background updater that fetches models
// immediately on startup and then refreshes the model catalog every 3 hours.
// Safe to call multiple times; only one updater will run.
func StartModelsUpdater(ctx context.Context) {
	updaterOnce.Do(func() {
		go runModelsUpdater(ctx)
	})
}

func runModelsUpdater(ctx context.Context) {
	tryStartupRefresh(ctx)
	periodicRefresh(ctx)
}

func periodicRefresh(ctx context.Context) {
	ticker := time.NewTicker(modelsRefreshInterval)
	defer ticker.Stop()
	log.Infof("periodic model refresh started (interval=%s)", modelsRefreshInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tryPeriodicRefresh(ctx)
		}
	}
}

// tryPeriodicRefresh fetches models from remote, compares with the current
// catalog, and notifies the registered callback if any provider changed.
func tryPeriodicRefresh(ctx context.Context) {
	tryRefreshModels(ctx, "periodic model refresh")
}

// tryStartupRefresh fetches models from remote in the background during
// process startup. It uses the same change detection as periodic refresh so
// existing auth registrations can be updated after the callback is registered.
func tryStartupRefresh(ctx context.Context) {
	tryRefreshModels(ctx, "startup model refresh")
}

func tryRefreshModels(ctx context.Context, label string) {
	oldData := getModels()

	parsed, url := fetchModelsFromRemote(ctx)
	if parsed == nil {
		log.Warnf("%s: fetch failed from all URLs, keeping current data", label)
		return
	}

	if n := enforceOfficialGpt56ContextWindows(parsed); n > 0 {
		log.Infof("%s: clamped %d GPT-5.6 context_length field(s) to %d from %s", label, n, officialGpt56ContextWindow, url)
	}

	if n := migrateRetiredAntigravityModelIDs(parsed); n > 0 {
		log.Warnf("%s: migrated %d retired Antigravity model ID(s) from %s", label, n, url)
	}

	if n := retainLastKnownGoodCodexCatalog(oldData, parsed); n > 0 {
		log.Warnf("%s: retained %d Codex catalog section(s) that the remote snapshot omitted", label, n)
	}

	if n := retainOmittedVertexModels(oldData, parsed); n > 0 {
		log.Warnf("%s: retained %d Vertex model(s) that the remote snapshot omitted", label, n)
	}

	// Detect changes before updating store.
	changed := detectChangedProviders(oldData, parsed)

	// Update store with new data regardless.
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = parsed
	modelsCatalogStore.mu.Unlock()

	if len(changed) == 0 {
		log.Infof("%s completed from %s, no changes detected", label, url)
		return
	}

	log.Infof("%s completed from %s, changes detected for providers: %v", label, url, changed)
	notifyModelRefresh(changed)
}

// fetchModelsFromRemote tries all remote URLs and returns the parsed model catalog
// along with the URL it was fetched from. Returns (nil, "") if all fetches fail.
func migrateRetiredAntigravityModelIDs(models *staticModelsJSON) int {
	if models == nil {
		return 0
	}
	migrated := 0
	for _, model := range models.Antigravity {
		if model == nil || model.ID != "gemini-3.7-flash-high" {
			continue
		}
		model.ID = "gemini-3.7-flash-tiered"
		model.Name = "gemini-3.7-flash-tiered"
		model.Description = "Gemini 3.7 Flash (Tiered)"
		migrated++
	}
	return migrated
}

func fetchModelsFromRemote(ctx context.Context) (*staticModelsJSON, string) {
	client := &http.Client{Timeout: modelsFetchTimeout}
	for _, url := range modelsURLs {
		reqCtx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
		req, err := http.NewRequestWithContext(reqCtx, "GET", url, nil)
		if err != nil {
			cancel()
			log.Debugf("models fetch request creation failed for %s: %v", url, err)
			continue
		}

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			log.Debugf("models fetch failed from %s: %v", url, err)
			continue
		}

		if resp.StatusCode != 200 {
			resp.Body.Close()
			cancel()
			log.Debugf("models fetch returned %d from %s", resp.StatusCode, url)
			continue
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		if err != nil {
			log.Debugf("models fetch read error from %s: %v", url, err)
			continue
		}

		var parsed staticModelsJSON
		if err := json.Unmarshal(data, &parsed); err != nil {
			log.Warnf("models parse failed from %s: %v", url, err)
			continue
		}
		if err := validateModelsCatalog(&parsed); err != nil {
			log.Warnf("models validate failed from %s: %v", url, err)
			continue
		}

		return &parsed, url
	}
	return nil, ""
}

// retainLastKnownGoodCodexCatalog keeps the previous Codex plan sections when a
// remote refresh returns an empty slice for that tier. An empty remote section
// is treated as a failed/partial snapshot, not as a deliberate catalog wipe.
func retainLastKnownGoodCodexCatalog(oldData, newData *staticModelsJSON) int {
	if oldData == nil || newData == nil {
		return 0
	}
	retained := 0
	retain := func(dst *[]*ModelInfo, src []*ModelInfo, name string) {
		if dst == nil || len(*dst) > 0 || len(src) == 0 {
			return
		}
		*dst = cloneModelInfos(src)
		retained++
		log.Warnf("codex catalog refresh omitted %s; retaining last-known-good snapshot (%d models)", name, len(src))
	}
	retain(&newData.CodexFree, oldData.CodexFree, "codex-free")
	retain(&newData.CodexTeam, oldData.CodexTeam, "codex-team")
	retain(&newData.CodexPlus, oldData.CodexPlus, "codex-plus")
	retain(&newData.CodexPro, oldData.CodexPro, "codex-pro")
	return retained
}

// retainOmittedVertexModels keeps locally known Vertex models when a remote
// refresh returns a non-empty Vertex section that simply has not listed them
// yet. Vertex Gemini 3.7 Flash is the current case: the live alias
// vertex/gemini-3.7-flash has nothing to fork from if the ID is dropped.
func retainOmittedVertexModels(oldData, newData *staticModelsJSON) int {
	if oldData == nil || newData == nil {
		return 0
	}
	seen := make(map[string]struct{}, len(newData.Vertex))
	for _, model := range newData.Vertex {
		if model == nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(model.ID))
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
	}
	added := 0
	for _, model := range oldData.Vertex {
		if model == nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(model.ID))
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		newData.Vertex = append(newData.Vertex, cloneModelInfo(model))
		seen[id] = struct{}{}
		added++
	}
	return added
}

// detectChangedProviders compares two model catalogs and returns provider names
// whose model definitions differ. Codex tiers (free/team/plus/pro) are grouped
// under a single "codex" provider.
func detectChangedProviders(oldData, newData *staticModelsJSON) []string {
	if oldData == nil || newData == nil {
		return nil
	}

	type section struct {
		provider string
		oldList  []*ModelInfo
		newList  []*ModelInfo
	}

	sections := []section{
		{"claude", oldData.Claude, newData.Claude},
		{"gemini", oldData.Gemini, newData.Gemini},
		{"vertex", oldData.Vertex, newData.Vertex},
		{"aistudio", oldData.AIStudio, newData.AIStudio},
		{"codex", oldData.CodexFree, newData.CodexFree},
		{"codex", oldData.CodexTeam, newData.CodexTeam},
		{"codex", oldData.CodexPlus, newData.CodexPlus},
		{"codex", oldData.CodexPro, newData.CodexPro},
		{"kimi", oldData.Kimi, newData.Kimi},
		{"antigravity", oldData.Antigravity, newData.Antigravity},
		{"xai", oldData.XAI, newData.XAI},
	}

	seen := make(map[string]bool, len(sections))
	var changed []string
	for _, s := range sections {
		if seen[s.provider] {
			continue
		}
		if modelSectionChanged(s.oldList, s.newList) {
			changed = append(changed, s.provider)
			seen[s.provider] = true
		}
	}
	return changed
}

// modelSectionChanged reports whether two model slices differ.
func modelSectionChanged(a, b []*ModelInfo) bool {
	if len(a) != len(b) {
		return true
	}
	if len(a) == 0 {
		return false
	}
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return true
	}
	return string(aj) != string(bj)
}

func notifyModelRefresh(changedProviders []string) {
	if len(changedProviders) == 0 {
		return
	}

	refreshCallbackMu.Lock()
	cb := refreshCallback
	if cb == nil {
		pendingRefreshChanges = mergeProviderNames(pendingRefreshChanges, changedProviders)
		refreshCallbackMu.Unlock()
		return
	}
	refreshCallbackMu.Unlock()
	cb(changedProviders)
}

func mergeProviderNames(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, provider := range existing {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	for _, provider := range incoming {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	return merged
}

func loadModelsFromBytes(data []byte, source string) error {
	var parsed staticModelsJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("%s: decode models catalog: %w", source, err)
	}
	if n := enforceOfficialGpt56ContextWindows(&parsed); n > 0 {
		log.Infof("registry: clamped %d GPT-5.6 context_length field(s) to %d from %s", n, officialGpt56ContextWindow, source)
	}
	if err := validateModelsCatalog(&parsed); err != nil {
		return fmt.Errorf("%s: validate models catalog: %w", source, err)
	}

	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &parsed
	modelsCatalogStore.mu.Unlock()
	return nil
}

func getModels() *staticModelsJSON {
	modelsCatalogStore.mu.RLock()
	defer modelsCatalogStore.mu.RUnlock()
	return modelsCatalogStore.data
}

func validateModelsCatalog(data *staticModelsJSON) error {
	if data == nil {
		return fmt.Errorf("catalog is nil")
	}

	requiredSections := []struct {
		name   string
		models []*ModelInfo
	}{
		{name: "claude", models: data.Claude},
		{name: "gemini", models: data.Gemini},
		{name: "vertex", models: data.Vertex},
		{name: "aistudio", models: data.AIStudio},
		{name: "codex-free", models: data.CodexFree},
		{name: "codex-team", models: data.CodexTeam},
		{name: "codex-plus", models: data.CodexPlus},
		{name: "codex-pro", models: data.CodexPro},
		{name: "kimi", models: data.Kimi},
		{name: "antigravity", models: data.Antigravity},
		{name: "xai", models: data.XAI},
	}

	for _, section := range requiredSections {
		if err := validateModelSection(section.name, section.models); err != nil {
			return err
		}
	}
	return nil
}

func validateModelSection(section string, models []*ModelInfo) error {
	if len(models) == 0 {
		log.Warnf("models catalog: %s section is empty, continuing without those model definitions", section)
		return nil
	}

	seen := make(map[string]struct{}, len(models))
	for i, model := range models {
		if model == nil {
			return fmt.Errorf("%s[%d] is null", section, i)
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			return fmt.Errorf("%s[%d] has empty id", section, i)
		}
		if _, exists := seen[modelID]; exists {
			return fmt.Errorf("%s contains duplicate model id %q", section, modelID)
		}
		seen[modelID] = struct{}{}
	}
	return nil
}
