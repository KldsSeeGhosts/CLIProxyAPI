package modelconfig

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestOpenAICompatModelLimitChangesInvalidateHash(t *testing.T) {
	base := config.OpenAICompatibilityModel{Name: "upstream", Alias: "public", MaxContextLength: 262000, MaxCompletionTokens: 128000}
	before := ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{base})
	for _, field := range []string{"context", "completion"} {
		changed := base
		if field == "context" {
			changed.MaxContextLength++
		} else {
			changed.MaxCompletionTokens++
		}
		if after := ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{changed}); after == before {
			t.Errorf("%s-only change does not trigger model reload", field)
		}
	}
}
