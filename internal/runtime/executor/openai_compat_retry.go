package executor

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// openAICompatResourceExhaustedRetryAfter is a bounded local fallback for
// providers that return a transient model-serving 429 without Retry-After.
// The Qwen Token Plan endpoint currently reports
// Throttling.ResourceExhausted in the response body but omits a retry hint.
const openAICompatResourceExhaustedRetryAfter = 10 * time.Second

// newOpenAICompatStatusErr preserves upstream Retry-After information and
// supplies a bounded retry hint for the known transient model-serving 429
// shape used by the Qwen Token Plan endpoint.
func newOpenAICompatStatusErr(resp *http.Response, body []byte) statusErr {
	if resp == nil {
		return statusErr{msg: string(body)}
	}
	err := statusErr{code: resp.StatusCode, msg: string(body)}
	err.retryAfter = openAICompatRetryAfter(resp.Header.Get("Retry-After"))
	if err.retryAfter == nil && resp.StatusCode == http.StatusTooManyRequests && openAICompatResourceExhausted(body) {
		delay := openAICompatResourceExhaustedRetryAfter
		err.retryAfter = &delay
	}
	return err
}

func openAICompatResourceExhausted(body []byte) bool {
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("throttling.resourceexhausted"))
}

// openAICompatRetryAfter parses both Retry-After's delta-seconds and HTTP-date
// forms. A zero delay is preserved so callers can distinguish an explicit
// immediate retry from an absent header.
func openAICompatRetryAfter(raw string) *time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		delay := time.Duration(seconds) * time.Second
		return &delay
	}
	if when, err := http.ParseTime(raw); err == nil {
		delay := time.Until(when)
		if delay < 0 {
			delay = 0
		}
		return &delay
	}
	return nil
}
