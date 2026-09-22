package main

import (
	"strings"
	"testing"

	antigravity "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/antigravity/openai/responses"
	"github.com/tidwall/gjson"
)

func TestGeminiVideoSurvivesShimAndTranslator(t *testing.T) {
	for _, videoURL := range []string{`"data:video/mp4;base64,QUJD"`, `{"url":"data:video/mp4;base64,QUJD"}`} {
		body := []byte(`{"model":"gemini-3.8-flash","input":[
			{"type":"function_call","call_id":"v1","name":"view_video","arguments":"{}"},
			{"type":"function_call_output","call_id":"v1","output":"Prepared video"},
			{"role":"user","content":[{"type":"input_video","video_url":` + videoURL + `,"video_metadata":{"fps":2,"startOffset":"1s","endOffset":"3s"}}]}
		]}`)
		rewritten, _, err := rewriteResponseJSON(body)
		if err != nil {
			t.Fatal(err)
		}
		out := antigravity.ConvertOpenAIResponsesRequestToAntigravity("gemini-3.8-flash-high", rewritten, true)
		found := false
		gjson.GetBytes(out, "request.contents").ForEach(func(_, turn gjson.Result) bool {
			turn.Get("parts").ForEach(func(_, part gjson.Result) bool {
				inline := part.Get("inlineData")
				if !inline.Exists() {
					inline = part.Get("inline_data")
				}
				if inline.Get("data").String() == "QUJD" {
					found = true
					if part.Get("videoMetadata.fps").Float() != 2 || part.Get("videoMetadata.startOffset").String() != "1s" || part.Get("videoMetadata.endOffset").String() != "3s" {
						t.Fatalf("video metadata lost: %s", part.Raw)
					}
					if !strings.Contains(inline.Raw, "video/mp4") {
						t.Fatalf("video MIME lost: %s", inline.Raw)
					}
				}
				return true
			})
			return true
		})
		if !found {
			t.Fatalf("native video silently dropped: %s", out)
		}
	}
}

func TestGeminiShimPreservesOnlyTypedReasoningCarriers(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","input":[
		{"type":"reasoning","encrypted_content":"cpa-gemini-responses-carrier-v1:next:function:QUJD"},
		{"type":"reasoning","encrypted_content":"foreign-signature"},
		{"type":"function_call_output","output":{"type":"reasoning","encrypted_content":"cpa-gemini-responses-carrier-v1:next:function:QUJD"}},
		{"type":"function_call","thoughtSignature":"foreign-signature"}
	]}`)
	out, changed, err := rewriteResponseJSON(body)
	if err != nil || !changed {
		t.Fatalf("rewrite failed: changed=%v error=%v", changed, err)
	}
	if got := gjson.GetBytes(out, "input.0.encrypted_content").String(); got != "cpa-gemini-responses-carrier-v1:next:function:QUJD" {
		t.Fatalf("typed carrier lost: %s", out)
	}
	for _, field := range []string{"input.1.encrypted_content", "input.2.output.encrypted_content", "input.3.thoughtSignature"} {
		if gjson.GetBytes(out, field).Exists() {
			t.Fatalf("unexpected retained field %s", field)
		}
	}
}
