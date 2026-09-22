package helps

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestIsOpenCodeGo(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		auth    *cliproxyauth.Auth
		want    bool
	}{
		{
			name:    "matches opencode.ai in base url",
			baseURL: "https://opencode.ai/zen/go/v1",
			want:    true,
		},
		{
			name:    "matches opencode in provider",
			baseURL: "https://api.example.com",
			auth:    &cliproxyauth.Auth{Provider: "openai-compatibility:opencode-go"},
			want:    true,
		},
		{
			name:    "matches opencode in compat_name attribute",
			baseURL: "https://api.example.com",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"compat_name": "opencode-go"},
			},
			want: true,
		},
		{
			name:    "matches opencode.ai in auth base_url attribute",
			baseURL: "https://api.example.com",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"base_url": "https://opencode.ai/zen/go/v1"},
			},
			want: true,
		},
		{
			name:    "unrelated provider returns false",
			baseURL: "https://api.openai.com/v1",
			auth:    &cliproxyauth.Auth{Provider: "openai"},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsOpenCodeGo(tt.baseURL, tt.auth); got != tt.want {
				t.Errorf("IsOpenCodeGo() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnsureOpenCodeSession(t *testing.T) {
	t.Run("preserves existing x-opencode-session", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", nil)
		req.Header.Set(OpenCodeSessionHeader, "existing-session-123")

		EnsureOpenCodeSession(req, nil, nil, nil)
		if got := req.Header.Get(OpenCodeSessionHeader); got != "existing-session-123" {
			t.Fatalf("expected existing-session-123, got %q", got)
		}
	})

	t.Run("preserves existing Session-Id", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", nil)
		req.Header.Set("Session-Id", "existing-session-456")

		EnsureOpenCodeSession(req, nil, nil, nil)
		if got := req.Header.Get("Session-Id"); got != "existing-session-456" {
			t.Fatalf("expected existing-session-456, got %q", got)
		}
		if got := req.Header.Get(OpenCodeSessionHeader); got != "" {
			t.Fatalf("expected empty x-opencode-session when Session-Id exists, got %q", got)
		}
	})

	t.Run("uses client x-opencode-session header", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", nil)
		clientHeaders := http.Header{"x-opencode-session": []string{"client-opencode-1"}}

		EnsureOpenCodeSession(req, clientHeaders, nil, nil)
		if got := req.Header.Get(OpenCodeSessionHeader); got != "client-opencode-1" {
			t.Fatalf("expected client-opencode-1, got %q", got)
		}
	})

	t.Run("uses client Claude Code session header", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", nil)
		clientHeaders := http.Header{"X-Claude-Code-Session-Id": []string{"claude-ses-99"}}

		EnsureOpenCodeSession(req, clientHeaders, nil, nil)
		if got := req.Header.Get(OpenCodeSessionHeader); got != "claude-ses-99" {
			t.Fatalf("expected claude-ses-99, got %q", got)
		}
	})

	t.Run("extracts session from payload session_id", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", nil)
		payload := []byte(`{"model":"deepseek-v4-flash","session_id":"body-session-abc"}`)

		EnsureOpenCodeSession(req, nil, payload, nil)
		if got := req.Header.Get(OpenCodeSessionHeader); got != "body-session-abc" {
			t.Fatalf("expected body-session-abc, got %q", got)
		}
	})

	t.Run("extracts stable hash from multi-turn conversation messages", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", nil)
		payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"Hello world"}]}`)

		EnsureOpenCodeSession(req, nil, payload, nil)
		got := req.Header.Get(OpenCodeSessionHeader)
		if got == "" {
			t.Fatal("expected non-empty session header")
		}

		// A second request with the same conversation root should yield the same session header
		req2, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", nil)
		payload2 := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"Hello world"},{"role":"assistant","content":"Hi"},{"role":"user","content":"How are you?"}]}`)
		EnsureOpenCodeSession(req2, nil, payload2, nil)
		got2 := req2.Header.Get(OpenCodeSessionHeader)
		if got != got2 {
			t.Fatalf("expected identical session hashes for same root conversation, got %q vs %q", got, got2)
		}
	})

	t.Run("generates fallback UUID when no context is available", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/go/v1/chat/completions", nil)

		EnsureOpenCodeSession(req, nil, nil, nil)
		got := req.Header.Get(OpenCodeSessionHeader)
		if got == "" {
			t.Fatal("expected non-empty fallback UUID")
		}
		if len(got) < 32 {
			t.Fatalf("unexpected fallback UUID format: %q", got)
		}
	})
}
