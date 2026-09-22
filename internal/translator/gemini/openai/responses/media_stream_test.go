package responses

import (
	"context"
	"strings"
	"testing"
)

func TestMediaStreamKeepsInitializedToolMaps(t *testing.T) {
	var state any
	request := []byte(`{"input":"hello"}`)
	chunk := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"a"}]}}]}`)
	ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.8-flash", request, nil, chunk, &state)
	st := state.(*geminiToResponsesState)
	if st.ToolIdentityMap == nil || st.SanitizedNameMap == nil {
		t.Fatal("empty tool maps must be initialized so media-heavy requests are not reparsed per chunk")
	}
	st.SanitizedNameMap["test"] = "preserved"
	ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.8-flash", request, nil, chunk, &state)
	if st.SanitizedNameMap["test"] != "preserved" {
		t.Fatal("tool maps rebuilt during streaming")
	}
}

func BenchmarkMediaStreamChunk(b *testing.B) {
	request := []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/jpeg;base64,` + strings.Repeat("A", 34*1024*1024) + `"}]}],"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}`)
	chunk := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"a"}]}}]}`)
	var state any
	ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.8-flash", request, nil, chunk, &state)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ConvertGeminiResponseToOpenAIResponses(context.Background(), "gemini-3.8-flash", request, nil, chunk, &state)
	}
}
