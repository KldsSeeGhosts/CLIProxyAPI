package executor

import (
	"net/http"
	"testing"
	"time"
)

func TestNewOpenAICompatStatusErrUsesRetryAfterHeader(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {"3"}},
	}
	err := newOpenAICompatStatusErr(resp, []byte(`{"error":"rate limited"}`))
	if err.RetryAfter() == nil {
		t.Fatal("RetryAfter() = nil, want 3s")
	}
	if got := *err.RetryAfter(); got != 3*time.Second {
		t.Fatalf("RetryAfter() = %s, want 3s", got)
	}
}

func TestNewOpenAICompatStatusErrUsesResourceExhaustedFallback(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	body := []byte(`{"error":{"message":"data: {\"error\":{\"code\":\"Throttling.ResourceExhausted\"}}"}}`)
	err := newOpenAICompatStatusErr(resp, body)
	if err.RetryAfter() == nil {
		t.Fatal("RetryAfter() = nil, want fallback delay")
	}
	if got := *err.RetryAfter(); got != openAICompatResourceExhaustedRetryAfter {
		t.Fatalf("RetryAfter() = %s, want %s", got, openAICompatResourceExhaustedRetryAfter)
	}
}

func TestNewOpenAICompatStatusErrLeavesGeneric429WithoutHint(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	err := newOpenAICompatStatusErr(resp, []byte(`{"error":{"message":"quota exhausted"}}`))
	if err.RetryAfter() != nil {
		t.Fatalf("RetryAfter() = %s, want nil", *err.RetryAfter())
	}
}

func TestOpenAICompatRetryAfterParsesHTTPDate(t *testing.T) {
	when := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	got := openAICompatRetryAfter(when)
	if got == nil {
		t.Fatal("openAICompatRetryAfter() = nil, want parsed delay")
	}
	if *got <= 0 || *got > 2*time.Second {
		t.Fatalf("parsed delay = %s, want a positive delay no greater than 2s", *got)
	}
}

func TestNewOpenAICompatStatusErrExposesRetryAfterHeader(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	err := newOpenAICompatStatusErr(resp, []byte(`{"error":{"code":"Throttling.ResourceExhausted"}}`))
	if got := err.SafeResponseHeaders().Get("Retry-After"); got != "10" {
		t.Fatalf("Retry-After = %q, want 10", got)
	}
}
