package management

import (
	"testing"
)

func TestNormalizeZAIQuotaCodingPlanWindows(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"planName": "Coding Pro",
			"limits": []any{
				map[string]any{
					"type":          "CREDIT_LIMIT",
					"unit":          float64(6),
					"number":        float64(1),
					"percentage":    float64(1),
					"usage":         float64(10000),
					"remaining":     float64(9931),
					"nextResetTime": float64(1789053164974),
				},
				map[string]any{
					"type":          "CREDIT_LIMIT",
					"unit":          float64(3),
					"number":        float64(5),
					"percentage":    float64(3),
					"usage":         float64(2000),
					"remaining":     float64(1931),
					"nextResetTime": float64(1788466507374),
				},
			},
		},
	}

	plan, windows := normalizeZAIQuota(payload)
	if plan != "Coding Pro" {
		t.Fatalf("plan = %q, want Coding Pro", plan)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(windows))
	}

	fiveHour := windows[0]
	if fiveHour.Label != "5-hour" || fiveHour.Percentage != 3 {
		t.Fatalf("five-hour window = %#v", fiveHour)
	}
	if fiveHour.Usage == nil || *fiveHour.Usage != 2000 {
		t.Fatalf("five-hour usage = %v, want 2000", fiveHour.Usage)
	}
	if fiveHour.Remaining == nil || *fiveHour.Remaining != 1931 {
		t.Fatalf("five-hour remaining = %v, want 1931", fiveHour.Remaining)
	}
	if fiveHour.PeriodHours == nil || *fiveHour.PeriodHours != 5 {
		t.Fatalf("five-hour period = %v, want 5", fiveHour.PeriodHours)
	}

	weekly := windows[1]
	if weekly.Label != "1 week" || weekly.Percentage != 1 {
		t.Fatalf("weekly window = %#v", weekly)
	}
	if weekly.PeriodHours == nil || *weekly.PeriodHours != 168 {
		t.Fatalf("weekly period = %v, want 168", weekly.PeriodHours)
	}
}

func TestNormalizeZAIQuotaConvertsEpochSeconds(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"limits": []any{
				map[string]any{
					"type":          "TOKENS_LIMIT",
					"unit":          float64(3),
					"number":        float64(5),
					"nextResetTime": float64(1788466507),
				},
			},
		},
	}

	_, windows := normalizeZAIQuota(payload)
	if len(windows) != 1 || windows[0].ResetAtMs == nil {
		t.Fatalf("windows = %#v", windows)
	}
	if got := *windows[0].ResetAtMs; got != 1788466507000 {
		t.Fatalf("reset_at_ms = %d, want 1788466507000", got)
	}
}

func TestZAIWindowLabels(t *testing.T) {
	tests := []struct {
		kind   string
		unit   int64
		number int64
		want   string
	}{
		{kind: "CREDIT_LIMIT", unit: 3, number: 5, want: "5-hour"},
		{kind: "CREDIT_LIMIT", unit: 6, number: 1, want: "1 week"},
		{kind: "CREDIT_LIMIT", unit: 4, number: 2, want: "2 days"},
		{kind: "CREDIT_LIMIT", unit: 5, number: 1, want: "1 month"},
		{kind: "TIME_LIMIT", unit: 5, number: 1, want: "monthly MCP"},
	}

	for _, test := range tests {
		minutes := zaiWindowMinutes(test.unit, test.number)
		if got := zaiWindowLabel(test.kind, test.unit, test.number, minutes); got != test.want {
			t.Errorf("label(%s, %d, %d) = %q, want %q", test.kind, test.unit, test.number, got, test.want)
		}
	}
}
