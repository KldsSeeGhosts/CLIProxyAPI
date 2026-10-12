package registry

import "testing"

func TestSolContextDefaultSurvivesStaleCatalog(t *testing.T) {
	for _, limit := range []int{272000, 500000, 600000} {
		input := []*ModelInfo{
			{ID: "gpt-6.1-sol", ContextLength: limit, MaxCompletionTokens: 128000},
			{ID: "gpt-6-astra", ContextLength: 272000},
			{ID: "gpt-6-luna", ContextLength: 272000},
		}
		got := WithCodexBuiltins(input)
		want := limit
		if limit == 272000 {
			want = 500000
		}
		if got[0].ContextLength != want || got[0].MaxCompletionTokens != 128000 {
			t.Fatalf("Sol limit %d: got context=%d output=%d", limit, got[0].ContextLength, got[0].MaxCompletionTokens)
		}
		if got[1].ContextLength != 272000 || got[2].ContextLength != 272000 {
			t.Fatal("changed Astra or Luna")
		}
	}
}
