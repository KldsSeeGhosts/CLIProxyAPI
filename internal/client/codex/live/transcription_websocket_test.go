package live

import (
	"context"
	"encoding/json"
	"io"
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
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executionregistry"
)

func TestTranscriptionRealtimeQuery(t *testing.T) {
	for _, tc := range []struct {
		query                  string
		transcription, invalid bool
	}{
		{"", false, false},
		{"model=gpt-realtime", false, false},
		{"intent=transcription", true, false},
		{"intent=quicksilver", false, true},
		{"intent=", false, true},
		{"intent=transcription&intent=transcription", false, true},
		{"intent=transcription&model=gpt-realtime", false, true},
		{"intent=transcription&call_id=call-123", false, true},
		{"intent=transcription&url=https%3A%2F%2Fevil.invalid", false, true},
		{"intent=%ZZ", false, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got, err := transcriptionRealtimeQuery(tc.query)
			if got != tc.transcription || (err != nil) != tc.invalid {
				t.Fatalf("transcription=%v err=%v", got, err)
			}
		})
	}
}

func TestTranscriptionRejectsUnsupportedQueryAndConversationSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(auth.NewManager(nil, nil, nil), nil)
	for _, path := range []string{"?intent=unknown", "?intent=transcription&model=gpt-realtime", "?call_id=call-123&intent=quicksilver", "?intent=transcription"} {
		router := gin.New()
		router.GET("/v1/realtime", func(c *gin.Context) {
			c.Set(ClientSecretSessionContextKey, json.RawMessage(`{"type":"realtime","model":"gpt-realtime"}`))
		}, handler.HandleRealtimeWebsocket)
		request := httptest.NewRequest(http.MethodGet, "/v1/realtime"+path, nil)
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "websocket")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		expected := http.StatusBadRequest
		if path == "?intent=transcription" {
			expected = http.StatusForbidden
		}
		if recorder.Code != expected {
			t.Fatalf("%s: status=%d want=%d", path, recorder.Code, expected)
		}
	}
}

// No public model registration is performed: transcription requires a Codex
// OAuth credential, not an inference model in the catalog.
func TestTranscriptionWebsocketPreservesFramesAndOAuthHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const setup = `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"noise_reduction":{"type":"near_field"},"transcription":{"model":"gpt-transcribe","prompt":"Keep words","keywords":["Jot"]},"turn_detection":null}}}}`
	sent := []string{setup, `{"type":"input_audio_buffer.append","audio":"AAAA"}`, `{"type":"input_audio_buffer.commit"}`}
	received := []string{`{"type":"session.updated","session":{"type":"transcription"}}`, `{"type":"conversation.item.input_audio_transcription.delta","delta":"hello"}`, `{"type":"conversation.item.input_audio_transcription.completed","transcript":"hello"}`}
	requests := make(chan *http.Request, 1)
	frames := make(chan string, len(sent))
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		requests <- r.Clone(r.Context())
		for _, reply := range received {
			kind, frame, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			frames <- string(frame)
			if errWrite := conn.WriteMessage(kind, []byte(reply)); errWrite != nil {
				return
			}
		}
	}))
	defer upstreamServer.Close()
	manager := auth.NewManager(nil, &apiKeyFirstSelector{}, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerCredential(t, manager, &auth.Auth{ID: "api-key", Provider: "codex", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "not-oauth"}})
	registerCredential(t, manager, &auth.Auth{ID: "oauth", Provider: "codex", Status: auth.StatusActive, Metadata: map[string]any{"access_token": "oauth-token", "account_id": "account-123"}})
	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"
	router := gin.New()
	router.GET("/v1/realtime", handler.HandleRealtimeWebsocket)
	downstreamServer := httptest.NewServer(router)
	defer downstreamServer.Close()
	headers := http.Header{"Authorization": []string{"Bearer client-key"}, "Chatgpt-Account-Id": []string{"client-account"}, "Originator": []string{"spoofed-client"}, "X-Session-Id": []string{"session-123"}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstreamServer.URL, "http")+"/v1/realtime?intent=transcription", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i, frame := range sent {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(frame)); errWrite != nil {
			t.Fatal(errWrite)
		}
		_, reply, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatal(errRead)
		}
		if string(reply) != received[i] {
			t.Fatalf("reply changed: %s", reply)
		}
		if got := <-frames; got != frame {
			t.Fatalf("frame changed: %s", got)
		}
	}
	request := <-requests
	if request.URL.RawQuery != "intent=transcription" {
		t.Fatalf("query=%s", request.URL.RawQuery)
	}
	if request.Header.Get("Authorization") != "Bearer oauth-token" || request.Header.Get("Chatgpt-Account-Id") != "account-123" {
		t.Fatal("selected OAuth identity was not injected")
	}
	if request.Header.Get("Originator") != "jot-openai-transcribe-macos" || request.Header.Get("X-Session-Id") != "session-123" {
		t.Fatal("transcription protocol headers incorrect")
	}
}

func TestTranscriptionWebsocketRefreshesBeforeUpgradeOnlyOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, succeeds := range []bool{true, false} {
		t.Run(map[bool]string{true: "refresh succeeds", false: "refresh still unauthorized"}[succeeds], func(t *testing.T) {
			var attempts atomic.Int32
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if !succeeds || r.Header.Get("Authorization") != "Bearer refreshed-home-live-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
				_, _, _ = conn.ReadMessage()
			}))
			defer upstreamServer.Close()
			manager := auth.NewManager(nil, nil, nil)
			executor := &captureExecutor{}
			manager.RegisterExecutor(executor)
			registerCredential(t, manager, &auth.Auth{ID: "oauth", Provider: "codex", Status: auth.StatusActive, Metadata: map[string]any{"access_token": "expired", "refresh_token": "refresh"}})
			handler := NewHandler(manager, nil)
			handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"
			router := gin.New()
			router.GET("/v1/realtime", handler.HandleRealtimeWebsocket)
			downstreamServer := httptest.NewServer(router)
			defer downstreamServer.Close()
			conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstreamServer.URL, "http")+"/v1/realtime?intent=transcription", nil)
			if succeeds {
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, _, errRead := conn.ReadMessage()
				_ = conn.Close()
				if errRead != nil {
					t.Fatal(errRead)
				}
			} else {
				if conn != nil {
					_ = conn.Close()
				}
				if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
					t.Fatal("expected rejected handshake")
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			if executor.refreshCalls.Load() != 1 || attempts.Load() != 2 {
				t.Fatalf("refreshes=%d attempts=%d", executor.refreshCalls.Load(), attempts.Load())
			}
		})
	}
}

// The selector has no request deadline to inherit. It must see the acquisition
// deadline and unblock when it expires, before downstream is upgraded.
type stalledTranscriptionDispatcher struct {
	homeDispatcher
	deadline chan time.Time
}

func (d *stalledTranscriptionDispatcher) RPopAuth(ctx context.Context, _ string, _ string, _ http.Header, _ int) ([]byte, error) {
	deadline, _ := ctx.Deadline()
	d.deadline <- deadline
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestTranscriptionWebsocketBoundsStalledSelector(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runtimeConfig := &config.Config{Home: config.HomeConfig{Enabled: true}}
	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(runtimeConfig)
	dispatcher := &stalledTranscriptionDispatcher{deadline: make(chan time.Time, 1)}
	manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	manager.RegisterExecutor(&captureExecutor{})
	handler := NewHandler(manager, runtimeConfig)
	router := gin.New()
	router.GET("/v1/realtime", handler.HandleRealtimeWebsocket)
	request := httptest.NewRequest(http.MethodGet, "/v1/realtime?intent=transcription", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		router.ServeHTTP(recorder, request.WithContext(ctx))
		close(done)
	}()
	select {
	case deadline := <-dispatcher.deadline:
		if deadline.IsZero() || deadline.After(time.Now().Add(transcriptionSetupTimeout)) {
			cancel()
			<-done
			t.Fatal("selector did not receive the setup deadline")
		}
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("selector not called")
	}
	select {
	case <-done:
	case <-time.After(transcriptionSetupTimeout + 5*time.Second):
		cancel()
		<-done
		t.Fatal("stalled selector outlived setup budget")
	}
	if recorder.Code != http.StatusGatewayTimeout || !strings.Contains(recorder.Body.String(), "realtime_setup_timeout") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if time.Since(start) < transcriptionSetupTimeout {
		t.Fatal("selector returned before its deadline")
	}
}

type blockingTranscriptionPreparer struct {
	captureExecutor
	started chan context.Context
}

func (e *blockingTranscriptionPreparer) PrepareRequest(req *http.Request, _ *auth.Auth) error {
	e.started <- req.Context()
	<-req.Context().Done()
	return req.Context().Err()
}

func TestTranscriptionWebsocketHomeCancellationBoundsSetup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stage := range []string{"preparation", "dial"} {
		t.Run(stage, func(t *testing.T) {
			started := make(chan context.Context, 1)
			stopUpstream := make(chan struct{})
			upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				started <- r.Context()
				select {
				case <-r.Context().Done():
				case <-stopUpstream:
				}
			}))
			defer upstreamServer.Close()
			defer close(stopUpstream)
			runtimeConfig := &config.Config{Home: config.HomeConfig{Enabled: true}}
			manager := auth.NewManager(nil, nil, nil)
			manager.SetConfig(runtimeConfig)
			registry := executionregistry.New()
			manager.PublishHomeDispatch(&homeDispatcher{}, registry, 1)
			if stage == "preparation" {
				manager.RegisterExecutor(&blockingTranscriptionPreparer{started: started})
			} else {
				manager.RegisterExecutor(&captureExecutor{})
			}
			handler := NewHandler(manager, runtimeConfig)
			handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"
			router := gin.New()
			router.GET("/v1/realtime", handler.HandleRealtimeWebsocket)
			request := httptest.NewRequest(http.MethodGet, "/v1/realtime?intent=transcription", nil)
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "websocket")
			ctx, cancel := context.WithCancel(request.Context())
			defer cancel()
			recorder := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				router.ServeHTTP(recorder, request.WithContext(ctx))
				close(done)
			}()
			select {
			case acquisitionCtx := <-started:
				if stage == "preparation" {
					deadline, ok := acquisitionCtx.Deadline()
					if !ok || deadline.After(time.Now().Add(transcriptionSetupTimeout)) {
						cancel()
						<-done
						t.Fatal("preparation not bounded by setup deadline")
					}
				}
			case <-time.After(5 * time.Second):
				cancel()
				<-done
				t.Fatal("setup stage not reached")
			}
			// Closing the Home registry cancels selection-owned attempts, not the
			// HTTP request. Both preparation and dialing must honor that signal.
			if err := registry.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cancel()
				<-done
				t.Fatal("Home cancellation did not terminate acquisition")
			}
			if ctx.Err() != nil || recorder.Code == http.StatusSwitchingProtocols {
				t.Fatalf("request canceled or downstream upgraded: ctx=%v status=%d", ctx.Err(), recorder.Code)
			}
		})
	}
}
