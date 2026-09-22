package helps

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// OpenCodeSessionHeader is the required routing and session affinity header for OpenCode Go.
const OpenCodeSessionHeader = "x-opencode-session"

// IsOpenCodeGo reports whether the target upstream endpoint or credential corresponds to OpenCode Go.
func IsOpenCodeGo(baseURL string, auth *cliproxyauth.Auth) bool {
	if strings.Contains(strings.ToLower(baseURL), "opencode.ai") {
		return true
	}
	if auth != nil {
		if strings.Contains(strings.ToLower(auth.Provider), "opencode") {
			return true
		}
		if auth.Attributes != nil {
			if strings.Contains(strings.ToLower(auth.Attributes["compat_name"]), "opencode") {
				return true
			}
			if strings.Contains(strings.ToLower(auth.Attributes["base_url"]), "opencode.ai") {
				return true
			}
			if strings.Contains(strings.ToLower(auth.Attributes["provider_key"]), "opencode") {
				return true
			}
		}
	}
	return false
}

// EnsureOpenCodeSession guarantees that requests to OpenCode Go carry a valid x-opencode-session header.
// It prioritizes explicit client session headers, falls back to session extraction from request payloads,
// and finally defaults to a fresh UUID if no session context exists.
func EnsureOpenCodeSession(httpReq *http.Request, clientHeaders http.Header, payload []byte, metadata map[string]any) {
	if httpReq == nil {
		return
	}
	if httpReq.Header.Get(OpenCodeSessionHeader) != "" || httpReq.Header.Get("Session-Id") != "" {
		return
	}

	// 1. Inspect client headers for explicit session tokens (case-insensitive).
	if clientHeaders != nil {
		for _, key := range []string{
			OpenCodeSessionHeader,
			"Session-Id",
			"Session_id",
			"X-Session-ID",
			"X-Session-Affinity",
			"X-Claude-Code-Session-Id",
			"X-Client-Request-Id",
		} {
			if val := strings.TrimSpace(getHeaderCaseInsensitive(clientHeaders, key)); val != "" {
				httpReq.Header.Set(OpenCodeSessionHeader, val)
				return
			}
		}
	}

	// 2. Extract explicit or derived session from metadata / payload.
	if derived := cliproxysession.DerivedID(metadata); derived != "" {
		httpReq.Header.Set(OpenCodeSessionHeader, derived)
		return
	}
	if len(payload) > 0 {
		if derived := cliproxysession.DeriveID(sdktranslator.FormatOpenAI, payload, ""); derived != "" {
			httpReq.Header.Set(OpenCodeSessionHeader, derived)
			return
		}
	}

	sid := cliproxyauth.ExtractSessionID(clientHeaders, payload, metadata)
	if sid != "" {
		if idx := strings.Index(sid, ":"); idx != -1 {
			candidate := strings.TrimSpace(sid[idx+1:])
			if candidate != "" {
				sid = candidate
			}
		}
		httpReq.Header.Set(OpenCodeSessionHeader, sid)
		return
	}

	// 3. Fallback: generate a new UUID for stateless single-turn requests.
	httpReq.Header.Set(OpenCodeSessionHeader, uuid.New().String())
}
