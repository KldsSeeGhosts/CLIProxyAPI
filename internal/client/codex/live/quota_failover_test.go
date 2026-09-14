package live

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// newQuotaTestUpstream serves a WebSocket upstream that rejects rejectTokens during
// the handshake and echoes text frames otherwise.
func newQuotaTestUpstream(t *testing.T, rejectStatus int, rejectTokens map[string]string, onFrame func(conn *websocket.Conn)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if body, rejected := rejectTokens[token]; rejected {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Retry-After", "120")
			writer.WriteHeader(rejectStatus)
			_, _ = writer.Write([]byte(body))
			return
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, errUpgrade := upgrader.Upgrade(writer, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		onFrame(conn)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func newDirectWebsocketHandler(manager *auth.Manager, cfg *config.Config, upstream *httptest.Server) *Handler {
	handler := NewHandler(manager, cfg)
	handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstream.URL, "http") + "/v1"
	return handler
}

func dialDirectRealtime(t *testing.T, handler *Handler) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	router := gin.New()
	router.GET("/v1/realtime", handler.HandleRealtimeWebsocket)
	downstream := httptest.NewServer(router)
	t.Cleanup(downstream.Close)
	wsURL := "ws" + strings.TrimPrefix(downstream.URL, "http") + "/v1/realtime?model=gpt-realtime"
	return websocket.DefaultDialer.Dial(wsURL, nil)
}

func registerOAuthCredential(t *testing.T, manager *auth.Manager, id, token string) {
	t.Helper()
	registerCredential(t, manager, &auth.Auth{
		ID:       id,
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{"access_token": token},
	})
}

func TestHandleDirectWebsocketRetriesCredentialOnQuotaHandshake(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rejectTokens := map[string]string{
		"token-limited": `{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`,
	}
	upstream, calls := newQuotaTestUpstream(t, http.StatusTooManyRequests, rejectTokens, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
		_, _, _ = conn.ReadMessage()
	})

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerOAuthCredential(t, manager, "codex-aaa-limited", "token-limited")
	registerOAuthCredential(t, manager, "codex-zzz-healthy", "token-healthy")

	handler := newDirectWebsocketHandler(manager, &config.Config{MaxRetryCredentials: 3}, upstream)
	connection, response, errDial := dialDirectRealtime(t, handler)
	if errDial != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	defer func() { _ = connection.Close() }()

	_, created, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read session.created: %v", errRead)
	}
	if string(created) != `{"type":"session.created"}` {
		t.Fatalf("created event = %s", created)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream handshake calls = %d, want 2", calls.Load())
	}
	failed, ok := manager.GetByID("codex-aaa-limited")
	if !ok {
		t.Fatal("limited credential missing from manager")
	}
	if !failed.Quota.Exceeded || failed.Quota.Reason != "credential_quota" {
		t.Fatalf("limited credential quota = %#v, want credential_quota cooldown", failed.Quota)
	}
	if failed.Quota.NextRecoverAt.IsZero() || time.Until(failed.Quota.NextRecoverAt) > 2*time.Minute {
		t.Fatalf("limited credential NextRecoverAt = %v, want ~120s", failed.Quota.NextRecoverAt)
	}
	healthy, ok := manager.GetByID("codex-zzz-healthy")
	if !ok || healthy.Quota.Exceeded {
		t.Fatalf("healthy credential quota = %#v, want untouched", healthy.Quota)
	}
}

func TestHandleDirectWebsocketHonorsCredentialRetryCap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rejectTokens := map[string]string{
		"token-limited-a": `{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`,
		"token-limited-b": `{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`,
	}
	upstream, calls := newQuotaTestUpstream(t, http.StatusTooManyRequests, rejectTokens, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
	})

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerOAuthCredential(t, manager, "codex-aaa-limited", "token-limited-a")
	registerOAuthCredential(t, manager, "codex-bbb-limited", "token-limited-b")
	registerOAuthCredential(t, manager, "codex-zzz-healthy", "token-healthy")

	handler := newDirectWebsocketHandler(manager, &config.Config{MaxRetryCredentials: 2}, upstream)
	connection, response, errDial := dialDirectRealtime(t, handler)
	if connection != nil {
		_ = connection.Close()
	}
	if errDial == nil || response == nil {
		t.Fatalf("dial downstream websocket = response %#v err %v, want rejected handshake", response, errDial)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("downstream rejection = status %d, want 429", response.StatusCode)
	}
	if got := response.Header.Get("Retry-After"); got != "120" {
		t.Fatalf("Retry-After = %q, want upstream value", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream handshake calls = %d, want 2 (retry cap)", calls.Load())
	}
	healthy, ok := manager.GetByID("codex-zzz-healthy")
	if !ok || healthy.Quota.Exceeded {
		t.Fatalf("healthy credential quota = %#v, want untouched", healthy.Quota)
	}
}

func TestHandleDirectWebsocketRewritesInBandQuotaError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream, _ := newQuotaTestUpstream(t, http.StatusTooManyRequests, nil, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":429,"body":{"error":{"type":"usage_limit_reached","resets_in_seconds":90}}}`))
		_, _, _ = conn.ReadMessage()
	})

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerOAuthCredential(t, manager, "codex-oauth", "quota-token")

	handler := newDirectWebsocketHandler(manager, nil, upstream)
	connection, _, errDial := dialDirectRealtime(t, handler)
	if errDial != nil {
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	defer func() { _ = connection.Close() }()

	_, created, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read session.created: %v", errRead)
	}
	if string(created) != `{"type":"session.created"}` {
		t.Fatalf("created event = %s", created)
	}
	_, errorFrame, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read rewritten error frame: %v", errRead)
	}
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(errorFrame, &event); errUnmarshal != nil {
		t.Fatalf("unmarshal rewritten error frame: %v; frame=%s", errUnmarshal, errorFrame)
	}
	if event.Type != "error" || event.Error.Code != "cpa_credential_exhausted" || !event.Error.Retryable {
		t.Fatalf("rewritten frame = %+v, want cpa_credential_exhausted retryable error", event)
	}
	_, _, errRead = connection.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(errRead, &closeErr) || closeErr.Code != liveCredentialExhaustedCloseCode {
		t.Fatalf("close error = %v, want websocket close %d", errRead, liveCredentialExhaustedCloseCode)
	}
	failed, ok := manager.GetByID("codex-oauth")
	if !ok || !failed.Quota.Exceeded || failed.Quota.Reason != "credential_quota" {
		t.Fatalf("credential quota = %#v, want credential_quota cooldown", failed.Quota)
	}
	if failed.Quota.NextRecoverAt.IsZero() || time.Until(failed.Quota.NextRecoverAt) > 2*time.Minute {
		t.Fatalf("credential NextRecoverAt = %v, want ~90s", failed.Quota.NextRecoverAt)
	}
}

func TestHandleDirectWebsocketRateLimitErrorIsModelScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream, _ := newQuotaTestUpstream(t, http.StatusTooManyRequests, nil, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":429,"body":{"error":{"type":"rate_limit_exceeded","resets_in_seconds":30}}}`))
		_, _, _ = conn.ReadMessage()
	})

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerOAuthCredential(t, manager, "codex-oauth", "quota-token")

	handler := newDirectWebsocketHandler(manager, nil, upstream)
	connection, _, errDial := dialDirectRealtime(t, handler)
	if errDial != nil {
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	defer func() { _ = connection.Close() }()

	_, errorFrame, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read rewritten error frame: %v", errRead)
	}
	var event struct {
		Type  string `json:"type"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(errorFrame, &event); errUnmarshal != nil {
		t.Fatalf("unmarshal rewritten error frame: %v; frame=%s", errUnmarshal, errorFrame)
	}
	if event.Type != "error" || event.Error.Code != "cpa_credential_exhausted" {
		t.Fatalf("rewritten frame = %+v, want cpa_credential_exhausted error", event)
	}
	_, _, errRead = connection.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(errRead, &closeErr) || closeErr.Code != liveCredentialExhaustedCloseCode {
		t.Fatalf("close error = %v, want websocket close %d", errRead, liveCredentialExhaustedCloseCode)
	}
	failed, ok := manager.GetByID("codex-oauth")
	if !ok || !failed.Quota.Exceeded {
		t.Fatalf("credential quota = %#v, want model-scoped quota cooldown", failed.Quota)
	}
	if failed.Quota.Reason == "credential_quota" {
		t.Fatalf("credential quota = %#v, want model-scoped cooldown not credential-wide", failed.Quota)
	}
	state := failed.ModelStates[defaultLiveModel]
	if state == nil || !state.Quota.Exceeded {
		t.Fatalf("model state = %#v, want %s cooled", state, defaultLiveModel)
	}
}

func TestHandleDirectWebsocketPassesThroughNonQuotaError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const protocolError = `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_session","message":"bad session"}}`
	upstream, _ := newQuotaTestUpstream(t, http.StatusTooManyRequests, nil, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(protocolError))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
		_, _, _ = conn.ReadMessage()
	})

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerOAuthCredential(t, manager, "codex-oauth", "fine-token")

	handler := newDirectWebsocketHandler(manager, nil, upstream)
	connection, _, errDial := dialDirectRealtime(t, handler)
	if errDial != nil {
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	defer func() { _ = connection.Close() }()

	_, errorFrame, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read protocol error frame: %v", errRead)
	}
	if string(errorFrame) != protocolError {
		t.Fatalf("forwarded frame = %s, want original %s", errorFrame, protocolError)
	}
	_, created, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("socket closed after non-quota error: %v", errRead)
	}
	if string(created) != `{"type":"session.created"}` {
		t.Fatalf("follow-up event = %s", created)
	}
	credential, ok := manager.GetByID("codex-oauth")
	if !ok || credential.Quota.Exceeded {
		t.Fatalf("credential quota = %#v, want untouched", credential.Quota)
	}
}
