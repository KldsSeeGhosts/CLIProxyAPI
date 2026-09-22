package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type museIdentityKey struct{}
type museIdentity struct{ Name, Namespace, Kind string }
type museIdentities map[string]museIdentity

// Record declarations before flattening. Never infer namespaces from unknown
// response names: MCP names and literal double underscores are valid names.
func museToolIdentities(root map[string]any) museIdentities {
	ids := museIdentities{}
	var visit func(any, string)
	visit = func(value any, namespace string) {
		tool, ok := value.(map[string]any)
		if !ok {
			return
		}
		if stringValue(tool["type"]) == "namespace" {
			for _, child := range rawToolSlice(tool["tools"]) {
				visit(child, stringValue(tool["name"]))
			}
			return
		}
		_, flat, ok := normalizeMuseTool(tool, namespace)
		if !ok {
			return
		}
		name := stringValue(tool["name"])
		if name == "" {
			if fn, ok := tool["function"].(map[string]any); ok {
				name = stringValue(fn["name"])
			}
		}
		id := museIdentity{name, namespace, stringValue(tool["type"])}
		if prior, exists := ids[flat]; !exists || (prior.Namespace == "" && namespace != "") {
			ids[flat] = id
		}
	}
	for _, tool := range rawToolSlice(root["tools"]) {
		visit(tool, "")
	}
	for _, item := range rawToolSlice(root["input"]) {
		if m, ok := item.(map[string]any); ok && stringValue(m["type"]) == "additional_tools" {
			for _, tool := range rawToolSlice(m["tools"]) {
				visit(tool, "")
			}
		}
	}
	return ids
}

func flattenMuseReplay(root map[string]any, ids museIdentities) bool {
	changed := false
	for _, value := range rawToolSlice(root["input"]) {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		kind := stringValue(item["type"])
		if kind == "custom_tool_call_output" {
			item["type"] = "function_call_output"
			changed = true
			continue
		}
		if kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		for flat, id := range ids {
			if stringValue(item["name"]) != id.Name || stringValue(item["namespace"]) != id.Namespace {
				continue
			}
			if stringValue(item["name"]) != flat || id.Namespace != "" {
				item["name"] = flat
				delete(item, "namespace")
				changed = true
			}
			if kind == "custom_tool_call" && id.Kind == "custom" {
				args, _ := json.Marshal(map[string]any{"input": item["input"]})
				item["type"] = "function_call"
				item["arguments"] = string(args)
				delete(item, "input")
				changed = true
			}
			break
		}
	}
	return changed
}

func restoreMuseItem(item map[string]any, ids museIdentities) (museIdentity, bool) {
	kind := stringValue(item["type"])
	if kind != "function_call" && kind != "custom_tool_call" {
		return museIdentity{}, false
	}
	id, ok := ids[stringValue(item["name"])]
	if !ok {
		return id, false
	}
	item["name"] = id.Name
	if id.Namespace != "" {
		item["namespace"] = id.Namespace
	} else {
		delete(item, "namespace")
	}
	if id.Kind == "custom" {
		item["type"] = "custom_tool_call"
		if args, ok := item["arguments"].(string); ok {
			var wrapper struct {
				Input string `json:"input"`
			}
			if args == "" {
				item["input"] = ""
			} else if json.Unmarshal([]byte(args), &wrapper) == nil {
				item["input"] = wrapper.Input
			} else {
				item["input"] = args
			}
			delete(item, "arguments")
		}
	}
	return id, true
}

func restoreMuseOutput(root map[string]any, ids museIdentities) {
	for _, value := range rawToolSlice(root["output"]) {
		if item, ok := value.(map[string]any); ok {
			restoreMuseItem(item, ids)
		}
	}
}

// Closing a transformed stream also closes upstream, including when the client
// disconnects while upstream is idle.
type museResponseBody struct {
	io.Reader
	upstream io.ReadCloser
	pipe     *io.PipeReader
}

func (b *museResponseBody) Close() error { _ = b.pipe.Close(); return b.upstream.Close() }

func museModifyResponse(resp *http.Response) error {
	ids, ok := resp.Request.Context().Value(museIdentityKey{}).(museIdentities)
	if !ok || len(ids) == 0 || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		source := resp.Body
		reader, writer := io.Pipe()
		resp.Body = &museResponseBody{Reader: reader, upstream: source, pipe: reader}
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		go func() {
			err := translateMuseSSE(source, writer, ids)
			_ = source.Close()
			_ = writer.CloseWithError(err)
		}()
		return nil
	}
	if !strings.Contains(ct, "application/json") {
		return nil
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) == nil {
		restoreMuseOutput(root, ids)
		if encoded, err := json.Marshal(root); err == nil {
			raw = encoded
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	resp.Header.Set("Content-Length", fmt.Sprint(len(raw)))
	return nil
}

func translateMuseSSE(source io.Reader, target io.Writer, ids museIdentities) error {
	reader := bufio.NewReader(source)
	custom := map[string]bool{}
	var lines []string
	flush := func() error {
		if len(lines) == 0 {
			_, err := io.WriteString(target, "\n")
			return err
		}
		var data []string
		for _, line := range lines {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		joined := strings.Join(data, "\n")
		var event map[string]any
		if len(data) == 0 || json.Unmarshal([]byte(joined), &event) != nil {
			_, err := io.WriteString(target, strings.Join(lines, "\n")+"\n\n")
			lines = nil
			return err
		}
		kind := stringValue(event["type"])
		if kind == "response.function_call_arguments.done" {
			if id, ok := ids[stringValue(event["name"])]; ok {
				event["name"] = id.Name
				if id.Namespace != "" {
					event["namespace"] = id.Namespace
				}
			}
		}
		if item, ok := event["item"].(map[string]any); ok {
			if id, found := restoreMuseItem(item, ids); found && id.Kind == "custom" {
				custom[stringValue(item["id"])] = true
			}
		}
		if response, ok := event["response"].(map[string]any); ok {
			restoreMuseOutput(response, ids)
		}
		restoreMuseOutput(event, ids)
		if strings.HasPrefix(kind, "response.function_call_arguments.") && custom[stringValue(event["item_id"])] {
			// Function arguments contain a JSON wrapper, not freeform text. The complete
			// input is emitted on done, avoiding invalid partial JSON in custom deltas.
			if strings.HasSuffix(kind, ".delta") {
				lines = nil
				return nil
			}
			if strings.HasSuffix(kind, ".done") {
				var wrapper struct {
					Input string `json:"input"`
				}
				args := stringValue(event["arguments"])
				if err := json.Unmarshal([]byte(args), &wrapper); err != nil {
					wrapper.Input = args
				}
				event["type"] = "response.custom_tool_call_input.done"
				event["input"] = wrapper.Input
				delete(event, "arguments")
			}
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "data:") {
				continue
			}
			if strings.HasPrefix(line, "event:") && kind != stringValue(event["type"]) {
				line = "event: " + stringValue(event["type"])
			}
			if _, err := io.WriteString(target, line+"\n"); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(target, "data: %s\n\n", encoded)
		lines = nil
		return err
	}
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if line == "" {
				if e := flush(); e != nil {
					return e
				}
			} else {
				lines = append(lines, line)
			}
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			if len(lines) > 0 {
				return flush()
			}
			return nil
		}
	}
}
