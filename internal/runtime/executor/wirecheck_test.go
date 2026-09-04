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
	for _, check := range []struct{ name, want string }{
		{"User-Agent", "ZCode/3.10.1"},
		{"HTTP-Referer", "https://zcode.z.ai"},
		{"X-Zcode-App-Version", "3.10.1"},
		{"X-Title", "Z Code@electron"},
		{"X-Release-Channel", "production"},
		{"X-Client-Language", "en-US"},
		{"X-Client-Timezone", "America/Detroit"},
		{"X-Platform", "linux-x64"},
		{"X-Os-Category", "linux"},
		{"X-Os-Version", "7.2.2-1-cachyos"},
		{"X-Zcode-Agent", "glm"},
		{"X-Zcode-Session-Type", "main"},
		{"X-Zcode-Trace-Id", "trace-1"},
		{"X-Session-Id", "sess-1"},
		{"X-Api-Key", "key.secret"},
		{"Authorization", "Bearer key.secret"},
	} {
		if gotValue := got.Get(check.name); gotValue != check.want {
			t.Errorf("wire %s = %q, want %q", check.name, gotValue, check.want)
		}
	}
	for _, banned := range []string{"X-App", "X-Stainless-Lang", "Anthropic-Beta"} {
		if got.Get(banned) != "" {
			t.Errorf("wire still carries %s", banned)
		}
	}
	if got.Get("X-Request-Id") == "" || got.Get("X-Query-Id") == "" {
		t.Error("missing per-request UUID headers")
	}
}
