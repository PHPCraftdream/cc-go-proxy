package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

// usageLimitEvent is how the backend ends a reply when the plan's limit is used up.
const usageLimitEvent = `{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":324230},"status_code":429}`

func sseBackend(events ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			_, _ = w.Write([]byte("data: " + ev + "\n\n"))
		}
	})
}

// A used-up limit in a non-stream reply is a 429 Claude Code does not send again.
func TestNonStreamUsageLimitIs429WithoutRetry(t *testing.T) {
	nonStreamBody(t)
	status, body, access, _ := codexCall(t, sseBackend(usageLimitEvent), time.Minute)
	if status != fiber.StatusTooManyRequests || !strings.Contains(body, `"rate_limit_error"`) ||
		!strings.Contains(body, "The usage limit has been reached") {
		t.Fatalf("status %d, body %q", status, body)
	}
	if got := lastCodexReply.Get("x-should-retry"); got != "false" {
		t.Errorf("x-should-retry = %q, want false", got)
	}
	if !strings.Contains(access, " 429 - ") {
		t.Errorf("access log = %q", access)
	}
}

// Any other failure of a non-stream reply stays a 502 the client may retry.
func TestNonStreamOtherErrorStays502(t *testing.T) {
	nonStreamBody(t)
	status, body, _, _ := codexCall(t, sseBackend(`{"type":"error","error":{"type":"invalid_request","code":"cyber_policy","message":"flagged"}}`), time.Minute)
	if status != fiber.StatusBadGateway || !strings.Contains(body, "cyber_policy: flagged") {
		t.Fatalf("status %d, body %q", status, body)
	}
	if got := lastCodexReply.Get("x-should-retry"); got != "" {
		t.Errorf("x-should-retry = %q, want none", got)
	}
}

// The limit over HTTP is already a 429; it also tells Claude Code not to retry.
func TestHTTPUsageLimitTellsNotToRetry(t *testing.T) {
	status, _, _, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`))
	}), time.Minute)
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if got := lastCodexReply.Get("x-should-retry"); got != "false" {
		t.Errorf("x-should-retry = %q, want false", got)
	}
}

// A short rate limit stays retryable.
func TestHTTPRateLimitStaysRetryable(t *testing.T) {
	status, _, _, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_exceeded","message":"Rate limit exceeded"}}`))
	}), time.Minute)
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if got := lastCodexReply.Get("x-should-retry"); got != "" {
		t.Errorf("x-should-retry = %q, want none", got)
	}
}
