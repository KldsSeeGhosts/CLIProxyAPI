package auth

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestDetachedStreamContextSurvivesGinReuse(t *testing.T) {
	original := &gin.Context{Request: httptest.NewRequest("POST", "/v1/responses", nil)}
	original.Request.Header.Set("User-Agent", "codex_cli_rs/0.159.2")
	original.Set("userApiKey", "fixture-key")
	original.Set("accessProvider", "fixture-provider")
	original.Set("accessMetadata", map[string]string{"tenant": "fixture"})
	detached := detachGinContext(original)
	original.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	original.Keys = nil
	original.Set("userApiKey", "next-request-key")

	if !IsCodexClientRequest(context.WithValue(context.Background(), "gin", detached)) {
		t.Fatal("stream lost its original client identity after gin context reuse")
	}
	if key, ok := detached.Get("userApiKey"); !ok || key != "fixture-key" {
		t.Fatalf("stream attribution changed to another request: %v", key)
	}
	if provider, ok := detached.Get("accessProvider"); !ok || provider != "fixture-provider" {
		t.Fatal("stream lost access provider")
	}
	if _, ok := detached.Get("accessMetadata"); !ok {
		t.Fatal("stream lost access metadata")
	}
}
