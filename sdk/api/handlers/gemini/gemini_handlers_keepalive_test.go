package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestGeminiAntigravityKeepAliveSuppressionCoversBothStreamPhases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name    string
		ua      string
		query   string
		tickers int
	}{
		{"antigravity", "google-antigravity-sdk/0.0.0-dev python/3.14.7 antigravity/acp/1.4.1", "?alt=sse", 0},
		{"antigravity-default-alt", "google-antigravity-sdk/1.4.4", "", 0},
		{"genai", "google-genai-sdk/1.75.0", "?alt=sse", 2},
		{"pi", "pi/1.4.0", "?alt=sse", 2},
		{"unspecified", "", "", 2},
		{"unrelated-prefix", "other-google-antigravity-sdk/1.4.4", "", 2},
		{"raw-json", "google-genai-sdk/1.75.0", "?alt=json", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tickers := 0
			restore := handlers.SetStreamingKeepAliveTickerFactoryForTest(func(interval time.Duration) (<-chan time.Time, func()) {
				tickers++
				if interval != 5*time.Second {
					t.Errorf("interval = %v, want 5s", interval)
				}
				return make(chan time.Time), func() {}
			})
			defer restore()

			chunks := make(chan coreexecutor.StreamChunk, 2)
			chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`)}
			chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"candidates":[{"finishReason":"STOP"}]}`)}
			close(chunks)
			executor := &preFTKeepAliveGeminiExecutor{chunks: chunks}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			auth := &coreauth.Auth{ID: "keepalive-auth-" + tt.name, Provider: executor.Identifier(), Status: coreauth.StatusActive}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			model := "keepalive-model-" + tt.name
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

			cfg := &sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{
				KeepAliveSeconds: 5, SendKeepAliveBeforeFT: true,
			}}
			h := NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
			router := gin.New()
			router.POST("/v1beta/models/*action", h.GeminiHandler)
			request := httptest.NewRequest(http.MethodPost, "/v1beta/models/"+model+":streamGenerateContent"+tt.query,
				strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
			request.Header.Set("User-Agent", tt.ua)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"text":"hello"`) ||
				!strings.Contains(recorder.Body.String(), `"finishReason":"STOP"`) {
				t.Fatalf("stream payload changed: status=%d body=%q", recorder.Code, recorder.Body.String())
			}
			if tickers != tt.tickers {
				t.Fatalf("created %d tickers, want %d (pre-first-token and forwarding)", tickers, tt.tickers)
			}
			if cfg.Streaming.KeepAliveSeconds != 5 || !cfg.Streaming.SendKeepAliveBeforeFT {
				t.Fatal("request changed the shared streaming configuration")
			}
		})
	}
}

func TestGeminiForwardStreamKeepsHeartbeatsForOtherClients(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name      string
		ua        string
		heartbeat bool
	}{
		{"antigravity", "google-antigravity-sdk/0.0.0-dev", false},
		{"genai", "google-genai-sdk/1.75.0", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ticks := make(chan time.Time)
			restore := handlers.SetStreamingKeepAliveTickerFactoryForTest(func(time.Duration) (<-chan time.Time, func()) {
				return ticks, func() {}
			})
			defer restore()
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			c.Request.Header.Set("User-Agent", tt.ua)
			h := NewGeminiAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
				Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 5},
			}, nil))
			data := make(chan []byte)
			go func() {
				if tt.heartbeat {
					ticks <- time.Unix(1, 0)
				}
				data <- []byte(`{"candidates":[{"finishReason":"STOP"}]}`)
				close(data)
			}()
			h.forwardGeminiStream(c, recorder, "", func(err error) {
				if err != nil {
					t.Errorf("stream canceled: %v", err)
				}
			}, data, nil)
			body := recorder.Body.String()
			if got := strings.Contains(body, ": keep-alive\n\n"); got != tt.heartbeat {
				t.Fatalf("heartbeat=%v, want %v; body=%q", got, tt.heartbeat, body)
			}
			if !strings.Contains(body, "data: ") || !strings.Contains(body, `"finishReason":"STOP"`) {
				t.Fatalf("SSE payload lost: %q", body)
			}
		})
	}
}
