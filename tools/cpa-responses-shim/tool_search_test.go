package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const toolSearchDecl = `{"type":"tool_search","execution":"client","description":"# Tool discovery\nuse tool_search","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"],"additionalProperties":false}}`

// liteRequest is the Codex Desktop / Responses Lite shape: tool_search lives
// in an additional_tools input item, there is no top-level tools array.
func liteRequest(model, history string) string {
	return `{"model":"` + model + `","stream":true,"input":[
		{"type":"additional_tools","role":"developer","tools":[
			{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}]},
			` + toolSearchDecl + `]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}` + history + `]}`
}

const toolSearchHistory = `,
	{"type":"reasoning","id":"rs_1","summary":[]},
	{"type":"tool_search_call","id":"tsc_call_9","call_id":"call_9","status":"completed","execution":"client","arguments":{"query":"dummy ping","limit":2}},
	{"type":"tool_search_output","id":"tso_1","call_id":"call_9","status":"completed","execution":"client","tools":[
		{"type":"namespace","name":"mcp__dummy","description":"Tools in the mcp__dummy namespace.","tools":[
			{"type":"function","name":"noches_dummy_ping","description":"Ping.","strict":false,"defer_loading":true,"parameters":{"type":"object","properties":{"word":{"type":"string"}}}}]}]}`

func TestToolSearchGeminiDeclaredAsFunction(t *testing.T) {
	out := rewriteOrFail(t, wireResponses, liteRequest("gemini-3.8-flash", ""))
	if got := gjson.GetBytes(out, "input.0.tools.1.type").String(); got != "function" {
		t.Fatalf("tool_search not flattened to a function: %s", out)
	}
	if gjson.GetBytes(out, "input.0.tools.1.name").String() != "tool_search" ||
		gjson.GetBytes(out, "input.0.tools.1.parameters.required.0").String() != "query" {
		t.Fatalf("tool_search function lost its schema: %s", out)
	}
	if gjson.GetBytes(out, "input.0.tools.0.name").String() != "functions" {
		t.Fatalf("other declarations reshaped: %s", out)
	}
}

func TestToolSearchGeminiTopLevelToolsDeclaredAsFunction(t *testing.T) {
	out := rewriteOrFail(t, wireResponses, `{"model":"gemini-3.8-flash","input":"hi","tools":[`+toolSearchDecl+`,{"type":"function","name":"f","parameters":{}}]}`)
	if gjson.GetBytes(out, "tools.0.type").String() != "function" || gjson.GetBytes(out, "tools.0.name").String() != "tool_search" {
		t.Fatalf("top-level tool_search not flattened: %s", out)
	}
	if gjson.GetBytes(out, "tools.1.name").String() != "f" {
		t.Fatalf("sibling tool changed: %s", out)
	}
}

func TestToolSearchGeminiHistoryRoundTrip(t *testing.T) {
	out := rewriteOrFail(t, wireResponses, liteRequest("gemini-3.8-flash", toolSearchHistory))
	items := gjson.GetBytes(out, "input").Array()
	var call, result gjson.Result
	for _, item := range items {
		switch item.Get("type").String() {
		case "tool_search_call", "tool_search_output":
			t.Fatalf("native tool_search item reached the translated upstream: %s", out)
		case "function_call":
			call = item
		case "function_call_output":
			result = item
		}
	}
	if call.Get("name").String() != "tool_search" || call.Get("call_id").String() != "call_9" ||
		call.Get("id").String() != "fc_call_9" || call.Get("namespace").Exists() {
		t.Fatalf("call not flattened: %s", call.Raw)
	}
	if args := call.Get("arguments"); args.Type != gjson.String || gjson.Parse(args.String()).Get("query").String() != "dummy ping" {
		t.Fatalf("arguments not a JSON string: %s", call.Raw)
	}
	if result.Get("call_id").String() != "call_9" || !strings.Contains(result.Get("output").String(), "mcp__dummy.noches_dummy_ping") {
		t.Fatalf("result not flattened: %s", result.Raw)
	}
	// The discovered tool is declared again so the model can call it, without
	// the defer_loading hint.
	loaded := gjson.GetBytes(out, `input.0.tools.#(name=="mcp__dummy")`)
	if loaded.Get("tools.0.name").String() != "noches_dummy_ping" || loaded.Get("tools.0.defer_loading").Exists() ||
		loaded.Get("tools.0.parameters.properties.word.type").String() != "string" {
		t.Fatalf("discovered tool not declared: %s", out)
	}
	if order := gjson.GetBytes(out, "input.#.type").Raw; !strings.Contains(order, `"reasoning","function_call","function_call_output"`) {
		t.Fatalf("reasoning no longer precedes the call: %s", order)
	}
}

func TestToolSearchGeminiLoadedToolsMergeIntoExistingNamespace(t *testing.T) {
	body := `{"model":"gemini-3.8-flash","tools":[` + toolSearchDecl + `,{"type":"namespace","name":"mcp__dummy","tools":[{"type":"function","name":"already","parameters":{}}]}],
		"input":[{"type":"tool_search_call","call_id":"c","execution":"client","arguments":{"query":"x"}},
		{"type":"tool_search_output","call_id":"c","execution":"client","tools":[{"type":"namespace","name":"mcp__dummy","tools":[
			{"type":"function","name":"already","parameters":{}},{"type":"function","name":"fresh","parameters":{}}]}]}]}`
	out := rewriteOrFail(t, wireResponses, body)
	names := gjson.GetBytes(out, `tools.#(name=="mcp__dummy").tools.#.name`).Raw
	if names != `["already","fresh"]` {
		t.Fatalf("namespace children = %s: %s", names, out)
	}
}

func TestToolSearchRequestsUntouchedOutsideEmulationScope(t *testing.T) {
	history := toolSearchHistory
	for _, model := range []string{"gpt-6-luna", "gpt-6.1-sol", "claude-sonnet-5-5", "devin/swe-2", "cursor/muse-spark-1.3-high"} {
		if _, changed, err := rewriteRequestJSON(wireResponses, []byte(liteRequest(model, history))); err != nil || changed {
			t.Fatalf("%s: changed=%v err=%v", model, changed, err)
		}
	}
	// Other wires never carry the Responses tool_search protocol.
	for _, w := range []wire{wireChat, wireMessages} {
		if _, changed, err := rewriteRequestJSON(w, []byte(liteRequest("gemini-3.8-flash", history))); err != nil || changed {
			t.Fatalf("wire %d: changed=%v err=%v", w, changed, err)
		}
	}
	// Server-executed tool_search is OpenAI's hosted tool, not ours to emulate.
	server := `{"model":"gemini-3.8-flash","input":"hi","tools":[{"type":"tool_search","execution":"server"}]}`
	if _, changed, err := rewriteRequestJSON(wireResponses, []byte(server)); err != nil || changed {
		t.Fatalf("server tool_search: changed=%v err=%v", changed, err)
	}
	// Without a declaration there is nothing to emulate.
	if _, changed, err := rewriteRequestJSON(wireResponses, []byte(`{"model":"gemini-3.8-flash","input":"hi"}`)); err != nil || changed {
		t.Fatalf("no declaration: changed=%v err=%v", changed, err)
	}
}

func TestToolSearchOpenCodeKeepsExistingBehaviour(t *testing.T) {
	out := rewriteOrFail(t, wireResponses, liteRequest("opencode-go/deepseek-v4.1-flash", ""))
	if strings.Contains(string(out), `"tool_search"`) && gjson.GetBytes(out, `tools.#(name=="tool_search")`).Exists() {
		t.Fatalf("OpenCode route got a tool_search function: %s", out)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(liteRequest("opencode-go/deepseek-v4.1-flash", "")))
	if err := rewriteResponsesRequest(req); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := req.Context().Value(toolSearchKey{}).(bool); enabled {
		t.Fatal("tool_search restore enabled for an OpenCode route")
	}
}

func TestRestoreToolSearchCallSpellings(t *testing.T) {
	for _, tc := range []struct{ name, item string }{
		{"lite", `{"type":"function_call","id":"fc_1","call_id":"c1","namespace":"tool_search","name":"tool_search_tool","arguments":"{\"query\":\"q\"}","status":"completed"}`},
		{"emulated", `{"type":"function_call","id":"fc_1","call_id":"c1","name":"tool_search","arguments":"{\"query\":\"q\"}","status":"completed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := decodeTestJSON(t, []byte(tc.item))
			if !restoreToolSearchCall(item) {
				t.Fatal("not restored")
			}
			if item["type"] != "tool_search_call" || item["execution"] != "client" || item["call_id"] != "c1" ||
				item["id"] != "tsc_1" || item["status"] != "completed" {
				t.Fatalf("bad item: %v", item)
			}
			if _, ok := item["name"]; ok {
				t.Fatalf("name kept: %v", item)
			}
			if _, ok := item["namespace"]; ok {
				t.Fatalf("namespace kept: %v", item)
			}
			if item["arguments"].(map[string]any)["query"] != "q" {
				t.Fatalf("arguments not an object: %v", item)
			}
		})
	}
	for _, other := range []string{
		`{"type":"function_call","name":"exec_command","namespace":"functions","arguments":"{}"}`,
		`{"type":"function_call","name":"tool_search_tool","arguments":"{}"}`,
		`{"type":"tool_search_call","call_id":"c","execution":"client","arguments":{}}`,
		`{"type":"message","role":"assistant"}`,
	} {
		item := decodeTestJSON(t, []byte(other))
		if restoreToolSearchCall(item) {
			t.Fatalf("restored unrelated item: %s", other)
		}
	}
}

func TestRestoreToolSearchCallBadArgumentsFallBackToQuery(t *testing.T) {
	item := decodeTestJSON(t, []byte(`{"type":"function_call","namespace":"tool_search","name":"tool_search_tool","arguments":"find the thing"}`))
	if !restoreToolSearchCall(item) || item["arguments"].(map[string]any)["query"] != "find the thing" {
		t.Fatalf("bad arguments not wrapped: %v", item)
	}
	empty := decodeTestJSON(t, []byte(`{"type":"function_call","namespace":"tool_search","name":"tool_search_tool","arguments":""}`))
	if !restoreToolSearchCall(empty) || len(empty["arguments"].(map[string]any)) != 0 {
		t.Fatalf("empty arguments: %v", empty)
	}
}

func toolSearchResponse(t *testing.T, requestBody, contentType, raw string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody))
	if err := rewriteResponsesRequest(req); err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: 200, Request: req, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(raw))}
	if err := modifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	result, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(result)
}

func TestToolSearchNonStreamingRestore(t *testing.T) {
	raw := `{"output":[{"type":"message","role":"assistant"},{"type":"function_call","id":"fc_a","call_id":"c1","namespace":"tool_search","name":"tool_search_tool","arguments":"{\"query\":\"ping\"}"},` +
		`{"type":"function_call","id":"fc_b","call_id":"c2","namespace":"functions","name":"exec_command","arguments":"{}"}]}`
	got := toolSearchResponse(t, liteRequest("gpt-6-luna", ""), "application/json", raw)
	if gjson.Get(got, "output.1.type").String() != "tool_search_call" || gjson.Get(got, "output.1.arguments.query").String() != "ping" ||
		gjson.Get(got, "output.1.execution").String() != "client" || gjson.Get(got, "output.1.id").String() != "tsc_a" {
		t.Fatalf("not restored: %s", got)
	}
	if gjson.Get(got, "output.2.type").String() != "function_call" || gjson.Get(got, "output.2.namespace").String() != "functions" {
		t.Fatalf("regular call reshaped: %s", got)
	}
}

// luna's real stream shape (captured through the proxy): added, argument
// deltas, arguments.done, output_item.done, then completed with the output.
const lunaToolSearchStream = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":21,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"tool_search","name":"tool_search_tool","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\""}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"query\":\"ping\"}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":1,"arguments":"{\"query\":\"ping\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":39,"output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"tool_search","name":"tool_search_tool","arguments":"{\"query\":\"ping\"}","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"tool_search","name":"tool_search_tool","arguments":"{\"query\":\"ping\"}","status":"completed"}]}}

`

func sseEvents(t *testing.T, stream string) []gjson.Result {
	t.Helper()
	var events []gjson.Result
	for _, block := range strings.Split(stream, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				events = append(events, gjson.Parse(strings.TrimPrefix(line, "data: ")))
			}
		}
	}
	return events
}

func TestToolSearchStreamingRestore(t *testing.T) {
	for _, model := range []string{"gpt-6-luna", "gemini-3.8-flash"} {
		t.Run(model, func(t *testing.T) {
			got := toolSearchResponse(t, liteRequest(model, ""), "text/event-stream", lunaToolSearchStream)
			events := sseEvents(t, got)
			var kinds []string
			for _, e := range events {
				kinds = append(kinds, e.Get("type").String())
			}
			want := "response.created,response.output_item.added,response.output_item.done,response.completed"
			if strings.Join(kinds, ",") != want {
				t.Fatalf("events = %v, want argument delta/done dropped:\n%s", kinds, got)
			}
			added, done := events[1].Get("item"), events[2].Get("item")
			if added.Get("type").String() != "tool_search_call" || added.Get("status").String() != "in_progress" ||
				added.Get("execution").String() != "client" || !added.Get("arguments").IsObject() || added.Get("name").Exists() {
				t.Fatalf("added item: %s", added.Raw)
			}
			if done.Get("type").String() != "tool_search_call" || done.Get("call_id").String() != "call_1" ||
				done.Get("arguments.query").String() != "ping" || done.Get("status").String() != "completed" ||
				done.Get("execution").String() != "client" || done.Get("id").String() != "tsc_1" {
				t.Fatalf("done item: %s", done.Raw)
			}
			if events[3].Get("response.output.0.type").String() != "tool_search_call" {
				t.Fatalf("completed output not restored: %s", events[3].Raw)
			}
			if !strings.Contains(got, "event: response.output_item.done\n") || strings.Contains(got, "function_call_arguments") {
				t.Fatalf("event framing wrong:\n%s", got)
			}
		})
	}
}

func TestToolSearchStreamForwardsOtherEventsVerbatim(t *testing.T) {
	stream := ": keepalive\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_9\",\"call_id\":\"c\",\"namespace\":\"functions\",\"name\":\"exec_command\",\"arguments\":\"{}\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_9\",\"delta\":\"{}\"}\n\n" +
		"data: [DONE]\n\n"
	got := toolSearchResponse(t, liteRequest("gpt-6-luna", ""), "text/event-stream", stream)
	if got != stream {
		t.Fatalf("unrelated events changed:\n%s", got)
	}
}

func TestToolSearchResponseScope(t *testing.T) {
	raw := `{"output":[{"type":"function_call","call_id":"c","namespace":"tool_search","name":"tool_search_tool","arguments":"{}"}]}`
	// No tool_search declared: a function that merely carries the name is left alone.
	if got := toolSearchResponse(t, `{"model":"gpt-6-luna","input":"hi"}`, "application/json", raw); got != raw {
		t.Fatalf("response changed without a declaration: %s", got)
	}
	// Other wires.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(liteRequest("gpt-6-luna", "")))
	if err := rewriteResponsesRequest(req); err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{StatusCode: 200, Request: req, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(raw))}
	if err := modifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(resp.Body); string(got) != raw {
		t.Fatalf("chat wire response changed: %s", got)
	}
	// Native routes already answer with tool_search_call; nothing to restore.
	native := `{"output":[{"type":"tool_search_call","id":"tsc_1","call_id":"c","execution":"client","arguments":{"query":"q"}}]}`
	if got := toolSearchResponse(t, liteRequest("gpt-6.1-sol", ""), "application/json", native); got != native {
		t.Fatalf("native tool_search_call reshaped: %s", got)
	}
	// Upstream errors pass through.
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(liteRequest("gpt-6-luna", "")))
	if err := rewriteResponsesRequest(req); err != nil {
		t.Fatal(err)
	}
	resp = &http.Response{StatusCode: 400, Request: req, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(raw))}
	if err := modifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(resp.Body); string(got) != raw {
		t.Fatalf("error response changed: %s", got)
	}
}
