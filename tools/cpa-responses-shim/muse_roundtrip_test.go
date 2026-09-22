package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const museDeclarations = `{"model":"opencode-go/muse-spark-1.3-contributor","input":"hello","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}},{"type":"custom","name":"apply_patch"}]},{"type":"function","name":"mcp__server__tool","parameters":{"type":"object"}}]}`

func TestMuseToolRoundTrip(t *testing.T) {
	root := decodeTestJSON(t, []byte(museDeclarations))
	ids := museToolIdentities(root)
	out, changed, err := rewriteResponseJSON([]byte(museDeclarations))
	if err != nil || !changed {
		t.Fatalf("rewrite = %t %v", changed, err)
	}
	flattened := decodeTestJSON(t, out)
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
			restoreMuseItem(item, ids)
			if item["type"] != tc.kind || item["call_id"] != "call-1" {
				t.Fatalf("restored = %#v", item)
			}
			if strings.HasPrefix(tc.flat, "functions__") && item["namespace"] != "functions" {
				t.Fatal("missing namespace")
			}
			replay := map[string]any{"input": []any{item}}
			flattenMuseReplay(replay, ids)
			if item["name"] != tc.flat || item["type"] != "function_call" {
				t.Fatalf("replay = %#v", item)
			}
			var want, got any
			_ = json.Unmarshal([]byte(tc.args), &want)
			_ = json.Unmarshal([]byte(stringValue(item["arguments"])), &got)
			a, _ := json.Marshal(want)
			b, _ := json.Marshal(got)
			if !bytes.Equal(a, b) {
				t.Fatalf("arguments = %s want %s", b, a)
			}
		})
	}
	unknown := map[string]any{"type": "function_call", "name": "other__exec_command", "arguments": "{}"}
	restoreMuseItem(unknown, ids)
	if unknown["name"] != "other__exec_command" || unknown["namespace"] != nil {
		t.Fatal("unknown call rewritten")
	}
}

func TestMuseSSELargeMultilineAndCustom(t *testing.T) {
	ids := museToolIdentities(decodeTestJSON(t, []byte(museDeclarations)))
	huge := strings.Repeat("x", 100000)
	input := ": keepalive\r\n\r\nevent: response.output_item.added\r\ndata: {\"type\":\"response.output_item.added\",\r\ndata: \"item\":{\"id\":\"patch\",\"type\":\"function_call\",\"name\":\"functions__apply_patch\",\"arguments\":\"\"}}\r\n\r\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"patch","delta":"wrapper"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.done","item_id":"patch","arguments":"{\"input\":\"patch-text\"}"}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"` + huge + `"}` + "\n\n" + "data: [DONE]\n\n"
	var output bytes.Buffer
	if err := translateMuseSSE(strings.NewReader(input), &output, ids); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{": keepalive", `"namespace":"functions"`, `"type":"custom_tool_call"`, `"input":"patch-text"`, huge, "data: [DONE]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s", want[:min(len(want), 60)])
		}
	}
	if strings.Contains(got, "wrapper") || strings.Contains(got, "functions__apply_patch") {
		t.Fatal("untranslated wrapper leaked")
	}
}

func TestMuseHTTPResponseScope(t *testing.T) {
	for _, tc := range []struct {
		name, path, model, contentType string
		restore                        bool
	}{
		{"json", "/v1/responses", "opencode-go/muse-spark-1.3-contributor", "application/json", true},
		{"sse", "/responses", "opencode-go/muse-spark-1.3-contributor", "text/event-stream", true},
		{"gemini", "/v1/responses", "gemini-3.8-flash", "application/json", false},
		{"messages", "/v1/messages", "opencode-go/muse-spark-1.3-contributor", "application/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestBody := strings.Replace(museDeclarations, "opencode-go/muse-spark-1.3-contributor", tc.model, 1)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(requestBody))
			if err := rewriteResponsesRequest(req); err != nil {
				t.Fatal(err)
			}
			raw := `{"output":[{"type":"function_call","name":"functions__exec_command","arguments":"{}"}]}`
			if tc.contentType == "text/event-stream" {
				raw = `data: {"type":"response.completed","response":` + raw + "}\n\n"
			}
			resp := &http.Response{StatusCode: 200, Request: req, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(raw))}
			if err := museModifyResponse(resp); err != nil {
				t.Fatal(err)
			}
			result, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if tc.restore {
				if !bytes.Contains(result, []byte(`"namespace":"functions"`)) {
					t.Fatalf("not restored: %s", result)
				}
			} else if string(result) != raw {
				t.Fatalf("non-target response changed: %s", result)
			}
		})
	}
}
