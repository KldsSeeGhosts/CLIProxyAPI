package live

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// liveCredentialExhaustedCloseCode is a private-use WebSocket close code that
// tells the downstream client the pinned credential ran out of quota and a
// reconnect will select a different one.
const liveCredentialExhaustedCloseCode = 4429

// liveCredentialExhaustedCode is the machine-readable code forwarded in the
// rewritten terminal error frame.
const liveCredentialExhaustedCode = "cpa_credential_exhausted"

// errLiveCredentialExhausted terminates the guarded upstream copy once the
// rewritten error frame has been delivered.
var errLiveCredentialExhausted = &websocket.CloseError{Code: liveCredentialExhaustedCloseCode, Text: liveCredentialExhaustedCode}

// directWebsocketMaxCredentials returns the configured per-request credential
// cap. Zero disables the limit, matching the execution loop semantics.
func directWebsocketMaxCredentials(cfg *config.Config) int {
	if cfg == nil || cfg.MaxRetryCredentials < 0 {
		return 0
	}
	return cfg.MaxRetryCredentials
}

// isCredentialHandshakeFailure reports whether a rejected upstream WebSocket
// handshake is scoped to the credential rather than the request, and therefore
// eligible for failover to a different Codex OAuth credential.
func isCredentialHandshakeFailure(status int, body []byte) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	}
	return isLiveUsageLimitBody(body)
}

// isLiveUsageLimitBody reports whether a handshake rejection body carries a
// recognizable usage-limit signal even on a non-standard status.
func isLiveUsageLimitBody(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	for _, path := range []string{"error.type", "error.code", "type", "code", "body.error.type", "body.error.code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, path).String()), "usage_limit_reached") {
			return true
		}
	}
	return false
}

// liveCredentialScopedQuotaCodes are the in-band error identifiers whose
// exhaustion applies to the whole credential across model routes.
var liveCredentialScopedQuotaCodes = map[string]struct{}{
	"usage_limit_reached":                {},
	"websocket_connection_limit_reached": {},
}

// liveModelScopedQuotaCodes are in-band error identifiers that exhaust only the
// selected realtime model on this credential. A generic rate_limit_exceeded is
// RPM-scoped, so it must not cool sibling model routes.
var liveModelScopedQuotaCodes = map[string]struct{}{
	"rate_limit_exceeded": {},
}

// classifyLiveQuotaErrorFrame inspects an upstream text frame and reports
// whether it is a credential-exhaustion error. Both the Realtime error shape
// ({"type":"error","error":{...}}) and the Codex envelope shape
// ({"type":"error","status":429,"body":{"error":{...}}}) are recognized.
// Ordinary protocol/session errors and transient statuses are never matched.
func classifyLiveQuotaErrorFrame(payload []byte) (code string, retryAfter *time.Duration, credentialScoped bool, ok bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return "", nil, false, false
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "error" {
		return "", nil, false, false
	}
	if matched, matchedCode, scoped := liveQuotaErrorCodeAt(payload, "error"); matched {
		return matchedCode, liveFrameRetryAfter(payload, "error"), scoped, true
	}
	// Codex envelope: quota failures arrive with an explicit 4xx status and a
	// nested body.error object.
	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
	default:
		return "", nil, false, false
	}
	if matched, matchedCode, scoped := liveQuotaErrorCodeAt(payload, "body.error"); matched {
		return matchedCode, liveFrameRetryAfter(payload, "body.error"), scoped, true
	}
	return "", nil, false, false
}

func liveQuotaErrorCodeAt(payload []byte, prefix string) (bool, string, bool) {
	for _, field := range []string{prefix + ".type", prefix + ".code"} {
		candidate := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, field).String()))
		if _, matched := liveCredentialScopedQuotaCodes[candidate]; matched {
			return true, candidate, true
		}
		if _, matched := liveModelScopedQuotaCodes[candidate]; matched {
			return true, candidate, false
		}
	}
	return false, "", false
}

// liveFrameRetryAfter extracts reset metadata from a matched quota frame. It
// understands resets_at (unix seconds), resets_in_seconds and retry_after
// (seconds) below the given error object.
func liveFrameRetryAfter(payload []byte, prefix string) *time.Duration {
	now := time.Now()
	if resetsAt := gjson.GetBytes(payload, prefix+".resets_at").Int(); resetsAt > 0 {
		resetAtTime := time.Unix(resetsAt, 0)
		if resetAtTime.After(now) {
			retryAfter := resetAtTime.Sub(now)
			return &retryAfter
		}
	}
	for _, field := range []string{prefix + ".resets_in_seconds", prefix + ".retry_after", prefix + ".reset_after_seconds"} {
		if seconds := gjson.GetBytes(payload, field).Int(); seconds > 0 {
			retryAfter := time.Duration(seconds) * time.Second
			return &retryAfter
		}
	}
	return nil
}

// handshakeRetryAfter reads the Retry-After header (seconds or HTTP date) from
// a rejected handshake response, falling back to usage-limit reset fields in
// the body.
func handshakeRetryAfter(header http.Header, body []byte) *time.Duration {
	if header != nil {
		if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
			if seconds, errParse := strconv.ParseInt(raw, 10, 64); errParse == nil && seconds > 0 {
				retryAfter := time.Duration(seconds) * time.Second
				return &retryAfter
			}
			if at, errParse := http.ParseTime(raw); errParse == nil {
				if delay := time.Until(at); delay > 0 {
					return &delay
				}
			}
		}
	}
	if isLiveUsageLimitBody(body) {
		for _, prefix := range []string{"error", "body.error"} {
			if retryAfter := liveFrameRetryAfter(body, prefix); retryAfter != nil {
				return retryAfter
			}
		}
	}
	return nil
}

// credentialExhaustedFrame builds the single machine-readable error frame
// forwarded downstream before the socket is closed with the private code.
func credentialExhaustedFrame(code string) []byte {
	frame := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":      "server_error",
			"code":      liveCredentialExhaustedCode,
			"message":   "Codex credential quota exhausted; reconnect to retry on a different credential",
			"retryable": true,
		},
	}
	errorFields := frame["error"].(map[string]any)
	if code != "" {
		errorFields["upstream_code"] = code
	}
	encoded, errMarshal := json.Marshal(frame)
	if errMarshal != nil {
		return []byte(`{"type":"error","error":{"type":"server_error","code":"cpa_credential_exhausted","message":"Codex credential quota exhausted","retryable":true}}`)
	}
	return encoded
}

// liveResultOptions returns result options carrying the session affinity
// provider/model metadata the normal execution contract attaches before
// MarkResult, so affinity bindings resolve under the same namespace.
func liveResultOptions(opts coreexecutor.Options, model string) coreexecutor.Options {
	meta := make(map[string]any, len(opts.Metadata)+2)
	for key, value := range opts.Metadata {
		meta[key] = value
	}
	meta[coreexecutor.SessionAffinityProviderMetadataKey] = "codex"
	meta[coreexecutor.SessionAffinityModelMetadataKey] = model
	opts.Metadata = meta
	return opts
}

// excludedAuthIDsOption returns options carrying the excluded_auth_ids metadata
// consumed by both Home dispatch and legacy selection.
func excludedAuthIDsOption(opts coreexecutor.Options, tried map[string]struct{}) coreexecutor.Options {
	if len(tried) == 0 {
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		meta[key] = value
	}
	ids := make([]string, 0, len(tried))
	for authID := range tried {
		if authID = strings.TrimSpace(authID); authID != "" {
			ids = append(ids, authID)
		}
	}
	sort.Strings(ids)
	meta[auth.ExcludedAuthIDsMetadataKey] = ids
	opts.Metadata = meta
	return opts
}

// copyWebsocketGuarded streams source to destination like copyWebsocket, but
// inspects every text frame. When inspect returns a non-nil replacement the
// rewritten frame is forwarded instead; when it also reports exhaustion the
// connection pair is closed with the private credential-exhausted code.
// Binary frames keep the original streaming path so audio payloads are not
// buffered.
func copyWebsocketGuarded(destination, source *websocket.Conn, inspect func(payload []byte) (replacement []byte, exhausted bool)) error {
	for {
		messageType, reader, errReader := source.NextReader()
		if errReader != nil {
			return errReader
		}
		if messageType != websocket.TextMessage || inspect == nil {
			writer, errWriter := destination.NextWriter(messageType)
			if errWriter != nil {
				return errWriter
			}
			_, errCopy := io.Copy(writer, reader)
			errClose := writer.Close()
			if errCopy != nil {
				return errCopy
			}
			if errClose != nil {
				return errClose
			}
			continue
		}
		payload, errRead := readLimitedBody(reader)
		if errRead != nil {
			return errRead
		}
		replacement, exhausted := inspect(payload)
		if replacement == nil {
			replacement = payload
		}
		if errWrite := destination.WriteMessage(websocket.TextMessage, replacement); errWrite != nil {
			return errWrite
		}
		if exhausted {
			closePayload := websocket.FormatCloseMessage(liveCredentialExhaustedCloseCode, liveCredentialExhaustedCode)
			_ = destination.WriteControl(websocket.CloseMessage, closePayload, time.Time{})
			_ = source.WriteControl(websocket.CloseMessage, closePayload, time.Time{})
			return errLiveCredentialExhausted
		}
	}
}

// relayWebsocketsQuotaGuarded relays downstream->upstream unchanged and applies
// the quota guard only to the upstream->downstream direction.
func relayWebsocketsQuotaGuarded(downstream, upstream *websocket.Conn, inspect func(payload []byte) ([]byte, bool)) error {
	results := make(chan error, 2)
	go func() { results <- copyWebsocket(upstream, downstream) }()
	go func() { results <- copyWebsocketGuarded(downstream, upstream, inspect) }()

	firstErr := <-results
	closeCode, closeReason := websocketCloseDetails(firstErr)
	payload := websocket.FormatCloseMessage(closeCode, closeReason)
	_ = downstream.WriteControl(websocket.CloseMessage, payload, time.Time{})
	_ = upstream.WriteControl(websocket.CloseMessage, payload, time.Time{})
	_ = downstream.Close()
	_ = upstream.Close()
	<-results
	return firstErr
}
