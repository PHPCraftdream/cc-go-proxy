package codexh

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/gofiber/fiber/v2"
)

// codexCall runs one streaming /v1/messages request through handleCodexMessages
// against backend and returns the status, body, access log and stream log.
// codexTestBody replaces the request the helpers send (default: a streaming one).
var codexTestBody string

func TestCodexTurnKind(t *testing.T) {
	user := map[string]any{"type": "message", "role": "user"}
	assistant := map[string]any{"type": "message", "role": "assistant"}
	call := map[string]any{"type": "function_call"}
	output := map[string]any{"type": "function_call_output"}
	reasoning := map[string]any{"type": "reasoning"}

	for _, tc := range []struct {
		name         string
		input        []any
		continuation bool
		newTurn      bool
	}{
		{"empty", nil, false, false},
		{"first user prompt", []any{user}, false, true},
		{"new prompt after an answer", []any{user, assistant, user}, false, true},
		{"new prompt after a tool loop", []any{user, call, output, assistant, user}, false, true},
		{"tool output last", []any{user, call, output}, true, false},
		{"tool output then reminder text (one converted user message)", []any{user, call, output, user}, true, false},
		{"several outputs then text", []any{user, call, call, output, output, user}, true, false},
		{"ends with an assistant call", []any{user, call}, false, false},
		{"reasoning before the user prompt", []any{user, reasoning, assistant, user}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			continuation, newTurn := codexTurnKind(&codex.Request{Input: tc.input})
			if continuation != tc.continuation || newTurn != tc.newTurn {
				t.Errorf("codexTurnKind = (continuation %v, new turn %v), want (%v, %v)", continuation, newTurn, tc.continuation, tc.newTurn)
			}
		})
	}
	if c, n := codexTurnKind(nil); c || n {
		t.Errorf("nil request = (%v, %v), want (false, false)", c, n)
	}
}

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
	oldStates, oldWatch, oldStats := obs.TurnStates, obs.PromptWatch, obs.PromptStats
	obs.TurnStates, obs.PromptWatch, obs.PromptStats = &obs.TurnStateStore{}, &obs.CacheWatch{}, &obs.CacheStats{}
	t.Cleanup(func() { obs.TurnStates, obs.PromptWatch, obs.PromptStats = oldStates, oldWatch, oldStats })
	oldBody, oldSlowTotal := codexTestBody, obs.SlowTotal
	codexTestBody = ""
	obs.SlowTotal = 0
	t.Cleanup(func() { codexTestBody, obs.SlowTotal = oldBody, oldSlowTotal })

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

	_, by := obs.PromptStats.Take()
	if !strings.Contains(by, "turn state sent: ") || !strings.Contains(by, "turn state not sent: ") {
		t.Errorf("integration cache stats = %q, want sent and not-sent groups", by)
	}
}

func TestCodexTurnStateIsRedactedFrom429Logs(t *testing.T) {
	oldStates := obs.TurnStates
	obs.TurnStates = &obs.TurnStateStore{}
	t.Cleanup(func() { obs.TurnStates = oldStates })
	oldBody, oldSlowTotal := codexTestBody, obs.SlowTotal
	codexTestBody = ""
	obs.SlowTotal = 0
	t.Cleanup(func() { codexTestBody, obs.SlowTotal = oldBody, oldSlowTotal })

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

// A continuation that sent a turn state and got none back is normal and says
// nothing; only a request with no state in hand and none returned is noted.
func TestTurnStateNoteIsQuietOnContinuations(t *testing.T) {
	oldStates, oldWatch, oldStats := obs.TurnStates, obs.PromptWatch, obs.PromptStats
	obs.TurnStates, obs.PromptWatch, obs.PromptStats = &obs.TurnStateStore{}, &obs.CacheWatch{}, &obs.CacheStats{}
	t.Cleanup(func() { obs.TurnStates, obs.PromptWatch, obs.PromptStats = oldStates, oldWatch, oldStats })
	oldBody, oldSlowTotal := codexTestBody, obs.SlowTotal
	codexTestBody = ""
	obs.SlowTotal = 0
	t.Cleanup(func() { codexTestBody, obs.SlowTotal = oldBody, oldSlowTotal })

	// A continuation with nothing in hand and nothing returned.
	_, _, _, stream := callTurnState(t, turnStateContinuationBody("quiet"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { turnStateOK(w, "") }))
	if !strings.Contains(stream, "no turn state returned") || strings.Contains(stream, "turn state sent") {
		t.Errorf("no state at all: %q", stream)
	}

	// The turn's first reply gives a token ...
	_, _, _, stream = callTurnState(t, turnStateUserBody("quiet", `""`, "null"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { turnStateOK(w, turnStateA) }))
	if !strings.Contains(stream, "turn state returned") || strings.Contains(stream, "no turn state returned") {
		t.Errorf("first reply: %q", stream)
	}
	// ... a later step sends it, gets none back, and that is not worth a warning.
	_, _, _, stream = callTurnState(t, turnStateContinuationBody("quiet"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { turnStateOK(w, "") }))
	if !strings.Contains(stream, "turn state sent") || strings.Contains(stream, "no turn state returned") || strings.Contains(stream, "turn state returned") {
		t.Errorf("continuation that sent the state: %q", stream)
	}
}

func TestTurnStateBucketsAreIsolatedByFullTuple(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	base := fp("ab", "c", "tools", 1)
	reg := s.Begin(base, false, false, now)
	reg.Store("saved", now)

	for _, other := range []codex.Fingerprint{
		fp("a", "bc", "tools", 1),
		fp("ab", "changed-system", "tools", 1),
		fp("ab", "c", "changed-tools", 1),
	} {
		if got := s.Begin(other, true, false, now).State; got != "" {
			t.Errorf("tuple %q/%q/%q read another bucket: %q", other.Key, other.System, other.Tools, got)
		}
	}
	modelOnly := base
	modelOnly.Model = "different-model"
	modelOnly.Reasoning = "different-reasoning"
	if got := s.Begin(modelOnly, true, false, now).State; got != "saved" {
		t.Errorf("model/control fields changed bucket identity: %q", got)
	}
}

func TestTurnThreadIDIsStableAndUnambiguous(t *testing.T) {
	first := fp("ab", "c", "", 1)
	if a, b := obs.TurnThreadID(obs.TurnKey(first)), obs.TurnThreadID(obs.TurnKey(first)); a != b {
		t.Fatalf("same tuple produced different IDs: %q and %q", a, b)
	}
	if a, b := obs.TurnThreadID(obs.TurnKey(first)), obs.TurnThreadID(obs.TurnKey(fp("a", "bc", "", 1))); a == b {
		t.Fatalf("ambiguous tuples produced the same ID: %q", a)
	}
	if got := len(obs.TurnThreadID(obs.TurnKey(first))); got != 64 {
		t.Errorf("SHA256 hex ID length = %d, want 64", got)
	}
}

func TestTurnStateTTLBoundary(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("key", "system", "tools", 1)
	s.Begin(f, false, false, now).Store("saved", now)
	if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL-time.Nanosecond)).State; got != "saved" {
		t.Fatalf("state just before expiry = %q", got)
	}
	if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL)).State; got != "" {
		t.Errorf("state at expiry = %q, want empty", got)
	}
}

func TestTurnStateMissingStateRefreshDoesNotExtendTTL(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("key", "system", "tools", 1)
	s.Begin(f, false, false, now).Store("saved", now)
	for i := 1; i <= 4; i++ {
		at := now.Add(time.Duration(i) * 5 * time.Minute)
		if got := s.Begin(f, true, false, at).State; got != "saved" {
			t.Fatalf("continuation %d state = %q", i, got)
		}
	}
	if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL)).State; got != "" {
		t.Errorf("repeated Begin extended old state to expiry: %q", got)
	}

	fresh := s.Begin(f, true, false, now.Add(obs.TurnStateTTL+time.Minute))
	fresh.Store("refreshed", now.Add(obs.TurnStateTTL+time.Minute))
	if got := s.Begin(f, true, false, now.Add(2*obs.TurnStateTTL)).State; got != "refreshed" {
		t.Errorf("fresh Store did not refresh TTL: %q", got)
	}
}

func TestTurnStateFIFOEvictsOldestWithoutReorderingRepeatedKeys(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	first := fp("key0000", "system", "tools", 1)
	s.Begin(first, false, false, now).Store("old", now)
	for i := 1; i < obs.MaxTurnStateBuckets; i++ {
		s.Begin(fp(fmt.Sprintf("key%04d", i), "system", "tools", 1), false, false, now)
	}
	s.Begin(first, true, false, now)
	s.Begin(fp("new-key", "system", "tools", 1), false, false, now)
	if len(s.Buckets) != obs.MaxTurnStateBuckets {
		t.Fatalf("bucket count = %d, want %d", len(s.Buckets), obs.MaxTurnStateBuckets)
	}
	if _, exists := s.Buckets[obs.TurnKey(first)]; exists {
		t.Error("repeated first key was reordered instead of evicted")
	}
	if got := s.Begin(fp("key0001", "system", "tools", 1), true, false, now).ThreadID; got != obs.TurnThreadID(obs.TurnKey(fp("key0001", "system", "tools", 1))) {
		t.Errorf("newest key thread ID changed: %q", got)
	}
}

func TestTurnStateNewTurnClearsAndRejectsStaleCompletion(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("key", "system", "tools", 1)
	s.Begin(f, false, false, now).Store("before", now)
	old := s.Begin(f, true, false, now.Add(time.Second))
	cleared := s.Begin(f, false, true, now.Add(2*time.Second))
	if !cleared.NewTurn {
		t.Fatal("new turn was not reported")
	}
	old.Store("stale reply", now.Add(3*time.Second))
	if got := s.Begin(f, true, false, now.Add(4*time.Second)).State; got != "" {
		t.Fatalf("stale completion restored cleared state: %q", got)
	}
}

func TestTurnStateStaleCompletionCannotRestoreEvictedOrExpiredBucket(t *testing.T) {
	t.Run("eviction", func(t *testing.T) {
		s := &obs.TurnStateStore{}
		now := time.Unix(1000, 0)
		f := fp("evicted", "system", "tools", 1)
		stale := s.Begin(f, false, false, now)
		for i := 0; i < obs.MaxTurnStateBuckets; i++ {
			s.Begin(fp(fmt.Sprintf("other%04d", i), "system", "tools", 1), false, false, now)
		}
		s.Begin(f, false, false, now)
		stale.Store("late", now)
		if got := s.Begin(f, true, false, now).State; got != "" {
			t.Errorf("evicted registration restored state: %q", got)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		s := &obs.TurnStateStore{}
		now := time.Unix(1000, 0)
		f := fp("expired", "system", "tools", 1)
		stale := s.Begin(f, false, false, now)
		if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL)).State; got != "" {
			t.Fatalf("state at expiry = %q", got)
		}
		stale.Store("late", now.Add(obs.TurnStateTTL))
		if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL+time.Nanosecond)).State; got != "" {
			t.Errorf("expired registration restored state: %q", got)
		}
	})
}

func TestTurnStateWithoutKeyIsNotStored(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("", "system", "tools", 1)
	reg := s.Begin(f, false, false, now)
	if reg.ThreadID != obs.TurnThreadID(obs.TurnKey(f)) {
		t.Errorf("thread ID = %q", reg.ThreadID)
	}
	reg.Store("private state", now)
	if len(s.Buckets) != 0 {
		t.Errorf("stored %d buckets without a key", len(s.Buckets))
	}
	if got := s.Begin(f, true, false, now).State; got != "" {
		t.Errorf("replayed state without a key: %q", got)
	}
}

func TestTurnStateStoreConcurrentUse(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				f := fp(fmt.Sprintf("key%d", (g*7+i)%80), fmt.Sprintf("system%d", i%3), "tools", 1)
				reg := s.Begin(f, i%2 == 0, i%17 == 0, now.Add(time.Duration(i)*time.Second))
				reg.Store(fmt.Sprintf("state-%d", i), now.Add(time.Duration(i)*time.Second))
			}
		}()
	}
	wg.Wait()
	if len(s.Buckets) > obs.MaxTurnStateBuckets {
		t.Errorf("bucket count = %d, exceeds %d", len(s.Buckets), obs.MaxTurnStateBuckets)
	}
}

func fp(key, system, tools string, items int) codex.Fingerprint {
	return codex.Fingerprint{Key: key, System: system, Tools: tools, NTools: 3, Items: items}
}
