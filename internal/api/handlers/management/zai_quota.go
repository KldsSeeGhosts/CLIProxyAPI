package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// zaiQuotaWindow is a normalized coding-plan usage window (5-hour, weekly, ...)
// derived from one upstream limit entry.
type zaiQuotaWindow struct {
	// Kind is the upstream limit type: TOKENS_LIMIT / CREDIT_LIMIT for coding-plan
	// windows, TIME_LIMIT for the MCP lane.
	Kind string `json:"kind"`
	// Label is a human window name: "5-hour", "1 week", "monthly MCP", ...
	Label string `json:"label"`
	// Percentage of the window consumed, clamped to 0-100.
	Percentage int `json:"percentage"`
	// Usage/Remaining are the upstream token counts when provided.
	Usage     *int64 `json:"usage,omitempty"`
	Remaining *int64 `json:"remaining,omitempty"`
	// ResetAtMs is the epoch-millisecond time the window rolls over.
	ResetAtMs *int64 `json:"reset_at_ms,omitempty"`
	// PeriodHours is the nominal window size used by quota timelines.
	PeriodHours *float64 `json:"period_hours,omitempty"`
}

// zaiQuotaAccount is the quota snapshot for one stored zai/bigmodel credential.
type zaiQuotaAccount struct {
	ID       string           `json:"id"`
	Provider string           `json:"provider"`
	Label    string           `json:"label,omitempty"`
	Email    string           `json:"email,omitempty"`
	Plan     string           `json:"plan,omitempty"`
	Windows  []zaiQuotaWindow `json:"windows"`
	Error    string           `json:"error,omitempty"`
	Raw      map[string]any   `json:"raw,omitempty"`
}

// zaiWindowMinutes converts the duration codes used by the monitor API to a
// nominal number of minutes. Z.ai has emitted both 1 and 4 for day windows.
// Month windows use 30 days only for timeline sizing; their reset timestamp
// remains authoritative.
func zaiWindowMinutes(unit, number int64) int64 {
	if number <= 0 {
		return 0
	}
	switch unit {
	case 1, 4:
		return number * 24 * 60
	case 3:
		return number * 60
	case 5:
		return number * 30 * 24 * 60
	case 6:
		return number * 7 * 24 * 60
	default:
		return 0
	}
}

// zaiQuotaHost returns the monitor API host for a stored credential's provider.
func zaiQuotaHost(provider string) string {
	if strings.EqualFold(provider, "bigmodel") {
		return "https://open.bigmodel.cn"
	}
	return "https://api.z.ai"
}

// zaiWindowLabel names a limit entry: "5-hour" for the coding-plan primary
// window, week/day phrasing otherwise, and the MCP lane for TIME_LIMIT.
func zaiWindowLabel(kind string, unit, number, windowMinutes int64) string {
	if kind == "TIME_LIMIT" {
		if unit == 5 && number == 1 {
			return "monthly MCP"
		}
		return "MCP"
	}
	switch {
	case windowMinutes == 300:
		return "5-hour"
	case unit == 6:
		return fmt.Sprintf("%d week%s", number, plural(number))
	case unit == 1 || unit == 4:
		return fmt.Sprintf("%d day%s", number, plural(number))
	case unit == 3:
		return fmt.Sprintf("%d hour%s", number, plural(number))
	case unit == 5:
		return fmt.Sprintf("%d month%s", number, plural(number))
	default:
		return fmt.Sprintf("%d min", windowMinutes)
	}
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// fetchZAIQuota calls the monitor quota API with the credential's API key and
// returns the decoded upstream payload.
func fetchZAIQuota(ctx context.Context, host, apiKey string) (map[string]any, error) {
	url := host + "/api/monitor/usage/quota/limit"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("authorization", "Bearer "+apiKey)
	req.Header.Set("accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload map[string]any
	if errDecode := json.NewDecoder(resp.Body).Decode(&payload); errDecode != nil {
		return nil, fmt.Errorf("decode response: %w", errDecode)
	}
	if success, _ := payload["success"].(bool); !success {
		if msg, _ := payload["msg"].(string); msg != "" {
			return nil, fmt.Errorf("API error: %s", msg)
		}
		return nil, fmt.Errorf("API error: HTTP %d", resp.StatusCode)
	}
	return payload, nil
}

// normalizeZAIQuota converts the upstream data.limits[] entries into windows.
func normalizeZAIQuota(payload map[string]any) (plan string, windows []zaiQuotaWindow) {
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return "", nil
	}
	plan, _ = data["planName"].(string)
	if plan == "" {
		plan, _ = data["plan"].(string)
	}
	limits, _ := data["limits"].([]any)
	for _, item := range limits {
		limit, _ := item.(map[string]any)
		if limit == nil {
			continue
		}
		kind, _ := limit["type"].(string)
		if kind != "TOKENS_LIMIT" && kind != "CREDIT_LIMIT" && kind != "TIME_LIMIT" {
			continue
		}
		unit := zaiJSONInt(limit["unit"])
		number := zaiJSONInt(limit["number"])
		windowMinutes := zaiWindowMinutes(unit, number)
		percentage := int(zaiJSONInt(limit["percentage"]))
		if _, hasPercentage := limit["percentage"]; !hasPercentage {
			if usage := zaiJSONInt(limit["usage"]); usage > 0 {
				used := usage - zaiJSONInt(limit["remaining"])
				if current := zaiJSONInt(limit["currentValue"]); current > 0 && current > used {
					used = current
				}
				if used < 0 {
					used = 0
				}
				if used > usage {
					used = usage
				}
				percentage = int(used * 100 / usage)
			}
		}
		if percentage < 0 {
			percentage = 0
		}
		if percentage > 100 {
			percentage = 100
		}
		window := zaiQuotaWindow{
			Kind:       kind,
			Label:      zaiWindowLabel(kind, unit, number, windowMinutes),
			Percentage: percentage,
		}
		if usage := zaiJSONInt(limit["usage"]); usage > 0 {
			window.Usage = &usage
		}
		if remaining := zaiJSONInt(limit["remaining"]); remaining > 0 {
			window.Remaining = &remaining
		}
		if reset := zaiJSONInt(limit["nextResetTime"]); reset > 0 {
			if reset < 1_000_000_000_000 {
				reset *= 1000
			}
			window.ResetAtMs = &reset
		}
		if windowMinutes > 0 {
			periodHours := float64(windowMinutes) / 60
			window.PeriodHours = &periodHours
		}
		windows = append(windows, window)
	}
	sort.SliceStable(windows, func(i, j int) bool {
		pi, pj := zaiWindowPriority(windows[i]), zaiWindowPriority(windows[j])
		if pi != pj {
			return pi < pj
		}
		return windows[i].Kind < windows[j].Kind
	})
	return plan, windows
}

// zaiWindowPriority orders windows for display: 5-hour, then weekly, then other
// coding-plan windows, then the MCP lane.
func zaiWindowPriority(w zaiQuotaWindow) int {
	switch {
	case w.Label == "5-hour":
		return 0
	case strings.Contains(w.Label, "week"):
		return 1
	case w.Kind == "TIME_LIMIT":
		return 3
	default:
		return 2
	}
}

// zaiJSONInt coerces a JSON number (float64) to int64, 0 when absent/malformed.
func zaiJSONInt(v any) int64 {
	f, ok := v.(float64)
	if !ok {
		return 0
	}
	return int64(f)
}

// GetZAIQuota returns live coding-plan usage windows for every stored Z.AI /
// BigModel credential. Optional ?name=<file> scopes the check to one credential.
func (h *Handler) GetZAIQuota(c *gin.Context) {
	authDir := strings.TrimSpace(h.cfg.AuthDir)
	if authDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth dir not configured"})
		return
	}
	entries, errReadDir := os.ReadDir(authDir)
	if errReadDir != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("read auth dir: %v", errReadDir)})
		return
	}

	wantName := strings.TrimSpace(c.Query("name"))
	accounts := make([]zaiQuotaAccount, 0, 4)
	ctx := c.Request.Context()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "zai-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		if wantName != "" && name != wantName {
			continue
		}
		account := zaiQuotaAccount{ID: name}
		data, errRead := os.ReadFile(filepath.Join(authDir, name))
		if errRead != nil {
			account.Error = fmt.Sprintf("read credential: %v", errRead)
			accounts = append(accounts, account)
			continue
		}
		var stored struct {
			Provider    string `json:"provider"`
			AccessToken string `json:"access_token"`
			Email       string `json:"email"`
			Name        string `json:"name"`
		}
		if errUnmarshal := json.Unmarshal(data, &stored); errUnmarshal != nil {
			account.Error = fmt.Sprintf("parse credential: %v", errUnmarshal)
			accounts = append(accounts, account)
			continue
		}
		account.Provider = stored.Provider
		account.Email = stored.Email
		account.Label = stored.Email
		if account.Label == "" {
			account.Label = stored.Name
		}
		if strings.TrimSpace(stored.AccessToken) == "" {
			account.Error = "credential has no API key"
			accounts = append(accounts, account)
			continue
		}

		payload, errFetch := fetchZAIQuota(ctx, zaiQuotaHost(stored.Provider), stored.AccessToken)
		if errFetch != nil {
			account.Error = errFetch.Error()
			accounts = append(accounts, account)
			continue
		}
		account.Plan, account.Windows = normalizeZAIQuota(payload)
		account.Raw = payload
		accounts = append(accounts, account)
	}

	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	c.JSON(http.StatusOK, gin.H{
		"accounts":   accounts,
		"checked_at": time.Now().UnixMilli(),
	})
}
