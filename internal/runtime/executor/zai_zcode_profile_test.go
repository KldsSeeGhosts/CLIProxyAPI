package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// zcodeOpts builds executor options carrying the original OpenAI request.
func zcodeOpts(original string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{OriginalRequest: []byte(original)}
}

// zcodeTestAuth returns a cloned zai auth with the base-URL/x-api-key treatment
// applied, as every ZAIExecutor execution receives.
func zcodeTestAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         id,
		Provider:   "zai",
		Attributes: map[string]string{"zcode_profile": "zcode"},
		Metadata:   map[string]any{"access_token": "key.secret"},
	}
}

func TestZcodeProfileEnabled(t *testing.T) {
	if !zcodeProfileEnabled(zcodeTestAuth("zai-x")) {
		t.Fatal("expected default profile on")
	}
	off := zcodeTestAuth("zai-x")
	off.Attributes["zcode_profile"] = "off"
	if zcodeProfileEnabled(off) {
		t.Fatal("expected profile off")
	}
	if zcodeProfileEnabled(nil) {
		t.Fatal("nil auth must disable the profile")
	}
}

func TestZcodeUpstreamModelNames(t *testing.T) {
	cases := map[string]string{
		"glm-5.3":       "GLM-5.3",
		"glm-5.3-flash": "GLM-5.3-Flash",
		"GLM-5.3":       "GLM-5.3",
		"glm-4.6":       "glm-4.6",
		"unknown-model": "unknown-model",
	}
	for in, want := range cases {
		if got := zcodeUpstreamModel(in); got != want {
			t.Errorf("zcodeUpstreamModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestZcodeNormalizeBodyEffortPairing(t *testing.T) {
	auth := zcodeTestAuth("zai-effort")
	ctx := zcodeWithSessionContext(context.Background(), zcodeSessionIdentity{
		SessionID: "3ec87987-f4ba-4ead-a4c3-666838cb654a",
		TraceID:   "trace-1",
	})

	// Simulate the post-thinking-pipeline body: the generic Claude applier
	// writes adaptive thinking with output_config.effort for level models.
	opts := zcodeOpts(`{"model":"glm-5.3","reasoning_effort":"max","messages":[]}`)
	body := []byte(`{"model":"GLM-5.3","max_tokens":32000,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`)
	out := zcodeNormalizeBody(ctx, body, auth, opts)

	var parsed struct {
		Thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
			Display      string `json:"display"`
		} `json:"thinking"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("normalized body is not valid JSON: %v", err)
	}
	if parsed.Thinking.Type != "enabled" {
		t.Errorf("thinking.type = %q, want enabled", parsed.Thinking.Type)
	}
	if parsed.Thinking.BudgetTokens != 32000 {
		t.Errorf("thinking.budget_tokens = %d, want 32000 for max effort", parsed.Thinking.BudgetTokens)
	}
	if parsed.Thinking.Display != "" {
		t.Errorf("thinking.display = %q, want empty", parsed.Thinking.Display)
	}
	if parsed.OutputConfig.Effort != "max" {
		t.Errorf("output_config.effort = %q, want max", parsed.OutputConfig.Effort)
	}

	// metadata.user_id must be the ZCode JSON string.
	var userID map[string]string
	if err := json.Unmarshal([]byte(parsed.Metadata.UserID), &userID); err != nil {
		t.Fatalf("metadata.user_id is not a JSON string payload: %v (raw %q)", err, parsed.Metadata.UserID)
	}
	if userID["session_id"] != "3ec87987-f4ba-4ead-a4c3-666838cb654a" {
		t.Errorf("session_id = %q, want the ctx session", userID["session_id"])
	}
	if userID["account_uuid"] != "" {
		t.Errorf("account_uuid = %q, want empty", userID["account_uuid"])
	}
	if userID["device_id"] == "" {
		t.Error("device_id missing")
	}
}

func TestZcodeNormalizeBodyEffortLevels(t *testing.T) {
	auth := zcodeTestAuth("zai-levels")
	ctx := zcodeWithSessionContext(context.Background(), zcodeSessionIdentity{SessionID: "sess", TraceID: "t"})
	for effort, want := range map[string]int{"low": 8000, "high": 16000, "max": 32000} {
		opts := zcodeOpts(`{"model":"glm-5.3-flash","reasoning_effort":"` + effort + `","messages":[]}`)
		body := []byte(`{"model":"GLM-5.3-Flash","thinking":{"type":"adaptive"},"output_config":{"effort":"` + effort + `"},"messages":[]}`)
		out := zcodeNormalizeBody(ctx, body, auth, opts)
		var parsed struct {
			Thinking struct {
				Type         string `json:"type"`
				BudgetTokens int    `json:"budget_tokens"`
			} `json:"thinking"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("effort %s: invalid JSON: %v", effort, err)
		}
		if parsed.Thinking.Type != "enabled" || parsed.Thinking.BudgetTokens != want {
			t.Errorf("effort %s: thinking = %+v, want enabled/%d", effort, parsed.Thinking, want)
		}
	}
}

func TestZcodeNormalizeBodyMaxTokensDefault(t *testing.T) {
	auth := zcodeTestAuth("zai-maxtok")
	ctx := zcodeWithSessionContext(context.Background(), zcodeSessionIdentity{SessionID: "s", TraceID: "t"})
	opts := cliproxyexecutor.Options{}
	body := []byte(`{"model":"GLM-5.3","messages":[]}`)
	out := zcodeNormalizeBody(ctx, body, auth, opts)
	if !strings.Contains(string(out), `"max_tokens":128000`) {
		t.Fatalf("expected max_tokens 128000, got %s", out)
	}
	// Explicit max_tokens is preserved when no effort intent asks for the
	// official value.
	body = []byte(`{"model":"GLM-5.3","max_tokens":1024,"messages":[]}`)
	out = zcodeNormalizeBody(ctx, body, auth, opts)
	if !strings.Contains(string(out), `"max_tokens":1024`) {
		t.Fatalf("expected max_tokens preserved, got %s", out)
	}
}

func TestZcodeNormalizeBodyAdaptiveDroppedWithoutEffort(t *testing.T) {
	auth := zcodeTestAuth("zai-adaptive")
	ctx := zcodeWithSessionContext(context.Background(), zcodeSessionIdentity{SessionID: "s", TraceID: "t"})
	opts := zcodeOpts(`{"model":"glm-5.3","messages":[]}`)
	body := []byte(`{"model":"GLM-5.3","thinking":{"type":"adaptive"},"messages":[]}`)
	out := zcodeNormalizeBody(ctx, body, auth, opts)
	if strings.Contains(string(out), "adaptive") {
		t.Fatalf("expected adaptive thinking removed, got %s", out)
	}
}

func TestZcodeDeviceIDStable(t *testing.T) {
	auth := zcodeTestAuth("zai-device")
	first := zcodeDeviceID(auth)
	second := zcodeDeviceID(zcodeTestAuth("zai-device"))
	if first == "" {
		t.Fatal("device id empty")
	}
	if first != second {
		t.Fatalf("device id not deterministic: %q vs %q", first, second)
	}
}

func TestZcodeFinalizeUpstreamRequestHeaders(t *testing.T) {
	auth := zcodeTestAuth("zai-finalize")
	identity := zcodeSessionIdentity{SessionID: "sess-1", TraceID: "trace-1"}
	ctx := zcodeWithSessionContext(context.Background(), identity)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages?beta=true", nil)
	// Claude executor artifacts present.
	req.Header.Set("X-App", "cli")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Claude-Code-Session-Id", "abc")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219")
	req.Header.Set("User-Agent", "CLIProxyAPI/7.2.123")

	zcodeFinalizeUpstreamRequest(req, auth)

	if got := req.Header.Get("User-Agent"); got != "ZCode/3.10.1" {
		t.Errorf("User-Agent = %q, want ZCode/3.10.1", got)
	}
	if got := req.Header.Get("HTTP-Referer"); got != "https://zcode.z.ai" {
		t.Errorf("HTTP-Referer = %q", got)
	}
	if got := req.Header.Get("X-Zcode-Agent"); got != "glm" {
		t.Errorf("X-Zcode-Agent = %q, want glm", got)
	}
	if got := req.Header.Get("X-Zcode-Session-Type"); got != "main" {
		t.Errorf("X-Zcode-Session-Type = %q, want main", got)
	}
	if got := req.Header.Get("X-Session-Id"); got != "sess-1" {
		t.Errorf("X-Session-Id = %q, want sess-1", got)
	}
	if got := req.Header.Get("X-Zcode-Trace-Id"); got != "trace-1" {
		t.Errorf("X-Zcode-Trace-Id = %q, want trace-1", got)
	}
	if req.Header.Get("X-Request-Id") == "" || req.Header.Get("X-Query-Id") == "" {
		t.Error("per-request UUIDs missing")
	}
	// Claude artifacts removed.
	for _, name := range []string{"X-App", "X-Stainless-Lang", "X-Claude-Code-Session-Id", "Anthropic-Beta"} {
		if req.Header.Get(name) != "" {
			t.Errorf("header %q must be stripped, got %q", name, req.Header.Get(name))
		}
	}
	// Dual auth: x-api-key and Bearer both carry the minted key.
	if got := req.Header.Get("X-Api-Key"); got != "key.secret" {
		t.Errorf("x-api-key = %q, want key.secret", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer key.secret" {
		t.Errorf("Authorization = %q, want Bearer key.secret", got)
	}
	if req.URL.RawQuery != "" {
		t.Errorf("RawQuery = %q, want empty (no ?beta=true)", req.URL.RawQuery)
	}
}

func TestZcodeFinalizeUpstreamRequestSkipsWhenDisabled(t *testing.T) {
	auth := zcodeTestAuth("zai-off")
	auth.Attributes["zcode_profile"] = "off"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages", nil)
	req.Header.Set("User-Agent", "CLIProxyAPI/7.2.123")
	zcodeFinalizeUpstreamRequest(req, auth)
	if got := req.Header.Get("User-Agent"); got != "CLIProxyAPI/7.2.123" {
		t.Errorf("profile off must not rewrite headers, got %q", got)
	}
}

func TestZcodeProfileOffKeepsBody(t *testing.T) {
	auth := zcodeTestAuth("zai-off2")
	auth.Attributes["zcode_profile"] = "off"
	body := []byte(`{"model":"GLM-5.3","thinking":{"type":"adaptive"},"messages":[]}`)
	out := zcodeNormalizeBody(context.Background(), body, auth, cliproxyexecutor.Options{})
	if string(out) != string(body) {
		t.Fatalf("profile off must leave body untouched; got %s", out)
	}
}

func TestZcodeRecoverEffortFromBudget(t *testing.T) {
	auth := zcodeTestAuth("zai-recover")
	ctx := zcodeWithSessionContext(context.Background(), zcodeSessionIdentity{SessionID: "s", TraceID: "t"})
	// Body shape after the OpenAI translator's legacy fallback: budget only,
	// with the original intent recoverable from the caller's request.
	opts := zcodeOpts(`{"model":"glm-5.3-flash","reasoning_effort":"max","messages":[]}`)
	body := []byte(`{"model":"GLM-5.3-Flash","max_tokens":4000,"thinking":{"type":"enabled","budget_tokens":32000},"messages":[]}`)
	out := zcodeNormalizeBody(ctx, body, auth, opts)
	var parsed struct {
		Thinking struct {
			BudgetTokens int `json:"budget_tokens"`
		} `json:"thinking"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed.OutputConfig.Effort != "max" || parsed.Thinking.BudgetTokens != 32000 {
		t.Errorf("recovered effort = %q budget = %d, want max/32000", parsed.OutputConfig.Effort, parsed.Thinking.BudgetTokens)
	}
}
