package live

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const defaultStandardRealtimeModel = "gpt-realtime"

// HandleRealtimeWebsocket dispatches a standard Realtime WebSocket or an existing call sideband.
func (h *Handler) HandleRealtimeWebsocket(c *gin.Context) {
	if strings.TrimSpace(c.Query("call_id")) != "" {
		h.HandleSideband(c)
		return
	}
	h.HandleDirectWebsocket(c)
}

// HandleDirectWebsocket relays a standard Realtime WebSocket through Codex OAuth.
func (h *Handler) HandleDirectWebsocket(c *gin.Context) {
	if h == nil || h.authManager == nil {
		writeRealtimeError(c, http.StatusServiceUnavailable, "Codex auth manager unavailable", "server_error", "codex_auth_unavailable")
		return
	}
	if !websocket.IsWebSocketUpgrade(c.Request) {
		c.Header("Upgrade", "websocket")
		writeRealtimeError(c, http.StatusUpgradeRequired, "WebSocket upgrade required", "invalid_request_error", "websocket_upgrade_required")
		return
	}

	requestedModel := strings.TrimSpace(c.Query("model"))
	if requestedModel == "" {
		requestedModel = defaultStandardRealtimeModel
	}
	selectionModel := codexRealtimeModel(requestedModel)
	tokenSession := clientSecretSession(c)
	if len(tokenSession) > 0 {
		tokenModel := codexRealtimeModel(modelFromJSON(tokenSession))
		if selectionModel != tokenModel {
			writeRealtimeError(c, http.StatusForbidden, "Realtime client secret is not valid for the requested model", "invalid_request_error", "realtime_client_secret_scope_mismatch")
			return
		}
	}
	ctx := context.WithValue(c.Request.Context(), "gin", c)
	ctx = coreexecutor.WithDownstreamWebsocket(ctx)
	selectionOpts := coreexecutor.Options{Headers: liveSelectionHeaders(c)}
	ctx = handlers.EnrichContextWithSessionHierarchy(ctx, selectionOpts.Headers, nil, nil)
	upstreamURL := h.directRealtimeURL(requestedModel)
	helpConfig := h.currentConfig()
	maxCredentials := directWebsocketMaxCredentials(helpConfig)
	tried := make(map[string]struct{})

	dialUpstream := func(attemptCtx context.Context, current *auth.Auth) (*websocket.Conn, *http.Response, error) {
		request, errRequest := http.NewRequestWithContext(attemptCtx, http.MethodGet, websocketHTTPURL(upstreamURL), nil)
		if errRequest != nil {
			return nil, nil, errRequest
		}
		request.Header = directRealtimeHeaders(c.Request.Header)
		setAccountHeader(request.Header, current)
		if errPrepare := h.authManager.PrepareHttpRequest(attemptCtx, current, request); errPrepare != nil {
			return nil, nil, errPrepare
		}
		authType, authValue := current.AccountInfo()
		helps.RecordAPIWebsocketRequest(attemptCtx, helpConfig, helps.UpstreamRequestLog{
			URL:       upstreamURL,
			Method:    "WEBSOCKET",
			Headers:   headersForLogging(request.Header),
			Provider:  "codex",
			AuthID:    current.ID,
			AuthLabel: current.Label,
			AuthType:  authType,
			AuthValue: authValue,
		})
		dialer := newProxyAwareSidebandDialer(helpConfig, current)
		dialer.Subprotocols = websocket.Subprotocols(c.Request)
		return dialer.DialContext(attemptCtx, upstreamURL, request.Header)
	}

	var selection *auth.HomeDispatchSelection
	var selected *auth.Auth
	var upstream *websocket.Conn
	var releaseAttempt func()
	var lastFailure *liveHandshakeFailure
	for {
		if maxCredentials > 0 && len(tried) >= maxCredentials {
			break
		}
		attemptOpts := excludedAuthIDsOption(selectionOpts, tried)
		var errSelect error
		selection, selected, errSelect = h.selectOAuth(ctx, selectionModel, attemptOpts)
		if errSelect != nil {
			if lastFailure != nil {
				lastFailure.write(c)
				return
			}
			writeSelectionError(c, errSelect)
			return
		}
		if selected == nil {
			if selection != nil {
				selection.End("missing_auth")
			}
			if lastFailure != nil {
				lastFailure.write(c)
				return
			}
			writeRealtimeError(c, http.StatusServiceUnavailable, "Codex auth unavailable", "server_error", "codex_auth_unavailable")
			return
		}
		if _, alreadyTried := tried[selected.ID]; alreadyTried {
			// A selector that ignores excluded_auth_ids must not loop forever on
			// the same credential.
			if selection != nil {
				selection.End("repeated_excluded_auth")
			}
			break
		}
		// Upstream v7.3.14: canonicalize the selected session hierarchy into
		// request metadata so downstream records carry parent linkage.
		if selection != nil && selection.CanonicalSessionID != "" {
			meta := logging.GetClientRequestMetadata(ctx)
			meta.SessionID = selection.CanonicalSessionID
			if selection.ParentSessionID != "" {
				meta.ParentSessionID = selection.ParentSessionID
			} else {
				meta.ParentSessionID = ""
			}
			if meta.SessionID == meta.ParentSessionID {
				meta.ParentSessionID = ""
			}
			ctx = logging.WithClientRequestMetadata(ctx, meta)
		}

		attemptCtx := ctx
		releaseAttempt = func() {}
		if selection != nil {
			boundCtx, release, errAttempt := selection.AttemptContext(ctx)
			if errAttempt != nil {
				selection.End("attempt_bind_failed")
				if lastFailure != nil {
					lastFailure.write(c)
					return
				}
				writeRealtimeError(c, http.StatusServiceUnavailable, errAttempt.Error(), "server_error", "realtime_upstream_unavailable")
				return
			}
			attemptCtx = boundCtx
			releaseAttempt = release
		}
		logging.SetGinCPATraceID(c, selected.EnsureIndex())

		conn, handshakeResponse, errDial := dialUpstream(attemptCtx, selected)
		if errDial == nil {
			closeHandshakeBody(handshakeResponse, "direct websocket handshake")
			upstream = conn
			break
		}
		failure := newLiveHandshakeFailure(attemptCtx, helpConfig, handshakeResponse, errDial)
		if selection != nil && failure.status == http.StatusUnauthorized {
			diagnosticBody := failure.body
			if len(diagnosticBody) == 0 {
				diagnosticBody = []byte(errDial.Error())
			}
			h.authManager.ReportHomeUnauthorized(attemptCtx, selected, "codex", selectionModel, diagnosticBody)
		}
		log.WithField("status", failure.status).Warnf("codex realtime websocket upstream handshake failed: %s", logging.SafeDiagnosticForLog(string(failure.body)))
		helps.RecordAPIWebsocketError(attemptCtx, helpConfig, "dial", errDial)

		tried[selected.ID] = struct{}{}
		lastFailure = failure
		if failure.credentialScoped && (maxCredentials == 0 || len(tried) < maxCredentials) {
			h.markHandshakeCredentialFailure(attemptCtx, selected, selectionModel, attemptOpts, failure)
			releaseAttempt()
			if selection != nil {
				selection.End("handshake_credential_failed")
			}
			continue
		}
		releaseAttempt()
		if selection != nil {
			selection.End("handshake_failed")
		}
		failure.write(c)
		return
	}
	if upstream == nil {
		if lastFailure != nil {
			lastFailure.write(c)
			return
		}
		writeRealtimeError(c, http.StatusServiceUnavailable, "Codex auth unavailable", "server_error", "codex_auth_unavailable")
		return
	}
	if selection != nil {
		selection.Retain()
		defer releaseAttempt()
		defer selection.End("session_closed")
	}
	closeUpstream := websocketCloseFunc("upstream", upstream)
	defer func() { _ = closeUpstream() }()
	if len(tokenSession) > 0 {
		updateSession, errSession := realtimeSessionUpdate(tokenSession)
		if errSession != nil {
			_ = closeUpstream()
			writeRealtimeError(c, http.StatusInternalServerError, "Failed to apply Realtime client secret session", "server_error", "realtime_session_failed")
			return
		}
		update, errMarshal := json.Marshal(struct {
			Type    string          `json:"type"`
			Session json.RawMessage `json:"session"`
		}{Type: "session.update", Session: updateSession})
		if errMarshal != nil {
			_ = closeUpstream()
			writeRealtimeError(c, http.StatusInternalServerError, "Failed to apply Realtime client secret session", "server_error", "realtime_session_failed")
			return
		}
		if errWrite := upstream.WriteMessage(websocket.TextMessage, update); errWrite != nil {
			_ = closeUpstream()
			writeRealtimeError(c, http.StatusBadGateway, "Failed to apply Realtime client secret session", "api_error", "realtime_upstream_unavailable")
			return
		}
	}

	if selection != nil {
		if errBind := selection.Bind(closeUpstream); errBind != nil {
			writeRealtimeError(c, http.StatusServiceUnavailable, errBind.Error(), "server_error", "realtime_upstream_unavailable")
			return
		}
	}

	upgradeHeaders := make(http.Header)
	if subprotocol := upstream.Subprotocol(); subprotocol != "" {
		upgradeHeaders.Set("Sec-WebSocket-Protocol", subprotocol)
	}
	downstream, errUpgrade := sidebandUpgrader.Upgrade(c.Writer, c.Request, upgradeHeaders)
	if errUpgrade != nil {
		_ = closeUpstream()
		return
	}
	closeDownstream := websocketCloseFunc("downstream", downstream)
	defer func() { _ = closeDownstream() }()
	if selection != nil {
		if errBind := selection.Bind(closeDownstream); errBind != nil {
			return
		}
	}

	inspect := h.liveQuotaFrameInspector(ctx, selected, selectionModel, selectionOpts)
	if errRelay := relayWebsocketsQuotaGuarded(downstream, upstream, inspect); errRelay != nil && !isNormalWebsocketClose(errRelay) && !errors.Is(errRelay, errLiveCredentialExhausted) {
		helps.RecordAPIWebsocketError(ctx, h.currentConfig(), "relay", errRelay)
		log.WithError(errRelay).Debug("codex realtime direct websocket relay closed")
	}
}

// liveHandshakeFailure captures a rejected upstream handshake so the terminal
// response can be replayed after credential failover is exhausted.
type liveHandshakeFailure struct {
	status           int
	credentialScoped bool
	headers          http.Header
	body             []byte
	contentType      string
	retryAfter       *time.Duration
}

func newLiveHandshakeFailure(ctx context.Context, cfg *config.Config, response *http.Response, errDial error) *liveHandshakeFailure {
	failure := &liveHandshakeFailure{
		status: clienterror.HTTPStatusFromErrorOr(errDial, http.StatusBadGateway),
	}
	if response != nil {
		if response.StatusCode > 0 {
			failure.status = response.StatusCode
		}
		failure.headers = response.Header.Clone()
		failure.contentType = response.Header.Get("Content-Type")
		helps.RecordAPIWebsocketHandshake(ctx, cfg, response.StatusCode, callResponseHeaders(response.Header))
		if response.Body != nil {
			var errRead error
			failure.body, errRead = readLimitedBody(response.Body)
			if errRead != nil {
				log.Errorf("codex realtime: read rejected handshake body error: %v", errRead)
			}
			helps.AppendAPIWebsocketResponse(ctx, cfg, failure.body)
		}
	}
	closeHandshakeBody(response, "direct websocket rejected")
	failure.credentialScoped = isCredentialHandshakeFailure(failure.status, failure.body)
	failure.retryAfter = handshakeRetryAfter(failure.headers, failure.body)
	return failure
}

// resultCredentialScope reports whether a handshake rejection marks the whole
// credential unusable. 401 and 403 mean revoked/unauthorized credentials, so
// every model route must stop selecting them; 402, 429 and usage-limit bodies
// keep their existing model-scoped or credential-scoped treatment.
func (f *liveHandshakeFailure) resultCredentialScope() bool {
	if f == nil {
		return false
	}
	switch f.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return isLiveUsageLimitBody(f.body) || f.status == http.StatusTooManyRequests || f.status == http.StatusPaymentRequired
}

// write reproduces the terminal response the upstream rejection produced,
// preserving Retry-After and request ID headers from the failed handshake.
func (f *liveHandshakeFailure) write(c *gin.Context) {
	copyRealtimeHandshakeHeaders(c.Writer.Header(), f.headers)
	if f.status == http.StatusUnauthorized && len(f.headers) > 0 {
		if f.contentType != "" {
			c.Header("Content-Type", f.contentType)
		}
		c.Status(f.status)
		if len(f.body) > 0 {
			if _, errWrite := c.Writer.Write(f.body); errWrite != nil {
				log.WithError(errWrite).Warn("codex realtime: write rejected handshake body failed")
			}
		}
		return
	}
	status := f.status
	helpDetails := "Codex Realtime WebSocket upstream unavailable"
	helpType := "api_error"
	if status == http.StatusNotFound || status == http.StatusNotImplemented {
		helpDetails = "Direct Realtime WebSocket is not supported by the Codex OAuth upstream"
		helpType = "not_supported_error"
		status = http.StatusNotImplemented
	}
	helpCode := "realtime_websocket_upstream_unavailable"
	if helpType == "not_supported_error" {
		helpCode = "realtime_capability_not_supported"
	} else if status == http.StatusUnauthorized {
		helpType = "authentication_error"
		helpCode = "realtime_upstream_unauthorized"
	} else if status == http.StatusTooManyRequests {
		helpType = "rate_limit_error"
		helpCode = "realtime_upstream_rate_limited"
	}
	writeRealtimeError(c, status, helpDetails, helpType, helpCode)
}

// markHandshakeCredentialFailure feeds a credential-scoped handshake rejection
// into the shared result/cooldown path so the failed credential cools for other
// requests too. Home-dispatched credentials receive the same bookkeeping; the
// manager observes the result even when the credential is not locally stored.
func (h *Handler) markHandshakeCredentialFailure(ctx context.Context, selected *auth.Auth, model string, opts coreexecutor.Options, failure *liveHandshakeFailure) {
	if h == nil || h.authManager == nil || selected == nil || failure == nil {
		return
	}
	message := strings.TrimSpace(string(failure.body))
	if len(message) > 512 {
		message = message[:512]
	}
	if message == "" {
		message = http.StatusText(failure.status)
	}
	result := auth.Result{
		AuthID:   selected.ID,
		Provider: "codex",
		Model:    model,
		Success:  false,
		Options:  liveResultOptions(opts, model),
		Error: &auth.Error{
			Code:       "upstream_handshake_failed",
			Message:    message,
			HTTPStatus: failure.status,
			Retryable:  true,
		},
		RetryAfter:      failure.retryAfter,
		CredentialScope: failure.resultCredentialScope(),
	}
	// MarkResult invokes the same OnResult hook as reportHomeResult, so Home
	// dispatch results are already observed; no separate Home report is needed.
	h.authManager.MarkResult(ctx, result)
}

// liveQuotaFrameInspector returns the guard applied to upstream->downstream
// frames. On a credential-exhaustion event it records the cooldown, rewrites
// the frame to a machine-readable error, and asks the relay to close both
// sides with the private close code.
func (h *Handler) liveQuotaFrameInspector(ctx context.Context, selected *auth.Auth, model string, opts coreexecutor.Options) func(payload []byte) ([]byte, bool) {
	var once sync.Once
	return func(payload []byte) ([]byte, bool) {
		code, retryAfter, credentialScoped, matched := classifyLiveQuotaErrorFrame(payload)
		if !matched {
			return nil, false
		}
		once.Do(func() {
			if h != nil && h.authManager != nil && selected != nil {
				// MarkResult invokes the same OnResult hook as reportHomeResult, so
				// Home dispatch results are already observed.
				h.authManager.MarkResult(ctx, auth.Result{
					AuthID:          selected.ID,
					Provider:        "codex",
					Model:           model,
					Success:         false,
					Options:         liveResultOptions(opts, model),
					RetryAfter:      retryAfter,
					CredentialScope: credentialScoped,
					Error: &auth.Error{
						Code:       liveCredentialExhaustedCode,
						Message:    "codex realtime credential exhausted: " + code,
						HTTPStatus: http.StatusTooManyRequests,
						Retryable:  true,
					},
				})
			}
			log.WithField("upstream_code", code).Warn("codex realtime upstream reported credential quota exhaustion")
		})
		return credentialExhaustedFrame(code), true
	}
}

func realtimeSessionUpdate(session json.RawMessage) (json.RawMessage, error) {
	var update map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(session, &update); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	for _, field := range []string{"model", "id", "object", "expires_at", "client_secret"} {
		delete(update, field)
	}
	return json.Marshal(update)
}

func (h *Handler) directRealtimeURL(model string) string {
	values := make(url.Values)
	values.Set("model", strings.TrimSpace(model))
	return strings.TrimRight(h.sidebandAPIBaseURL, "/") + "/realtime?" + values.Encode()
}

func directRealtimeHeaders(source http.Header) http.Header {
	headers := protocolHeaders(source)
	headers.Del("OpenAI-Alpha")
	if headers.Get("Originator") == "" {
		headers.Set("Originator", "Codex Desktop")
	}
	return headers
}

func copyRealtimeHandshakeHeaders(destination, source http.Header) {
	for _, name := range []string{"Retry-After", "X-Request-Id", "OpenAI-Request-Id"} {
		for _, value := range source.Values(name) {
			destination.Add(name, value)
		}
	}
}

func closeHandshakeBody(response *http.Response, label string) {
	if response == nil || response.Body == nil {
		return
	}
	if errClose := response.Body.Close(); errClose != nil {
		log.Errorf("codex realtime: close %s response body error: %v", label, errClose)
	}
}
