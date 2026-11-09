package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

const (
	turnStateA = "Opaque-MiXeD-State/AaZz=="
	turnStateB = "New-State-MiXeD/BBcc=="
	turnStateC = "Retry-Returned-State/CcDd=="
)

func turnStateUserBody(userID, system, tools string) string {
	return fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":%q},"system":%s,"tools":%s,"messages":[{"role":"user","content":"hello"}]}`,
		userID, system, tools)
}

func turnStateContinuationBody(userID string) string {
	return turnStateContinuationWith(userID, `""`, "null")
}

func turnStateContinuationWith(userID, system, tools string) string {
	return fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":%q},"system":%s,"tools":%s,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"done"}]}]}`, userID, system, tools)
}

func turnStateOK(w http.ResponseWriter, token string) {
	if token != "" {
		w.Header().Set("X-Codex-Turn-State", token)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"r"}}`+"\n\n"+
		`data: {"type":"response.completed","response":{"id":"r","usage":{"input_tokens":12000,"output_tokens":1,"input_tokens_details":{"cached_tokens":8000}}}}`+"\n\n")
}

type capturedTurnRequest struct {
	thread string
	id     string
	state  string
}

func TestCodexTurnStateRoutingAndRetry(t *testing.T) {
	oldStates, oldWatch, oldStats := turnStates, promptWatch, promptStats
	turnStates, promptWatch, promptStats = &turnStateStore{}, &cacheWatch{}, &cacheStats{}
	t.Cleanup(func() { turnStates, promptWatch, promptStats = oldStates, oldWatch, oldStats })
	oldBody, oldSlowTotal := codexTestBody, slowTotal
	codexTestBody = ""
	slowTotal = 0
	t.Cleanup(func() { codexTestBody, slowTotal = oldBody, oldSlowTotal })

	var mu sync.Mutex
	var seen []capturedTurnRequest
	capture := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, capturedTurnRequest{
			thread: r.Header.Get("thread-id"),
			id:     r.Header.Get("x-client-request-id"),
			state:  r.Header.Get("x-codex-turn-state"),
		})
	}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		capture(r)
		turnStateOK(w, turnStateA)
	})

	user := turnStateUserBody("stable-session", `""`, "null")
	status, _, _, stream := callTurnState(t, user, backend)
	if status != fiber.StatusOK || !strings.Contains(stream, "new turn") || !strings.Contains(stream, "turn state returned") || strings.Contains(stream, turnStateA) {
		t.Fatalf("initial user status %d, stream log %q", status, stream)
	}
	first := capturedAt(t, &mu, &seen, 0)
	if first.thread == "" || first.id != first.thread || first.state != "" {
		t.Fatalf("initial routing headers = %+v; want stable thread id as request id and no state", first)
	}

	fastRetries(t, 1)
	var retryCalls atomic.Int32
	retryBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		capture(r)
		if retryCalls.Add(1) == 1 {
			w.Header().Set("X-Codex-Turn-State", turnStateB)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"retry"}}`+"\n\n"+
				`data: {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"overloaded"}}`+"\n\n")
			return
		}
		turnStateOK(w, turnStateC)
	})
	status, _, _, retryLog := callTurnState(t, turnStateContinuationBody("stable-session"), retryBackend)
	if status != fiber.StatusOK || !strings.Contains(retryLog, "upstream retry 1/1") || !strings.Contains(retryLog, "turn state sent") || !strings.Contains(retryLog, "turn state returned") || strings.Contains(retryLog, turnStateA) || strings.Contains(retryLog, turnStateB) || strings.Contains(retryLog, turnStateC) {
		t.Fatalf("continuation with retry status %d, stream log %q", status, retryLog)
	}
	second, third := capturedAt(t, &mu, &seen, 1), capturedAt(t, &mu, &seen, 2)
	for i, got := range []capturedTurnRequest{second, third} {
		if got.thread != first.thread || got.id != first.thread {
			t.Errorf("retry request %d routing ids = %+v, want stable thread %q", i+1, got, first.thread)
		}
	}
	if second.state != turnStateA || third.state != turnStateB {
		t.Fatalf("continuation/retry states = %q, %q; want first and freshly returned state", second.state, third.state)
	}

	changed := []struct {
		label string
		body  string
	}{
		{"key", turnStateContinuationBody("other-session")},
		{"system", turnStateContinuationWith("stable-session", `"different"`, "null")},
		{"tools", turnStateContinuationWith("stable-session", `""`, `[{"name":"Read","input_schema":{"type":"object"}}]`)},
	}
	for i, tc := range changed {
		status, _, _, _ = callTurnState(t, tc.body, backend)
		got := capturedAt(t, &mu, &seen, 3+i)
		if status != fiber.StatusOK || got.state != "" || got.thread == first.thread || got.id != got.thread {
			t.Errorf("changed %s status=%d headers=%+v; want isolated thread and no state", tc.label, status, got)
		}
	}
	status, _, _, _ = callTurnState(t, turnStateContinuationBody("stable-session"), backend)
	if status != fiber.StatusOK || capturedAt(t, &mu, &seen, 6).state != turnStateC {
		t.Errorf("isolated continuation did not preserve original bucket state")
	}

	missingHeaderBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		capture(r)
		turnStateOK(w, "")
	})
	status, _, _, stream = callTurnState(t, user, missingHeaderBackend)
	if status != fiber.StatusOK || !strings.Contains(stream, "new turn") || !strings.Contains(stream, "no turn state returned") || strings.Contains(stream, turnStateA) || strings.Contains(stream, turnStateB) || strings.Contains(stream, turnStateC) {
		t.Errorf("new user status %d, stream log %q", status, stream)
	}
	fourth := capturedAt(t, &mu, &seen, 7)
	if fourth.thread != first.thread || fourth.state != "" {
		t.Errorf("new-turn headers = %+v, want same thread and no stale state", fourth)
	}

	_, by := promptStats.Take()
	if !strings.Contains(by, "turn state sent: ") || !strings.Contains(by, "turn state not sent: ") {
		t.Errorf("integration cache stats = %q, want sent and not-sent groups", by)
	}
}

func TestCodexTurnStateIsRedactedFrom429Logs(t *testing.T) {
	oldStates := turnStates
	turnStates = &turnStateStore{}
	t.Cleanup(func() { turnStates = oldStates })
	oldBody, oldSlowTotal := codexTestBody, slowTotal
	codexTestBody = ""
	slowTotal = 0
	t.Cleanup(func() { codexTestBody, slowTotal = oldBody, oldSlowTotal })

	const secret = "Very-Secret-Opaque-Turn-State/DO-NOT-LOG"
	var calls atomic.Int32
	var shortRetryState string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("X-Codex-Turn-State", secret)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"wait"}}`)
			return
		}
		shortRetryState = r.Header.Get("x-codex-turn-state")
		turnStateOK(w, "")
	})
	status, _, _, stream := callTurnState(t, turnStateUserBody("redaction-session", `""`, "null"), backend)
	if status != fiber.StatusOK || !strings.Contains(stream, "upstream rate limit: retrying") || !strings.Contains(stream, "new turn") || !strings.Contains(stream, "turn state sent") || shortRetryState != secret {
		t.Fatalf("short 429 status %d, retry state %q, stream %q", status, shortRetryState, stream)
	}
	if strings.Contains(stream, secret) {
		t.Fatalf("short-429 log exposed turn state: %q", stream)
	}

	// A later continuation must still use the value returned on the short 429.
	var continuationState string
	var finalAccess, finalStream string
	continuation := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		continuationState = r.Header.Get("x-codex-turn-state")
		w.Header().Set("Retry-After", "120")
		w.Header().Set("X-Codex-Turn-State", secret)
		w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limit"}}`)
	})
	status, _, finalAccess, finalStream = callTurnState(t, turnStateContinuationBody("redaction-session"), continuation)
	if status != fiber.StatusTooManyRequests || continuationState != secret {
		t.Fatalf("final 429 status %d, continuation sent %q; want state from prior attempt", status, continuationState)
	}
	if strings.Contains(finalAccess+finalStream, secret) || !strings.Contains(finalAccess, "x-ratelimit-remaining-requests=0") {
		t.Errorf("final-429 logs access=%q stream=%q; state must be redacted and normal limits retained", finalAccess, finalStream)
	}
}

func callTurnState(t *testing.T, body string, backend http.Handler) (int, string, string, string) {
	t.Helper()
	codexTestBody = body
	status, response, access, stream := codexCall(t, backend, time.Minute)
	return status, response, access, stream
}

func capturedAt(t *testing.T, mu *sync.Mutex, seen *[]capturedTurnRequest, index int) capturedTurnRequest {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if index >= len(*seen) {
		t.Fatalf("captured %d upstream attempts, want attempt %d", len(*seen), index+1)
	}
	return (*seen)[index]
}
