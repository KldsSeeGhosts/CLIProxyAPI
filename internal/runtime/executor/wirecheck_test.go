package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Verify the exact header set sent on the wire after doClaudeUpstreamRequest.
func TestWireHeadersAfterSendBoundary(t *testing.T) {
	var got http.Header
	var gotURL string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotURL = r.URL.String()
		w.WriteHeader(200)
	}))
	defer ts.Close()

	auth := &cliproxyauth.Auth{
		ID:         "zai-wire",
		Provider:   "zai",
		Attributes: map[string]string{"zcode_profile": "zcode"},
		Metadata:   map[string]any{"access_token": "key.secret"},
	}
	identity := zcodeSessionIdentity{SessionID: "sess-1", TraceID: "trace-1"}
	ctx := zcodeWithSessionContext(context.Background(), identity)
	ctx = context.WithValue(ctx, zcodeRequestAuthContextKey{}, auth)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/messages?beta=true", nil)
	req.Header.Set("X-App", "cli")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219")
	req.Header.Set("User-Agent", "CLIProxyAPI/dev")
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	_, _ = doClaudeUpstreamRequest(client, req)

	if gotURL != "/v1/messages" {
		t.Errorf("URL = %q, want /v1/messages (beta suffix stripped)", gotURL)
	}
	get := func(name string) string {
		if v, ok := got[name]; ok && len(v) > 0 {
			return v[0]
		}
		return got.Get(name)
	}
	for _, check := range []struct{ name, want string }{
		{"user-agent", "ZCode/3.10.1 ai-sdk/anthropic/3.0.81 ai-sdk/provider-utils/4.0.27 runtime/node.js/v24.14.0"},
		{"http-referer", "https://zcode.z.ai"},
		{"x-zcode-app-version", "3.10.1"},
		{"x-title", "Z Code@electron"},
		{"x-release-channel", "production"},
		{"x-client-language", "en-US"},
		{"x-client-timezone", "America/Detroit"},
		{"x-platform", "linux-x64"},
		{"x-os-category", "linux"},
		{"x-os-version", "7.2.2-1-cachyos"},
		{"x-zcode-agent", "glm"},
		{"x-zcode-session-type", "main"},
		{"x-zcode-trace-id", "trace-1"},
		{"x-session-id", "sess-1"},
		{"x-api-key", "key.secret"},
		{"authorization", "Bearer key.secret"},
		{"accept", "*/*"},
		{"accept-language", "*"},
		{"sec-fetch-mode", "cors"},
		{"accept-encoding", "gzip, deflate"},
	} {
		if gotValue := get(check.name); gotValue != check.want {
			t.Errorf("wire %s = %q, want %q", check.name, gotValue, check.want)
		}
	}
	for _, banned := range []string{"X-App", "X-Stainless-Lang", "Anthropic-Beta"} {
		if get(banned) != "" {
			t.Errorf("wire still carries %s", banned)
		}
	}
	if get("x-request-id") == "" || get("x-query-id") == "" {
		t.Error("missing per-request UUID headers")
	}
}
