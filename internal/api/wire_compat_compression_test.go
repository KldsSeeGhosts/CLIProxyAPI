package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestWireCompatRestoresToolsBeforeResponseCompression(t *testing.T) {
	const request = `{"model":"opencode-go/deepseek-v4.1-flash","input":"hi","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command","parameters":{}}]}]}`
	for _, encoding := range []string{"gzip", "zstd"} {
		for _, stream := range []bool{false, true} {
			name := encoding + "/json"
			if stream {
				name = encoding + "/sse"
			}
			t.Run(name, func(t *testing.T) {
				enabled := true
				cfg := &config.Config{}
				cfg.ServerResponseCompression.Enabled = &enabled
				cfg.ServerResponseCompression.MinBytes = 1
				engine := gin.New()
				engine.Use(responseCompressionMiddleware(cfg))
				engine.Use(middleware.WireCompatMiddleware())
				engine.POST("/v1/responses", func(c *gin.Context) {
					if stream {
						c.Header("Content-Type", "text/event-stream")
						_, _ = c.Writer.WriteString("event: response.output_item.done\ndata: " +
							`{"type":"response.output_item.done","item":{"type":"function_call","name":"functions__exec_command","arguments":"{}"}}` + "\n\n")
						c.Writer.Flush()
					} else {
						c.Data(http.StatusCreated, "application/json", []byte(`{"output":[{"type":"function_call","name":"functions__exec_command","arguments":"{}"}]}`))
					}
				})
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(request))
				req.Header.Set("Accept-Encoding", encoding)
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, req)
				if got := response.Header().Get("Content-Encoding"); got != encoding {
					t.Fatalf("encoding=%q, want %q", got, encoding)
				}
				body := string(compressionTestBody(t, encoding, response.Body.Bytes()))
				if !strings.Contains(body, `"namespace":"functions"`) || strings.Contains(body, "functions__exec_command") {
					t.Fatalf("compressed tool output not restored: %s", body)
				}
				if !stream && response.Code != http.StatusCreated {
					t.Fatalf("response status lost: %d", response.Code)
				}
			})
		}
	}
}
