package ws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/coder/websocket"
)

func TestWSManagerCanceledDialDoesNotDisable(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: 10 * time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := m.Call(ctx, "http://127.0.0.1:1", testSession(t), "pre-cancel", makeWSManagerTestRequest("pre", wsManagerUser("pre"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled call error = %v", err)
	}

	started := make(chan struct{})
	var handshakes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handshakes.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return
		}
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		_ = readWSRequest(t, ws)
		writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"cancel-retry"}}`)
	}))
	defer server.Close()
	callCtx, cancelCall := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := m.Call(callCtx, server.URL, testSession(t), "during", makeWSManagerTestRequest("during", wsManagerUser("during")))
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake did not start")
	}
	cancelCall()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled handshake error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled handshake did not return")
	}
	if disabled := wsDisabledUntil(t, m); !disabled.IsZero() {
		t.Fatalf("canceled handshake disabled manager until %s", disabled)
	}
	resp, _, err := m.Call(context.Background(), server.URL, testSession(t), "during", makeWSManagerTestRequest("during", wsManagerUser("retry")))
	if err != nil {
		t.Fatalf("retry after canceled dial: %v", err)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes = %d, want 2", got)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestWSManagerMidstreamBreakPreservesReadErrorAndFreshConnection(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	resp, _, err := backend.call(t, m, "broken", makeWSManagerTestRequest("broken", wsManagerUser("one")))
	if err != nil {
		t.Fatal(err)
	}
	first := backend.next(t)
	m.mu.Lock()
	firstConn := m.testEntry("broken").conn
	m.mu.Unlock()
	if err := first.respond(`{"type":"response.output_text.delta","delta":"partial"}`); err != nil {
		t.Fatal(err)
	}
	if err := first.abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("midstream break returned EOF without an error")
	} else if underlying := firstConn.Err(); underlying == nil || err != underlying {
		t.Fatalf("body read error %v was not passed through unchanged from conn.Err() %v", err, underlying)
	}
	_ = resp.Body.Close()
	resp, info, err := backend.call(t, m, "broken", makeWSManagerTestRequest("broken", wsManagerUser("two")))
	if err != nil {
		t.Fatal(err)
	}
	second := backend.next(t)
	if info.Mode != "full" || info.Reused || second.connection == first.connection || wsManagerPrevious(second) != "" {
		t.Fatalf("post-break call info=%+v connection=%d old=%d previous=%q", info, second.connection, first.connection, wsManagerPrevious(second))
	}
	if err := second.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
}

func TestWSManagerBreakBeforeContentUsesFreshFullConnection(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	resp, _, err := backend.call(t, m, "early-break", makeWSManagerTestRequest("early-break", wsManagerUser("one")))
	if err != nil {
		t.Fatal(err)
	}
	first := backend.next(t)
	if err := first.abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("break before content returned no error")
	}
	_ = resp.Body.Close()
	resp, info, err := backend.call(t, m, "early-break", makeWSManagerTestRequest("early-break", wsManagerUser("two")))
	if err != nil {
		t.Fatal(err)
	}
	second := backend.next(t)
	if info.Mode != "full" || second.connection == first.connection || wsManagerPrevious(second) != "" {
		t.Fatalf("call after pre-content break: info=%+v conn=%d/%d previous=%q", info, first.connection, second.connection, wsManagerPrevious(second))
	}
	if err := second.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
}

// TestWSManagerFailedIncompleteAndErrorTerminalsKeepTheConnection pins that a
// reply which did not complete keeps the connection but not the chain, so the
// next request goes out in full on the same connection.
func TestWSManagerFailedIncompleteAndErrorTerminalsKeepTheConnection(t *testing.T) {
	for _, terminal := range []string{"response.failed", "response.incomplete", "error"} {
		t.Run(terminal, func(t *testing.T) {
			m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
			backend := newWSManagerTestBackend(t)
			resp, _, err := backend.call(t, m, "terminal", makeWSManagerTestRequest("terminal", wsManagerUser("one")))
			if err != nil {
				t.Fatal(err)
			}
			first := backend.next(t)
			if err := first.terminal(terminal); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("read terminal: %v", err)
			}
			_ = resp.Body.Close()
			resp, info, err := backend.call(t, m, "terminal", makeWSManagerTestRequest("terminal", wsManagerUser("two")))
			if err != nil {
				t.Fatal(err)
			}
			second := backend.next(t)
			if info.Mode != "full" || !info.Reused || second.connection != first.connection || wsManagerPrevious(second) != "" {
				t.Fatalf("after %s: info=%+v conn=%d/%d previous=%q", terminal, info, first.connection, second.connection, wsManagerPrevious(second))
			}
			if stats := m.TakeStats(); stats.Dials != 1 || stats.Fallbacks != 1 {
				t.Fatalf("after %s stats = %+v, want one dial and one fallback", terminal, stats)
			}
			if err := second.complete(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}`); err != nil {
				t.Fatal(err)
			}
			wsManagerRead(t, resp)
			assistant := codex.MessageItem{Type: "message", Role: "assistant", Content: []codex.ContentPart{{Type: "output_text", Text: "recovered"}}}
			resp, info, err = backend.call(t, m, "terminal", makeWSManagerTestRequest("terminal", wsManagerUser("two"), assistant, wsManagerUser("three")))
			if err != nil {
				t.Fatal(err)
			}
			third := backend.next(t)
			if info.Mode != "delta" || third.connection != second.connection || wsManagerPrevious(third) != second.responseID {
				t.Fatalf("third call after %s: info=%+v conn=%d/%d previous=%q, want delta on %q", terminal, info, second.connection, third.connection, wsManagerPrevious(third), second.responseID)
			}
			if err := third.complete(); err != nil {
				t.Fatal(err)
			}
			wsManagerRead(t, resp)
		})
	}
}

func TestWSManagerEarlyBodyCloseDropsEvenBufferedCompleted(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	defer m.Close()
	backend := newWSManagerTestBackend(t)
	resp, _, err := backend.call(t, m, "early-close", makeWSManagerTestRequest("early-close", wsManagerUser("one")))
	if err != nil {
		t.Fatal(err)
	}
	first := backend.next(t)
	if err := first.complete(`{"type":"message","role":"assistant","content":[]}`); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	firstConn := m.testEntry("early-close").conn
	m.mu.Unlock()
	wsManagerWait(t, func() bool { return firstConn.Result().Completed }, "completed backend response")
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	resp, info, err := backend.call(t, m, "early-close", makeWSManagerTestRequest("early-close", wsManagerUser("two")))
	if err != nil {
		t.Fatal(err)
	}
	second := backend.next(t)
	if info.Reused || second.connection == first.connection || wsManagerPrevious(second) != "" {
		t.Fatalf("early close retained completed buffered response: info=%+v conn=%d/%d previous=%q", info, first.connection, second.connection, wsManagerPrevious(second))
	}
	if err := second.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
}

func TestWSManagerStatsAndAtomicConcurrentReset(t *testing.T) {
	const calls = 8
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: calls, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	type callResult struct {
		response *http.Response
		Err      error
	}
	results := make([]callResult, calls)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].response, _, results[i].Err = backend.call(t, m, fmt.Sprintf("stat-%d", i), makeWSManagerTestRequest(fmt.Sprintf("stat-%d", i), wsManagerUser("x")))
		}(i)
	}
	close(start)
	wg.Wait()
	for i, result := range results {
		if result.Err != nil {
			t.Fatalf("call %d: %v", i, result.Err)
		}
	}
	requests := make([]*wsManagerTestRequest, calls)
	for i := range calls {
		requests[i] = backend.next(t)
	}
	for _, request := range requests {
		if err := request.complete(); err != nil {
			t.Fatal(err)
		}
	}
	for _, result := range results {
		wsManagerRead(t, result.response)
	}

	var got []WSStats
	var gotMu sync.Mutex
	var reset sync.WaitGroup
	reset.Add(calls)
	for range calls {
		go func() {
			defer reset.Done()
			stats := m.TakeStats()
			gotMu.Lock()
			got = append(got, stats)
			gotMu.Unlock()
		}()
	}
	reset.Wait()
	sum := WSStats{}
	for _, stats := range got {
		sum.Full += stats.Full
		sum.Delta += stats.Delta
		sum.Busy += stats.Busy
		sum.Fallbacks += stats.Fallbacks
		sum.Dials += stats.Dials
		sum.SentBytes += stats.SentBytes
	}
	if sum.Full != calls || sum.Dials != calls || sum.Delta != 0 || sum.Busy != 0 || sum.Fallbacks != 0 || sum.SentBytes <= 0 {
		t.Fatalf("concurrent reset totals = %+v", sum)
	}
	if empty := m.TakeStats(); empty != (WSStats{}) {
		t.Fatalf("stats after atomic reset = %+v", empty)
	}
}

func countWSManagerJanitors() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "codex.(*WSManager).janitor(")
}

func TestWSManagerJanitorReclaimsCanceledUnreadBody(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: 5 * time.Millisecond, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	staleResp, _, err := m.Call(ctx, backend.server.URL, testSession(t), "canceled-unread", makeWSManagerTestRequest("canceled-unread", wsManagerUser("one")))
	if err != nil {
		t.Fatal(err)
	}
	staleRequest := backend.next(t)
	m.mu.Lock()
	staleEntry := m.testEntry("canceled-unread")
	staleConn := staleEntry.conn
	m.mu.Unlock()
	cancel()
	wsManagerWait(t, func() bool { return staleConn.Err() != nil }, "canceled exchange connection failure")
	wsManagerWait(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.testEntry("canceled-unread") == nil
	}, "janitor removal of busy dead connection")

	freshResp, info, err := backend.call(t, m, "canceled-unread", makeWSManagerTestRequest("canceled-unread", wsManagerUser("two")))
	if err != nil {
		t.Fatalf("call after janitor reclaimed dead entry: %v", err)
	}
	freshRequest := backend.next(t)
	if info.Mode != "full" || info.Reused || freshRequest.connection == staleRequest.connection {
		t.Fatalf("replacement info=%+v old/new connections=%d/%d", info, staleRequest.connection, freshRequest.connection)
	}
	m.mu.Lock()
	freshEntry := m.testEntry("canceled-unread")
	freshGeneration := freshEntry.generation
	m.mu.Unlock()
	if err := staleResp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	stillFresh := m.testEntry("canceled-unread") == freshEntry && freshEntry.conn != staleConn && freshEntry.busy && freshEntry.generation == freshGeneration
	m.mu.Unlock()
	if !stillFresh {
		t.Fatal("closing stale body corrupted the active replacement lease")
	}
	if err := freshRequest.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, freshResp)
}

func TestWSManagerJanitorIdleDeadAndShutdown(t *testing.T) {
	m, clock := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 8, maxAge: time.Hour, maxIdle: time.Minute, janitorEvery: 5 * time.Millisecond, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	if m.janitorOn || m.janitorDone != nil {
		t.Fatal("janitor started before first manager call")
	}
	for i := 0; i < 3; i++ {
		bucket := fmt.Sprintf("janitor-%d", i)
		resp, _, err := backend.call(t, m, bucket, makeWSManagerTestRequest(bucket, wsManagerUser(bucket)))
		if err != nil {
			t.Fatal(err)
		}
		request := backend.next(t)
		if err := request.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, resp)
		if i == 0 {
			wsManagerWait(t, func() bool { return countWSManagerJanitors() >= 1 }, "janitor goroutine")
			m.mu.Lock()
			initialDone := m.janitorDone
			m.mu.Unlock()
			if initialDone == nil {
				t.Fatal("janitorDone was not set")
			}
			for j := 0; j < 2; j++ {
				bucket2 := fmt.Sprintf("janitor-extra-%d", j)
				resp2, _, err := backend.call(t, m, bucket2, makeWSManagerTestRequest(bucket2, wsManagerUser(bucket2)))
				if err != nil {
					t.Fatal(err)
				}
				req2 := backend.next(t)
				if err := req2.complete(); err != nil {
					t.Fatal(err)
				}
				wsManagerRead(t, resp2)
			}
			m.mu.Lock()
			sameDone := m.janitorDone == initialDone && m.janitorOn
			m.mu.Unlock()
			if !sameDone {
				t.Fatal("manager started more than one janitor")
			}
			if n := countWSManagerJanitors(); n != 1 {
				t.Fatalf("live manager janitors = %d, want 1", n)
			}
		}
	}
	m.mu.Lock()
	idleConn := m.testEntry("janitor-0").conn
	deadConn := m.testEntry("janitor-1").conn
	m.mu.Unlock()
	if err := deadConn.Close(); err != nil {
		t.Fatal(err)
	}
	wsManagerWait(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		exists := m.testEntry("janitor-1") != nil
		return !exists
	}, "janitor dead-connection eviction")
	m.mu.Lock()
	healthyRemains := m.testEntry("janitor-0") != nil
	m.mu.Unlock()
	if !healthyRemains {
		t.Fatal("janitor removed healthy entry before it became idle")
	}
	clock.Add(2 * time.Minute)
	wsManagerWait(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.entries) == 0
	}, "janitor idle eviction")
	if idleConn.Err() == nil {
		wsManagerWait(t, func() bool { return idleConn.Err() != nil }, "expired idle connection close")
	}
	m.Close()
	select {
	case <-m.janitorDone:
	case <-time.After(3 * time.Second):
		t.Fatal("manager janitor did not stop")
	}
	wsManagerWait(t, func() bool { return countWSManagerJanitors() == 0 }, "janitor goroutine exit")
}

func TestWSManagerZeroValueAndConcurrentIdempotentClose(t *testing.T) {
	backend := newWSManagerTestBackend(t)
	var m WSManager
	defer m.Close()
	resp, _, err := backend.call(t, &m, "zero", makeWSManagerTestRequest("zero", wsManagerUser("zero")))
	if err != nil {
		t.Fatal(err)
	}
	request := backend.next(t)
	if err := request.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); m.Close() }()
	}
	wg.Wait()
	m.Close()
	if _, _, err := backend.call(t, &m, "closed", makeWSManagerTestRequest("closed", wsManagerUser("closed"))); !errors.Is(err, ErrWSUnavailable) {
		t.Fatalf("call after concurrent Close = %v, want ErrWSUnavailable", err)
	}
}

func TestWSManagerCloseDuringPendingDial(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	result := make(chan error, 1)
	go func() {
		_, _, err := m.Call(context.Background(), server.URL, testSession(t), "pending", makeWSManagerTestRequest("pending", wsManagerUser("pending")))
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("pending handshake did not start")
	}
	m.Close()
	select {
	case err := <-result:
		if !errors.Is(err, ErrWSUnavailable) && !errors.Is(err, context.Canceled) {
			t.Fatalf("call during Close error = %v, want ErrWSUnavailable or context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock pending dial")
	}
}

func TestWSManagerCloseUnblocksRead(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	resp, _, err := backend.call(t, m, "blocked-read", makeWSManagerTestRequest("blocked-read", wsManagerUser("hold")))
	if err != nil {
		t.Fatal(err)
	}
	_ = backend.next(t)
	readStarted := make(chan struct{})
	readResult := make(chan error, 1)
	go func() {
		close(readStarted)
		_, err := resp.Body.Read(make([]byte, 1))
		readResult <- err
	}()
	<-readStarted
	m.Close()
	select {
	case err := <-readResult:
		if err == nil {
			t.Fatal("blocked Read succeeded after manager Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("manager Close did not unblock Read")
	}
	_ = resp.Body.Close()
}

func TestWSManagerStaleCloseAfterEOFDoesNotCorruptNextLease(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	base := wsManagerUser("one")
	resp1, _, err := backend.call(t, m, "stale", makeWSManagerTestRequest("stale", base))
	if err != nil {
		t.Fatal(err)
	}
	first := backend.next(t)
	if err := first.complete(`{"type":"message","role":"assistant","content":[]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp1.Body); err != nil {
		t.Fatal(err)
	}
	resp2, info, err := backend.call(t, m, "stale", makeWSManagerTestRequest("stale", base, codex.MessageItem{Type: "message", Role: "assistant"}, wsManagerUser("two")))
	if err != nil {
		t.Fatal(err)
	}
	second := backend.next(t)
	if info.Mode != "delta" || second.connection != first.connection {
		t.Fatalf("second lease info=%+v connections=%d/%d", info, first.connection, second.connection)
	}
	if err := resp1.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.complete(`{"type":"message","role":"assistant","content":[]}`); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp2)
	resp3, info, err := backend.call(t, m, "stale", makeWSManagerTestRequest("stale", base,
		codex.MessageItem{Type: "message", Role: "assistant"}, wsManagerUser("two"),
		codex.MessageItem{Type: "message", Role: "assistant"}, wsManagerUser("three")))
	if err != nil {
		t.Fatal(err)
	}
	third := backend.next(t)
	if info.Mode != "delta" || third.connection != first.connection || wsManagerPrevious(third) != second.responseID {
		t.Fatalf("old Close corrupted next chain: info=%+v conn=%d/%d previous=%q", info, third.connection, first.connection, wsManagerPrevious(third))
	}
	if err := third.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp3)
}

type wsManagerCountingItem struct {
	calls chan struct{}
}

func (item wsManagerCountingItem) MarshalJSON() ([]byte, error) {
	select {
	case item.calls <- struct{}{}:
	default:
	}
	return []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"counted"}]}`), nil
}

func TestWSManagerDeltaAvoidsFullRequestSerialization(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	marshalCalls := make(chan struct{}, 16)
	counted := wsManagerCountingItem{calls: marshalCalls}
	resp, _, err := backend.call(t, m, "counted", makeWSManagerTestRequest("counted", counted))
	if err != nil {
		t.Fatal(err)
	}
	first := backend.next(t)
	if err := first.complete(`{"type":"message","role":"assistant","content":[]}`); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)

	resp, info, err := backend.call(t, m, "counted", makeWSManagerTestRequest("counted", counted,
		codex.MessageItem{Type: "message", Role: "assistant"}, wsManagerUser("next")))
	if err != nil {
		t.Fatal(err)
	}
	second := backend.next(t)
	if info.Mode != "delta" || wsManagerPrevious(second) != first.responseID {
		t.Fatalf("expected chained delta, info=%+v previous=%q", info, wsManagerPrevious(second))
	}
	var count int
	for {
		select {
		case <-marshalCalls:
			count++
		default:
			if count != 3 {
				t.Fatalf("counted item MarshalJSON calls = %d, want 3 (digest of the first request + full initial frame + digest of the second request)", count)
			}
			goto checked
		}
	}
checked:
	if err := second.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
}

// TestWSManagerDigestComputedOncePerRequest pins the marshal count of a full
// request and its delta follow-up: one digest per request serves the delta
// decision and the chain kept after the reply.
func TestWSManagerDigestComputedOncePerRequest(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	marshalCalls := make(chan struct{}, 16)
	counted := wsManagerCountingItem{calls: marshalCalls}
	resp, info, err := backend.call(t, m, "digest", makeWSManagerTestRequest("digest-key", counted))
	if err != nil {
		t.Fatal(err)
	}
	first := backend.next(t)
	if info.Mode != "full" || info.Reused {
		t.Fatalf("first info = %+v, want a full request on a new connection", info)
	}
	if err := first.complete(`{"type":"message","role":"assistant","content":[]}`); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)

	resp, info, err = backend.call(t, m, "digest", makeWSManagerTestRequest("digest-key", counted,
		codex.MessageItem{Type: "message", Role: "assistant"}, wsManagerUser("next")))
	if err != nil {
		t.Fatal(err)
	}
	second := backend.next(t)
	if info.Mode != "delta" || !info.Reused || wsManagerPrevious(second) != first.responseID {
		t.Fatalf("expected chained delta, info=%+v previous=%q", info, wsManagerPrevious(second))
	}
	if err := second.complete(`{"type":"message","role":"assistant","content":[]}`); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)

	var count int
drain:
	for {
		select {
		case <-marshalCalls:
			count++
		default:
			break drain
		}
	}
	if count != 3 {
		t.Fatalf("counted item MarshalJSON calls = %d, want 3 (digest of the first request + full initial frame + digest of the second request); more means the history was hashed again for the delta or the chain", count)
	}
}

func TestWSManagerZeroValueUsesDefaultCapacity(t *testing.T) {
	var m WSManager
	defer m.Close()
	backend := newWSManagerTestBackend(t)
	for i := 0; i < wsManagerMaxConnections+1; i++ {
		bucket := fmt.Sprintf("zero-capacity-%d", i)
		resp, _, err := backend.call(t, &m, bucket, makeWSManagerTestRequest(bucket, wsManagerUser(bucket)))
		if err != nil {
			t.Fatal(err)
		}
		request := backend.next(t)
		if err := request.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, resp)
	}
	m.mu.Lock()
	count := len(m.entries)
	m.mu.Unlock()
	if count != 16 {
		t.Fatalf("zero-value manager entry count = %d, want default 16", count)
	}
	m.Close()
}

func testSessionAccount(t *testing.T, account string) *oauth.Session {
	t.Helper()
	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok-" + account, RefreshToken: "rt", AccountID: account, LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return &oauth.Session{Store: store, Client: &oauth.Client{}}
}

// wsManagerRoundTrip sends one request and completes it; it returns the backend connection id.
func wsManagerRoundTrip(t *testing.T, m *WSManager, backend *wsManagerTestBackend, s *oauth.Session, bucket, text string) (int, WSInfo) {
	t.Helper()
	resp, info, err := m.Call(context.Background(), backend.server.URL, s, bucket, makeWSManagerTestRequest(bucket, wsManagerUser(text)))
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	request := backend.next(t)
	if err := request.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
	return request.connection, info
}

func TestWSManagerConnectionsBelongToTheirAccount(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	alice, bob := testSessionAccount(t, "alice"), testSessionAccount(t, "bob")
	first, _ := wsManagerRoundTrip(t, m, backend, alice, "same", "alice one")
	second, info := wsManagerRoundTrip(t, m, backend, bob, "same", "bob one")
	if second == first || info.Reused {
		t.Fatalf("bob got alice's connection (conn %d, info %+v)", second, info)
	}
	again, info := wsManagerRoundTrip(t, m, backend, alice, "same", "alice two")
	if again != first || !info.Reused {
		t.Fatalf("alice's connection was not kept for her (conn %d, want %d, info %+v)", again, first, info)
	}
}

func TestWSManagerLogoutClosesConnections(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	s := testSessionAccount(t, "alice")
	wsManagerRoundTrip(t, m, backend, s, "conv", "before logout")
	m.mu.Lock()
	conn := m.testEntry("conv").conn
	m.mu.Unlock()
	if err := os.Remove(s.Store.Path); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.Call(context.Background(), backend.server.URL, s, "conv", makeWSManagerTestRequest("conv", wsManagerUser("after logout")))
	if !errors.Is(err, ErrWSToken) || !errors.Is(err, ErrWSUnavailable) {
		t.Fatalf("call after logout = %v, want ErrWSUnavailable wrapping ErrWSToken", err)
	}
	m.mu.Lock()
	left := m.testEntry("conv")
	m.mu.Unlock()
	if left != nil {
		t.Fatal("the logged-out session's connection is still kept")
	}
	if conn.Err() == nil {
		t.Fatal("the logged-out session's connection is still open")
	}
	if got := wsDisabledUntil(t, m); !got.IsZero() {
		t.Fatalf("a missing login opened a no-websocket window until %s", got)
	}
}

func TestWSManagerAccountSwitchClosesOldConnection(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	s := testSessionAccount(t, "alice")
	first, _ := wsManagerRoundTrip(t, m, backend, s, "conv", "alice")
	m.mu.Lock()
	old := m.testEntry("conv").conn
	m.mu.Unlock()
	if err := s.Store.Save(&oauth.Tokens{AccessToken: "tok-bob", RefreshToken: "rt", AccountID: "bob", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	second, info := wsManagerRoundTrip(t, m, backend, s, "conv", "bob")
	if second == first || info.Reused {
		t.Fatalf("the new account reused the old account's connection (conn %d, info %+v)", second, info)
	}
	if old.Err() == nil {
		t.Fatal("the old account's connection is still open")
	}
}
