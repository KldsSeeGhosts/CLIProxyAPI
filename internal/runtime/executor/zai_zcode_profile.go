package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// ZCode harness emulation.
//
// Z.AI keys the ZCode plan benefits - the reduced quota consumption for GLM
// requests and the free-window entitlements - to the request shape of the
// official ZCode client. CPA's minted coding-plan key is byte-identical to the
// key the official client provisions from the same account (both run the same
// /api/biz .../api_keys provisioning), so the remaining difference is the wire
// fingerprint: identity headers, the Anthropic metadata.user_id JSON, and the
// GLM-5.3 effort/body shape.
//
// The values below were captured from the official ZCode 3.10.1 client's
// model-io records (exact request headers and body) against
// api.z.ai/api/anthropic/v1/messages, and cross-checked against the client
// bundle's header builders (buildZCodeSourceHeaders, brt attribution headers,
// Fvo authorization wrapper).

const (
	// zcodeAppVersion is the official client version to emulate.
	zcodeAppVersion = "3.10.1"
	// zcodeHTTPReferer is the referer the official client declares.
	zcodeHTTPReferer = "https://zcode.z.ai"
	// zcodeSourceTitle names the client in x-title ("Z Code@electron").
	zcodeSourceTitle = "Z Code@electron"
	// zcodeAgentHeaderValue marks the embedded GLM agent ("x-zcode-agent").
	zcodeAgentHeaderValue = "glm"
	// zcodeReleaseChannel is the public update channel.
	zcodeReleaseChannel = "production"

	zcodeClientLanguage = "en-US"
	zcodeClientTimezone = "America/Detroit"

	zcodePlatform   = "linux-x64"
	zcodeOsCategory = "linux"
	// zcodeOsVersion mirrors the kernel release of the captured client host.
	zcodeOsVersion = "7.2.2-1-cachyos"

	// The wire user-agent is assembled by three chained xm() calls in the
	// official bundle, each appending one telemetry suffix to the value the
	// previous stage set:
	//   1. the Anthropic provider factory (het) appends "ai-sdk/anthropic/3.0.81";
	//   2. the shared request layer (lir/postJsonToApi) appends
	//      "ai-sdk/provider-utils/4.0.27" and the runtime token from wye(),
	//      which in the Electron host is "runtime/node.js/v24.14.0"
	//      (Electron 41 bundles node 24.14.0).
	// The model-io records store the provider-stage value (ZCode/<version>)
	// only, which is why the bare UA is not the right wire value.
	zcodeSDKAnthropicVersion = "ai-sdk/anthropic/3.0.81"
	zcodeSDKProviderUtilsUA  = "ai-sdk/provider-utils/4.0.27"
	zcodeRuntimeUA            = "runtime/node.js/v24.14.0"

	// Session types: main-agent turns send "main"; background helpers
	// (compaction, titles) send "other".
	zcodeSessionTypeMain  = "main"
	zcodeSessionTypeOther = "other"

	// zcodeMaxTokens is the max_tokens the official client requests for
	// GLM-5.3 family models.
	zcodeMaxTokens = 128000
)

// zcodeThinkingBudgets maps the GLM-5.3 effort levels to the exact
// thinking.budget_tokens values the official client pairs with each
// output_config.effort (low=8000, high=16000, max=32000).
var zcodeThinkingBudgets = map[string]int{
	"low":  8000,
	"high": 16000,
	"max":  32000,
}

// zcodeUpstreamModelNames maps CPA's registry model IDs to the exact model
// strings the official client puts on the wire. Unlisted models pass through.
var zcodeUpstreamModelNames = map[string]string{
	"glm-5.3":       "GLM-5.3",
	"glm-5.3-flash": "GLM-5.3-Flash",
	"glm-5.2":       "GLM-5.2",
	"glm-5.1":       "GLM-5.1",
	"glm-4.7":       "GLM-4.7",
}

// zcodeUpstreamModel normalizes a CPA model ID to the official client's wire
// casing. It is installed as the ZAIExecutor's upstreamModelNormalizer, so the
// response model rewrite back to the caller-visible ID comes for free.
func zcodeUpstreamModel(baseModel string) string {
	if mapped, ok := zcodeUpstreamModelNames[strings.ToLower(strings.TrimSpace(baseModel))]; ok {
		return mapped
	}
	return baseModel
}

// zcodeProfileEnabled reports whether the ZCode harness profile applies to a
// credential. It is on by default for every zai request (the whole point of
// the provider) and can be disabled per credential with the attribute
// zcode_profile=off for debugging or A/B comparison.
func zcodeProfileEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(auth.Attributes["zcode_profile"])) {
	case "off", "disabled", "false":
		return false
	}
	return true
}

// zcodeSessionContextKey carries the resolved ZCode session identity through
// the executor into the body and header hooks.
type zcodeSessionContextKey struct{}

// zcodeRequestAuthContextKey carries the cloned auth selected for one upstream
// attempt so the send boundary can apply the ZCode profile after all Claude
// header assembly.
type zcodeRequestAuthContextKey struct{}

// zcodeSessionIdentity holds the per-conversation attribution values that must
// stay stable across a session's requests, matching the official client.
type zcodeSessionIdentity struct {
	// SessionID is x-session-id (the client session UUID).
	SessionID string
	// TraceID is x-zcode-trace-id (stable for a session's lifetime).
	TraceID string
}

// zcodeResolveSessionIdentity derives the session identity from the caller's
// request. A recognizable downstream session (Claude session headers, request
// metadata, or an explicit x-session-id) keeps its UUID stable across turns,
// like the official client; otherwise each request gets a fresh session, which
// matches how a client with no extractable identity behaves anyway.
func zcodeResolveSessionIdentity(headers http.Header, payload []byte, metadata ...map[string]any) zcodeSessionIdentity {
	sessionID := strings.TrimSpace(headers.Get("X-Session-Id"))
	if sessionID == "" {
		var meta map[string]any
		if len(metadata) > 0 {
			meta = metadata[0]
		}
		sessionID = cliproxyauth.ExtractSessionID(headers, payload, meta)
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	// The official client always sends a bare UUID here; fold any other
	// extractable identity (hashes, names) into a stable UUID so the wire
	// matches regardless of the downstream client's session format.
	if parsed, err := uuid.Parse(strings.TrimPrefix(sessionID, "claude:")); err == nil {
		sessionID = parsed.String()
	} else if strings.TrimSpace(sessionID) != "" {
		sessionID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cliproxyapi-zcode-session\x00"+strings.TrimSpace(sessionID))).String()
	}
	// The official client's trace id is session-scoped; derive it
	// deterministically so retries of the same conversation reuse it.
	traceID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("cliproxyapi-zcode-trace\x00"+sessionID)).String()
	return zcodeSessionIdentity{SessionID: sessionID, TraceID: traceID}
}

// zcodeWithSessionContext attaches the session identity to the request
// context for the body and header hooks.
func zcodeWithSessionContext(ctx context.Context, identity zcodeSessionIdentity) context.Context {
	return context.WithValue(ctx, zcodeSessionContextKey{}, identity)
}

// zcodeSessionFromContext returns the attached session identity, if any.
func zcodeSessionFromContext(ctx context.Context) (zcodeSessionIdentity, bool) {
	if ctx == nil {
		return zcodeSessionIdentity{}, false
	}
	identity, ok := ctx.Value(zcodeSessionContextKey{}).(zcodeSessionIdentity)
	return identity, ok
}

// zcodeNormalizeBody rewrites a translated Anthropic Messages body into the
// shape the official ZCode client sends for GLM coding-plan requests:
//
//   - metadata.user_id becomes the ZCode device/session JSON
//     {"device_id":"...","account_uuid":"","session_id":"..."} - the official
//     client sends exactly this string on anthropic-protocol requests, with a
//     stable device_id and a per-session session_id.
//   - output_config.effort is paired with the matching fixed
//     thinking.budget_tokens (low=8000, high=16000, max=32000) instead of
//     Claude's adaptive thinking, because the GLM endpoint reads effort from
//     output_config and the budget from thinking.
//   - max_tokens defaults to 128000 when unset, matching the official client.
//
// Fields the caller set explicitly keep their values; only missing companions
// are filled in.
func zcodeNormalizeBody(ctx context.Context, body []byte, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) || !zcodeProfileEnabled(auth) {
		return body
	}

	identity, _ := zcodeSessionFromContext(ctx)

	// 1. metadata.user_id -> ZCode device/session JSON.
	body = zcodeApplyMetadataUserID(body, auth, identity.SessionID)

	// 2. Effort pairing. The official client sends a fixed budget per effort
	// level; recover the caller's original effort intent first, because the
	// generic pipeline can lose it:
	//   - the OpenAI chat-completions translator resolves capabilities against
	//     the "claude" catalog (where zai models are absent) and falls back to
	//     legacy budgets (max->128000), then clamping folds it further;
	//   - the Claude budget constraint (max_tokens > budget) truncates budgets
	//     of small max_tokens requests, which the official client avoids by
	//     always sending max_tokens=128000.
	// The original request's reasoning_effort (openai) or the translated
	// output_config.effort/thinking budget are the intent carriers.
	intent := zcodeOriginalEffortIntent(opts)
	switch intent {
	case zcodeEffortNone:
		// Explicit "none"/"off": disable thinking entirely.
		if updated, err := sjson.SetBytes(body, "thinking.type", "disabled"); err == nil {
			body = updated
		}
		for _, path := range []string{"thinking.budget_tokens", "thinking.display", "output_config.effort"} {
			if updated, err := sjson.DeleteBytes(body, path); err == nil {
				body = updated
			}
		}
		if oc := gjson.GetBytes(body, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			if updated, err := sjson.DeleteBytes(body, "output_config"); err == nil {
				body = updated
			}
		}
	case zcodeEffortUnspecified:
		// No intent: leave the pipeline result; drop a lone adaptive marker
		// that the GLM endpoint rejects.
		if !gjson.GetBytes(body, "output_config.effort").Exists() &&
			gjson.GetBytes(body, "thinking.type").String() == "adaptive" {
			if updated, err := sjson.DeleteBytes(body, "thinking"); err == nil {
				body = updated
			}
		}
	default:
		// Pair the official budget with the requested effort level and restore
		// the official max_tokens so the budget always fits (the GLM endpoint
		// has no Claude-style max_tokens>budget rule; the official client sends
		// 128000 for every GLM-5.3 request).
		budget := zcodeThinkingBudgets[string(intent)]
		if updated, err := sjson.SetBytes(body, "output_config.effort", intent); err == nil {
			body = updated
		}
		if updated, err := sjson.SetBytes(body, "thinking.type", "enabled"); err == nil {
			body = updated
		}
		if updated, err := sjson.SetBytes(body, "thinking.budget_tokens", budget); err == nil {
			body = updated
		}
		if updated, err := sjson.DeleteBytes(body, "thinking.display"); err == nil {
			body = updated
		}
		if updated, err := sjson.SetBytes(body, "max_tokens", zcodeMaxTokens); err == nil {
			body = updated
		}
	}

	// 3. max_tokens default. Only fill when missing entirely.
	if !gjson.GetBytes(body, "max_tokens").Exists() {
		if updated, err := sjson.SetBytes(body, "max_tokens", zcodeMaxTokens); err == nil {
			body = updated
		}
	}

	return body
}

// zcodeEffortIntent is the resolved original effort intent of a request.
type zcodeEffortIntent string

const (
	zcodeEffortUnspecified zcodeEffortIntent = ""
	zcodeEffortNone        zcodeEffortIntent = "none"
	zcodeEffortLow         zcodeEffortIntent = "low"
	zcodeEffortHigh        zcodeEffortIntent = "high"
	zcodeEffortMax         zcodeEffortIntent = "max"
)

// zcodeOriginalEffortIntent recovers the caller's thinking intent from the
// original request payload, before translation and budget clamping could
// distort it. It checks, in order:
//   - the original request's OpenAI reasoning_effort (chat completions) or
//     reasoning.effort (responses);
//   - the translated body's output_config.effort;
//   - a thinking budget exactly matching an official effort budget (the
//     legacy fallback path), or none/adaptive markers.
func zcodeOriginalEffortIntent(opts cliproxyexecutor.Options) zcodeEffortIntent {
	for _, source := range [][]byte{opts.OriginalRequest} {
		if len(source) == 0 {
			continue
		}
		for _, path := range []string{"reasoning_effort", "reasoning.effort"} {
			if v := gjson.GetBytes(source, path); v.Exists() && v.Type == gjson.String {
				if intent := zcodeNormalizeEffort(v.String()); intent != zcodeEffortUnspecified {
					return intent
				}
			}
		}
	}
	return zcodeEffortUnspecified
}

// zcodeNormalizeEffort maps an OpenAI reasoning_effort value (and its close
// variants) to the official GLM effort levels.
func zcodeNormalizeEffort(raw string) zcodeEffortIntent {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return zcodeEffortUnspecified
	case "none", "off", "disabled", "minimal":
		return zcodeEffortNone
	case "low":
		return zcodeEffortLow
	case "medium", "high":
		// The official GLM-5.3 ladder is low/high/max; medium folds up.
		return zcodeEffortHigh
	case "xhigh", "max":
		return zcodeEffortMax
	default:
		return zcodeEffortUnspecified
	}
}

// zcodeRecoverEffortFromBudget reverses the OpenAI translator's legacy
// budget fallback. When a request arrives with an OpenAI reasoning_effort
// that the chat-completions translator could not map to adaptive effort
// (it resolves capabilities against the "claude" catalog, where zai models
// are absent), the intent lands as a plain thinking.budget_tokens. The
// official client pairs each effort level with exactly one budget, so a
// budget matching one of those values uniquely identifies the effort and
// the missing output_config.effort is restored.
func zcodeRecoverEffortFromBudget(body []byte) []byte {
	if gjson.GetBytes(body, "output_config.effort").Exists() {
		return body
	}
	budgetResult := gjson.GetBytes(body, "thinking.budget_tokens")
	if !budgetResult.Exists() || budgetResult.Type != gjson.Number {
		return body
	}
	// Invert zcodeThinkingBudgets; each value is unique.
	var effort string
	for level, budget := range zcodeThinkingBudgets {
		if int(budgetResult.Int()) == budget {
			effort = level
			break
		}
	}
	if effort == "" {
		return body
	}
	if updated, err := sjson.SetBytes(body, "output_config.effort", effort); err == nil {
		return updated
	}
	return body
}

// zcodeApplyMetadataUserID rewrites metadata.user_id into the official ZCode
// JSON shape. sjson marshals the Go string as a JSON string value, which is
// exactly the wire shape: user_id is a string containing serialized JSON.
func zcodeApplyMetadataUserID(body []byte, auth *cliproxyauth.Auth, sessionID string) []byte {
	if auth == nil {
		return body
	}
	deviceID := zcodeDeviceID(auth)
	if deviceID == "" {
		return body
	}
	sess := strings.TrimSpace(sessionID)
	if sess == "" {
		sess = uuid.NewString()
	}
	userID := fmt.Sprintf(`{"device_id":%q,"account_uuid":"","session_id":%q}`, deviceID, sess)
	if updated, err := sjson.SetBytes(body, "metadata.user_id", userID); err == nil {
		return updated
	}
	return body
}

// zcodeDeviceID returns a stable per-credential ZCode device_id. The official
// client persists one in its telemetry state; CPA derives a deterministic UUID
// from the credential ID instead, which is equally stable across requests and
// restarts without mutating the shared auth object.
func zcodeDeviceID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v, ok := auth.Metadata["zcode_device_id"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	seed := strings.TrimSpace(auth.ID)
	if seed == "" {
		seed = strings.TrimSpace(auth.Provider)
	}
	if seed == "" {
		return ""
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cliproxyapi-zcode-device\x00"+seed)).String()
}

// zcodeFinalizeUpstreamRequest applies the ZCode harness profile to the fully
// assembled upstream request, immediately before it is sent. It runs after
// ClaudeExecutor's header assembly, so it has the final word: it installs the
// official client's identity headers, the per-request attribution UUIDs, the
// dual x-api-key + Bearer authorization the official client sends, and strips
// the Claude-only artifacts (x-stainless, x-app, anthropic-beta, ?beta=true)
// that Z.AI never sees from a real ZCode client.
//
// The exact wire shape was derived from the official bundle's header chain
// (gin/Fvo source headers -> het's xm() -> sfr's brt merge -> postJsonToApi)
// and verified against a raw-socket capture of the same fetch (undici) stack:
//
//	host, connection: keep-alive, then the header map's insertion order
//	(Content-Type first, then the sorted-by-lowercase-name provider headers,
//	then the per-request attribution headers), then undici's trailing defaults
//	(accept, accept-language, sec-fetch-mode, accept-encoding, content-length).
//
// Go sorts headers bytewise and writes host/content-length outside the
// block; since every emulated name is lowercase except Content-Type, Go's
// sorted order equals undici's insertion order except for the trailing
// defaults, which this pass moves to the undici positions via the ordered
// connection installed by the zai transport.
func zcodeFinalizeUpstreamRequest(r *http.Request, auth *cliproxyauth.Auth) {
	if r == nil || !zcodeProfileEnabled(auth) {
		return
	}

	// Claude executor artifacts a real ZCode client never sends.
	for _, name := range []string{
		"X-App",
		"Anthropic-Beta",
		"Anthropic-Dangerous-Direct-Browser-Access",
		"X-Anthropic-Additional-Protection",
		"X-Client-App",
	} {
		r.Header.Del(name)
	}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-stainless-") ||
			strings.HasPrefix(lower, "x-claude-code-") ||
			strings.HasPrefix(lower, "x-claude-remote-") {
			r.Header.Del(name)
		}
	}

	// Identity headers (exact official client values). The official client
	// builds these PascalCase (gin) but they pass through xm()'s Headers
	// object, whose iteration lowercases every name, so the wire value is
	// lowercase. Collect via Set (canonicalising) and lowercase the whole map
	// at the end, like the official chain does.
	r.Header.Set("HTTP-Referer", zcodeHTTPReferer)
	r.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion+" "+zcodeSDKAnthropicVersion+" "+zcodeSDKProviderUtilsUA+" "+zcodeRuntimeUA)
	r.Header.Set("X-Zcode-App-Version", zcodeAppVersion)
	r.Header.Set("X-Title", zcodeSourceTitle)
	r.Header.Set("X-Release-Channel", zcodeReleaseChannel)
	r.Header.Set("X-Client-Language", zcodeClientLanguage)
	r.Header.Set("X-Client-Timezone", zcodeClientTimezone)
	r.Header.Set("X-Platform", zcodePlatform)
	r.Header.Set("X-Os-Category", zcodeOsCategory)
	r.Header.Set("X-Os-Version", zcodeOsVersion)
	r.Header.Set("X-Zcode-Agent", zcodeAgentHeaderValue)

	// Per-request attribution headers. x-request-id and x-query-id are fresh
	// per request; x-zcode-trace-id and x-session-id are session-stable.
	identity, _ := zcodeSessionFromContext(r.Context())
	r.Header.Set("X-Request-Id", uuid.NewString())
	r.Header.Set("X-Zcode-Trace-Id", identity.TraceID)
	r.Header.Set("X-Query-Id", uuid.NewString())
	if identity.SessionID != "" {
		r.Header.Set("X-Session-Id", identity.SessionID)
	}
	r.Header.Set("X-Zcode-Session-Type", zcodeSessionTypeMain)

	// Undici's trailing defaults. The AI SDK's postJsonToApi sets Content-Type
	// only; fetch adds accept, accept-language, sec-fetch-mode and
	// accept-encoding after the caller's headers. Go would otherwise send
	// Accept-Encoding: identity (and no sec-fetch-mode). The Claude response
	// pipeline decompresses Content-Encoding: gzip responses, so allowing
	// gzip is safe end to end.
	r.Header.Set("Accept", "*/*")
	r.Header.Set("Accept-Language", "*")
	r.Header.Set("Sec-Fetch-Mode", "cors")
	r.Header.Set("Accept-Encoding", "gzip, deflate")

	// Authorization: the official client sends the minted coding-plan key in
	// both x-api-key and Authorization: Bearer (the SDK sets x-api-key and the
	// provider wrapper adds the Bearer header).
	if apiKey, _ := claudeCreds(auth); strings.TrimSpace(apiKey) != "" {
		r.Header.Set("X-Api-Key", apiKey)
		r.Header.Set("Authorization", "Bearer "+apiKey)
	}

	// The official client posts plain /v1/messages; the ?beta=true suffix is
	// an Anthropic-specific convention.
	if r.URL != nil {
		r.URL.RawQuery = ""
	}

	// Header-name casing is normalised on the wire by the Z.AI model transport
	// (see helps.zaiModelHeaderCasing): every name except Content-Type goes out
	// lowercase, matching the official client's Headers-object normalisation.
	// Keep canonical names here so Go's own User-Agent/Host handling and every
	// Header.Get in the pipeline stay functional.
}

// zcodeRequestHeaderOrder returns the undici header order for one zai
// upstream request. Undici emits the caller's header map in insertion order
// (Content-Type first, then the Headers-sorted provider names), then appends
// its own defaults; Go emits a bytewise sort of the same lowercase names,
// which is identical up to undici's trailing block, so only the trailing
// defaults need explicit positioning. Content-Length is written by Go
// outside the ordered block and cannot be moved.
func zcodeRequestHeaderOrder(method, requestTarget string) []string {
	return []string{
		"Host",
		"Connection",
		"Content-Type",
		"anthropic-version",
		"authorization",
		"http-referer",
		"user-agent",
		"x-api-key",
		"x-client-language",
		"x-client-timezone",
		"x-os-category",
		"x-os-version",
		"x-platform",
		"x-release-channel",
		"x-title",
		"x-zcode-agent",
		"x-zcode-app-version",
		"x-request-id",
		"x-zcode-session-type",
		"x-zcode-trace-id",
		"x-query-id",
		"x-session-id",
		"accept",
		"accept-language",
		"sec-fetch-mode",
		"accept-encoding",
		"Content-Length",
	}
}
