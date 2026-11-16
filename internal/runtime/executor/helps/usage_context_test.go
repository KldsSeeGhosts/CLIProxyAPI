package helps

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestUsagePublishRetainsOriginalRequestAttribution(t *testing.T) {
	pooled := &gin.Context{Request: httptest.NewRequest("POST", "/v1/responses", nil)}
	pooled.Set("userApiKey", "fixture-first-key")
	pooled.Set("accessProvider", "fixture-first-provider")
	ctx := context.WithValue(context.Background(), "gin", pooled)
	reporter := NewUsageReporter(ctx, "fixture-provider", "fixture-model", nil)
	pooled.Request = httptest.NewRequest("POST", "/next-request", nil)
	pooled.Keys = nil
	pooled.Set("userApiKey", "fixture-next-key")

	detached, ok := reporter.publishContext(ctx).Value("gin").(*gin.Context)
	if !ok || detached == pooled || detached.Request.URL.Path != "/v1/responses" {
		t.Fatal("usage publication retained the pooled request context")
	}
	if key, _ := detached.Get("userApiKey"); key != "fixture-first-key" {
		t.Fatalf("usage publication changed request attribution: %v", key)
	}
	if provider, _ := detached.Get("accessProvider"); provider != "fixture-first-provider" {
		t.Fatal("usage publication lost its access provider")
	}
}
