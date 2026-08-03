// Package openai provides request translation functionality for OpenAI to OpenAI API compatibility.
// It converts OpenAI Chat Completions requests into OpenAI-compatible JSON using gjson/sjson only.
package chat_completions

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// isDeepSeekModel reports whether modelName targets a DeepSeek backend.
// DeepSeek's thinking API requires reasoning_content on assistant messages in
// multi-turn conversations, even when the client omits it from prior turns.
func isDeepSeekModel(modelName string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(modelName)), "deepseek")
}

// ConvertOpenAIRequestToOpenAI converts an OpenAI Chat Completions request (raw JSON)
// into a complete OpenAI request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Normalizes the "developer" role to "system" in messages[] so that OpenAI-compatible
// backends that predate the developer-role rename (e.g. DeepSeek) do not reject the request.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in OpenAI API format
func ConvertOpenAIRequestToOpenAI(modelName string, inputRawJSON []byte, _ bool) []byte {
	out := normalizeOpenAIChatDeveloperRoles(inputRawJSON)
	if isDeepSeekModel(modelName) {
		out = ensureDeepSeekAssistantReasoningContent(out)
	}

	currentModel := gjson.GetBytes(out, "model")
	if currentModel.Type == gjson.String && currentModel.String() == modelName {
		return out
	}

	// Update the "model" field in the JSON payload with the provided modelName
	// The sjson.SetBytes function returns a new byte slice with the updated JSON.
	updatedJSON, err := sjson.SetBytes(out, "model", modelName)
	if err != nil {
		// If there's an error, return the original JSON or handle the error appropriately.
		// For now, we'll return the original, but in a real scenario, logging or a more robust error
		// handling mechanism would be needed.
		return out
	}
	return updatedJSON
}

// normalizeOpenAIChatDeveloperRoles rewrites messages[].role == "developer" to "system".
// OpenAI introduced "developer" as the renamed "system" role, but many OpenAI-compatible
// backends only accept "system" and reject "developer" with a deserialization error.
// ensureDeepSeekAssistantReasoningContent injects an empty reasoning_content field
// on every assistant message that lacks one. DeepSeek's thinking mode rejects
// multi-turn requests where a prior assistant turn is missing reasoning_content,
// even though the field was present in the response that produced that turn.
// An empty string satisfies the validator and avoids a deserialization error.
func ensureDeepSeekAssistantReasoningContent(rawJSON []byte) []byte {
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return rawJSON
	}
	var out []byte = rawJSON
	changed := false
	messages.ForEach(func(idx, message gjson.Result) bool {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		if role != "assistant" {
			return true
		}
		if message.Get("reasoning_content").Exists() {
			return true
		}
		path := "messages." + idx.String() + ".reasoning_content"
		var err error
		out, err = sjson.SetBytes(out, path, "")
		if err != nil {
			return true
		}
		changed = true
		return true
	})
	if !changed {
		return rawJSON
	}
	return out
}

func normalizeOpenAIChatDeveloperRoles(rawJSON []byte) []byte {
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return rawJSON
	}
	var out []byte = rawJSON
	changed := false
	messages.ForEach(func(idx, message gjson.Result) bool {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		if role != "developer" {
			return true
		}
		path := "messages." + idx.String() + ".role"
		var err error
		out, err = sjson.SetBytes(out, path, "system")
		if err != nil {
			return true
		}
		changed = true
		return true
	})
	if !changed {
		return rawJSON
	}
	return out
}
