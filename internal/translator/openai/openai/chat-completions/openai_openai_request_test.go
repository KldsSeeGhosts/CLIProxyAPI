package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToOpenAIReusesMatchingModelPayload(t *testing.T) {
	input := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`)

	output := ConvertOpenAIRequestToOpenAI("gpt-test", input, false)

	if &output[0] != &input[0] {
		t.Fatal("matching model caused a payload copy")
	}
}

func TestConvertOpenAIRequestToOpenAIUpdatesDifferentModel(t *testing.T) {
	input := []byte(`{"model":"old-model","messages":[]}`)

	output := ConvertOpenAIRequestToOpenAI("new-model", input, false)

	if model := gjson.GetBytes(output, "model").String(); model != "new-model" {
		t.Fatalf("model = %q, want new-model", model)
	}
}

func TestConvertOpenAIRequestToOpenAINormalizesDeveloperRoleToSystem(t *testing.T) {
	input := []byte(`{"model":"deepseek-v4","messages":[{"role":"developer","content":"you are helpful"},{"role":"user","content":"hi"}]}`)

	output := ConvertOpenAIRequestToOpenAI("deepseek-v4", input, false)

	if got := gjson.GetBytes(output, "messages.0.role").String(); got != "system" {
		t.Fatalf("developer role = %q, want system; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "messages.1.role").String(); got != "user" {
		t.Fatalf("user role = %q, want user; output=%s", got, output)
	}
}

func TestConvertOpenAIRequestToOpenAINormalizesDeveloperRoleWithModelRewrite(t *testing.T) {
	input := []byte(`{"model":"old-model","messages":[{"role":"developer","content":"you are helpful"},{"role":"user","content":"hi"}]}`)

	output := ConvertOpenAIRequestToOpenAI("new-model", input, false)

	if model := gjson.GetBytes(output, "model").String(); model != "new-model" {
		t.Fatalf("model = %q, want new-model", model)
	}
	if got := gjson.GetBytes(output, "messages.0.role").String(); got != "system" {
		t.Fatalf("developer role = %q, want system; output=%s", got, output)
	}
}

func TestConvertOpenAIRequestToOpenAIPreservesSystemRole(t *testing.T) {
	input := []byte(`{"model":"deepseek-v4","messages":[{"role":"system","content":"you are helpful"},{"role":"user","content":"hi"}]}`)

	output := ConvertOpenAIRequestToOpenAI("deepseek-v4", input, false)

	if &output[0] != &input[0] {
		t.Fatal("no developer role present but payload was copied")
	}
}

func TestConvertOpenAIRequestToOpenAIInjectsEmptyReasoningContentForDeepSeekAssistant(t *testing.T) {
	input := []byte(`{"model":"deepseek-v4","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"bye"}]}`)

	output := ConvertOpenAIRequestToOpenAI("deepseek-v4", input, false)

	if !gjson.GetBytes(output, "messages.1.reasoning_content").Exists() {
		t.Fatalf("assistant message missing reasoning_content; output=%s", output)
	}
	if got := gjson.GetBytes(output, "messages.1.reasoning_content").String(); got != "" {
		t.Fatalf("injected reasoning_content = %q, want empty string", got)
	}
}

func TestConvertOpenAIRequestToOpenAIPreservesExistingReasoningContentForDeepSeek(t *testing.T) {
	input := []byte(`{"model":"deepseek-v4","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello","reasoning_content":"thinking here"},{"role":"user","content":"bye"}]}`)

	output := ConvertOpenAIRequestToOpenAI("deepseek-v4", input, false)

	if got := gjson.GetBytes(output, "messages.1.reasoning_content").String(); got != "thinking here" {
		t.Fatalf("reasoning_content = %q, want %q; output=%s", got, "thinking here", output)
	}
}

func TestConvertOpenAIRequestToOpenAIDoesNotInjectReasoningContentForNonDeepSeek(t *testing.T) {
	input := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"bye"}]}`)

	output := ConvertOpenAIRequestToOpenAI("gpt-5", input, false)

	if gjson.GetBytes(output, "messages.1.reasoning_content").Exists() {
		t.Fatalf("non-DeepSeek model got reasoning_content injected; output=%s", output)
	}
}
