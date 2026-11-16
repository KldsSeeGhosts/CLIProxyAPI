package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const openCodeDeclarations = `{"model":"opencode-go/muse-spark-1.3-contributor","input":"hello","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}},{"type":"custom","name":"apply_patch"}]},{"type":"function","name":"mcp__server__tool","parameters":{"type":"object"}}]}`

// serveWireCompat runs a request through WireCompatMiddleware against a stub
// handler that echoes a fixed response. It returns the recorder and the body
// the handler observed, so both directions of the transform can be asserted.
func serveWireCompat(t *testing.T, method, path, reqBody, respCT, respBody string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	return serveWireCompatStatus(t, method, path, reqBody, respCT, http.StatusOK, respBody)
}

func serveWireCompatStatus(t *testing.T, method, path, reqBody, respCT string, status int, respBody string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(WireCompatMiddleware())
	var seen []byte
	handler := func(c *gin.Context) {
		if c.Request.Body != nil {
			seen, _ = io.ReadAll(c.Request.Body)
		}
		if respCT != "" {
			c.Header("Content-Type", respCT)
		}
		c.Status(status)
		_, _ = c.Writer.Write([]byte(respBody))
	}
	engine.POST("/*any", handler)
	engine.GET("/*any", handler)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(reqBody))
	engine.ServeHTTP(recorder, req)
	return recorder, seen
}

func TestOpenCodeResponseTransformation(t *testing.T) {
	reqJSON := []byte(`{
		"model": "opencode-go/muse-spark-1.3-contributor",
		"tools": [
			{
				"type": "namespace",
				"name": "functions",
				"tools": [
					{"type": "function", "name": "exec_command", "description": "run command"},
					{"type": "custom", "name": "apply_patch", "description": "patch file"}
				]
			},
			{
				"type": "namespace",
				"name": "collaboration",
				"tools": [
					{"type": "function", "name": "spawn_agent", "description": "spawn agent"}
				]
			}
		]
	}`)

	var reqRoot map[string]any
	if err := json.Unmarshal(reqJSON, &reqRoot); err != nil {
		t.Fatalf("unmarshal reqJSON: %v", err)
	}
	ids := openCodeToolIdentities(reqRoot)

	if id, ok := ids["functions__exec_command"]; !ok || id.Name != "exec_command" || id.Namespace != "functions" {
		t.Fatalf("expected functions__exec_command mapped, got: %+v", id)
	}
	if id, ok := ids["functions__apply_patch"]; !ok || id.Name != "apply_patch" || id.Namespace != "functions" || id.Kind != "custom" {
		t.Fatalf("expected functions__apply_patch custom mapped, got: %+v", id)
	}
	if id, ok := ids["collaboration__spawn_agent"]; !ok || id.Name != "spawn_agent" || id.Namespace != "collaboration" {
		t.Fatalf("expected collaboration__spawn_agent mapped, got: %+v", id)
	}

	jsonResp := []byte(`{
		"id": "resp-1",
		"output": [
			{
				"type": "function_call",
				"name": "functions__exec_command",
				"arguments": "{\"cmd\":\"ls\"}"
			},
			{
				"type": "function_call",
				"name": "functions__apply_patch",
				"arguments": "{\"input\":\"*** patch ***\"}"
			}
		]
	}`)

	var respRoot map[string]any
	if err := json.Unmarshal(jsonResp, &respRoot); err != nil {
		t.Fatalf("unmarshal jsonResp: %v", err)
	}
	restoreOpenCodeOutput(respRoot, ids)

	outSlice := respRoot["output"].([]any)
	fc := outSlice[0].(map[string]any)
	if fc["name"] != "exec_command" || fc["namespace"] != "functions" {
		t.Fatalf("expected exec_command with namespace functions, got: %+v", fc)
	}

	ctc := outSlice[1].(map[string]any)
	if ctc["name"] != "apply_patch" || ctc["namespace"] != "functions" || ctc["type"] != "custom_tool_call" || ctc["input"] != "*** patch ***" {
		t.Fatalf("expected custom_tool_call apply_patch, got: %+v", ctc)
	}

	sseInput := "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"functions__exec_command\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.done\",\"name\":\"functions__exec_command\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"name\":\"functions__exec_command\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"ctc_1\",\"type\":\"function_call\",\"name\":\"functions__apply_patch\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"ctc_1\",\"name\":\"functions__apply_patch\",\"arguments\":\"{\\\"input\\\":\\\"diff\\\"}\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ctc_1\",\"type\":\"function_call\",\"name\":\"functions__apply_patch\",\"arguments\":\"{\\\"input\\\":\\\"diff\\\"}\"}}\n\n"

	result, err := readWireCompatSSE(strings.NewReader(sseInput), wireCompatModeOpenCode, ids)
	if err != nil {
		t.Fatalf("readWireCompatSSE error: %v", err)
	}
	outStr := string(result)
	if !strings.Contains(outStr, `"name":"exec_command"`) || !strings.Contains(outStr, `"namespace":"functions"`) {
		t.Fatalf("expected restored exec_command in SSE output: %s", outStr)
	}
	if strings.Contains(outStr, "functions__exec_command") {
		t.Fatalf("found un-restored functions__exec_command in SSE output: %s", outStr)
	}
	if !strings.Contains(outStr, `"type":"custom_tool_call"`) || !strings.Contains(outStr, `"input":"diff"`) {
		t.Fatalf("expected custom_tool_call in SSE output: %s", outStr)
	}
}

func TestOpenCodeToolRoundTrip(t *testing.T) {
	root := decodeWireCompatJSON(t, []byte(openCodeDeclarations))
	ids := openCodeToolIdentities(root)
	out, changed, err := rewriteWireCompatRequestJSON(wireKindResponses, []byte(openCodeDeclarations))
	if err != nil || !changed {
		t.Fatalf("rewrite = %t %v", changed, err)
	}
	flattened := decodeWireCompatJSON(t, out)
	if flattened["input"] != "hello" {
		t.Fatal("string input changed")
	}
	if got := flattened["tools"].([]any)[0].(map[string]any)["name"]; got != "functions__exec_command" {
		t.Fatalf("name = %v", got)
	}
	for _, tc := range []struct{ flat, kind, args string }{
		{"functions__exec_command", "function_call", `{"cmd":"pwd"}`},
		{"functions__apply_patch", "custom_tool_call", `{"input":"*** Begin Patch\n*** End Patch"}`},
		{"mcp__server__tool", "function_call", `{}`},
	} {
		t.Run(tc.flat, func(t *testing.T) {
			item := map[string]any{"type": "function_call", "name": tc.flat, "arguments": tc.args, "call_id": "call-1"}
			restoreOpenCodeItem(item, ids)
			if item["type"] != tc.kind || item["call_id"] != "call-1" {
				t.Fatalf("restored = %#v", item)
			}
			if strings.HasPrefix(tc.flat, "functions__") && item["namespace"] != "functions" {
				t.Fatal("missing namespace")
			}
			replay := map[string]any{"input": []any{item}}
			flattenOpenCodeReplay(replay, ids)
			if item["name"] != tc.flat || item["type"] != "function_call" {
				t.Fatalf("replay = %#v", item)
			}
			var want, got any
			_ = json.Unmarshal([]byte(tc.args), &want)
			_ = json.Unmarshal([]byte(wireCompatStringValue(item["arguments"])), &got)
			a, _ := json.Marshal(want)
			b, _ := json.Marshal(got)
			if !bytes.Equal(a, b) {
				t.Fatalf("arguments = %s want %s", b, a)
			}
		})
	}
	unknown := map[string]any{"type": "function_call", "name": "other__exec_command", "arguments": "{}"}
	restoreOpenCodeItem(unknown, ids)
	if unknown["name"] != "other__exec_command" || unknown["namespace"] != nil {
		t.Fatal("unknown call rewritten")
	}
}

func TestOpenCodeSSELargeMultilineAndCustom(t *testing.T) {
	ids := openCodeToolIdentities(decodeWireCompatJSON(t, []byte(openCodeDeclarations)))
	huge := strings.Repeat("x", 100000)
	input := ": keepalive\r\n\r\nevent: response.output_item.added\r\ndata: {\"type\":\"response.output_item.added\",\r\ndata: \"item\":{\"id\":\"patch\",\"type\":\"function_call\",\"name\":\"functions__apply_patch\",\"arguments\":\"\"}}\r\n\r\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"patch","delta":"wrapper"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.done","item_id":"patch","arguments":"{\"input\":\"patch-text\"}"}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"` + huge + `"}` + "\n\n" + "data: [DONE]\n\n"
	result, err := readWireCompatSSE(strings.NewReader(input), wireCompatModeOpenCode, ids)
	if err != nil {
		t.Fatal(err)
	}
	got := string(result)
	for _, want := range []string{": keepalive", `"namespace":"functions"`, `"type":"custom_tool_call"`, `"input":"patch-text"`, huge, "data: [DONE]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s", want[:min(len(want), 60)])
		}
	}
	if strings.Contains(got, "wrapper") || strings.Contains(got, "functions__apply_patch") {
		t.Fatal("untranslated wrapper leaked")
	}
}

func TestOpenCodeResponseScope(t *testing.T) {
	for _, tc := range []struct {
		name, path, model, contentType string
		restore                        bool
	}{
		{"json", "/v1/responses", "opencode-go/muse-spark-1.3-contributor", "application/json", true},
		{"sse", "/v1/responses", "opencode-go/muse-spark-1.3-contributor", "text/event-stream", true},
		{"gemini", "/v1/responses", "gemini-3.8-flash", "application/json", false},
		{"messages", "/v1/messages", "opencode-go/muse-spark-1.3-contributor", "application/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestBody := strings.Replace(openCodeDeclarations, "opencode-go/muse-spark-1.3-contributor", tc.model, 1)
			raw := `{"output":[{"type":"function_call","name":"functions__exec_command","arguments":"{}"}]}`
			if tc.contentType == "text/event-stream" {
				raw = `data: {"type":"response.completed","response":` + raw + "}\n\n"
			}
			recorder, _ := serveWireCompat(t, http.MethodPost, tc.path, requestBody, tc.contentType, raw)
			result := recorder.Body.String()
			if tc.restore {
				if !strings.Contains(result, `"namespace":"functions"`) {
					t.Fatalf("not restored: %s", result)
				}
			} else if result != raw {
				t.Fatalf("non-target response changed: %s", result)
			}
		})
	}
}
