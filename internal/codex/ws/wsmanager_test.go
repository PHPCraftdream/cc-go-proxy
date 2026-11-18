package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	wschain "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws/chain"
	wsconn "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws/conn"
	"github.com/coder/websocket"
)

type wsManagerTestClock struct{ n atomic.Int64 }

func newWSManagerTestClock(t time.Time) *wsManagerTestClock {
	c := &wsManagerTestClock{}
	c.Set(t)
	return c
}

func (c *wsManagerTestClock) Now() time.Time { return time.Unix(0, c.n.Load()) }

func (c *wsManagerTestClock) Set(t time.Time) { c.n.Store(t.UnixNano()) }

func (c *wsManagerTestClock) Add(d time.Duration) { c.n.Add(int64(d)) }

type wsManagerTestConfig struct {
	maxConnections int
	maxAge         time.Duration
	maxIdle        time.Duration
	janitorEvery   time.Duration
	disabledFor    time.Duration
	useDefaults    bool
}

// testEntry finds the entry of a conversation bucket; the caller holds m.mu or
// knows no call runs.
func (m *WSManager) testEntry(bucket string) *wsManagerEntry {
	for _, entry := range m.entries {
		if entry.bucket == bucket {
			return entry
		}
	}
	return nil
}

func newWSManagerForTest(t *testing.T, cfg wsManagerTestConfig) (*WSManager, *wsManagerTestClock) {
	t.Helper()
	oldConnections, oldAge, oldIdle := wsManagerMaxConnections, wsManagerMaxAge, wsManagerMaxIdle
	oldJanitor, oldDisabled := wsManagerJanitorEvery, wsManagerDisabledFor
	if !cfg.useDefaults {
		wsManagerMaxConnections = cfg.maxConnections
		wsManagerMaxAge = cfg.maxAge
		wsManagerMaxIdle = cfg.maxIdle
		wsManagerJanitorEvery = cfg.janitorEvery
		wsManagerDisabledFor = cfg.disabledFor
	}
	m := NewWSManager()
	clock := newWSManagerTestClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	m.now = clock.Now
	t.Cleanup(func() {
		m.Close()
		if !cfg.useDefaults {
			wsManagerMaxConnections, wsManagerMaxAge, wsManagerMaxIdle = oldConnections, oldAge, oldIdle
			wsManagerJanitorEvery, wsManagerDisabledFor = oldJanitor, oldDisabled
		}
	})
	return m, clock
}

type wsManagerTestAction struct {
	events []string
	result chan error
}

type wsManagerTestRequest struct {
	connection  int
	index       int
	responseID  string
	raw         []byte
	payload     map[string]json.RawMessage
	action      chan wsManagerTestAction
	abortSignal chan struct{}
	abortConn   func() error
}

func (r *wsManagerTestRequest) respond(events ...string) error {
	result := make(chan error, 1)
	select {
	case r.action <- wsManagerTestAction{events: events, result: result}:
	case <-time.After(3 * time.Second):
		return errors.New("timed out delivering backend response")
	}
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		return errors.New("timed out waiting for backend response write")
	}
}

func (r *wsManagerTestRequest) complete(items ...string) error {
	events := make([]string, 0, len(items)+1)
	for _, item := range items {
		events = append(events, `{"type":"response.output_item.done","item":`+item+`}`)
	}
	events = append(events, `{"type":"response.completed","response":{"id":"`+r.responseID+`"}}`)
	return r.respond(events...)
}

func (r *wsManagerTestRequest) terminal(eventType string) error {
	var event string
	if eventType == "error" {
		event = `{"type":"error","message":"test failure"}`
	} else {
		event = `{"type":"` + eventType + `","response":{"id":"` + r.responseID + `"}}`
	}
	return r.respond(event)
}

func (r *wsManagerTestRequest) abort() error {
	if r.abortConn == nil {
		return errors.New("backend connection is not abortable")
	}
	return r.abortConn()
}

type wsManagerTestBackend struct {
	server   *httptest.Server
	requests chan *wsManagerTestRequest
	conns    atomic.Int32
}

func newWSManagerTestBackend(t *testing.T) *wsManagerTestBackend {
	t.Helper()
	b := &wsManagerTestBackend{requests: make(chan *wsManagerTestRequest, 64)}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connID := int(b.conns.Add(1))
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		defer ws.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var index int
		lastCompleted := ""
		for {
			typ, raw, err := ws.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				t.Errorf("backend got non-text request frame: %v", typ)
				continue
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Errorf("decode manager frame: %v", err)
				return
			}
			var kind string
			_ = json.Unmarshal(payload["type"], &kind)
			if kind != "response.create" {
				t.Errorf("request type = %q, want response.create; frame %s", kind, raw)
			}
			var stream bool
			_ = json.Unmarshal(payload["stream"], &stream)
			if !stream {
				t.Errorf("stream = false or omitted in frame %s", raw)
			}
			var previous string
			_ = json.Unmarshal(payload["previous_response_id"], &previous)
			if previous != "" && previous != lastCompleted {
				t.Errorf("previous_response_id %q not last completed id %q on connection %d", previous, lastCompleted, connID)
				if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"error","message":"invalid previous_response_id"}`)); err != nil {
					return
				}
				continue
			}
			index++
			abortSignal := make(chan struct{})
			var abortOnce sync.Once
			request := &wsManagerTestRequest{
				connection:  connID,
				index:       index,
				responseID:  fmt.Sprintf("response-%d-%d", connID, index),
				raw:         append([]byte(nil), raw...),
				payload:     payload,
				action:      make(chan wsManagerTestAction, 1),
				abortSignal: abortSignal,
				abortConn: func() error {
					var err error
					abortOnce.Do(func() {
						close(abortSignal)
						err = ws.CloseNow()
					})
					return err
				},
			}
			select {
			case b.requests <- request:
			case <-ctx.Done():
				return
			}
			select {
			case action := <-request.action:
				var writeErr error
				for _, event := range action.events {
					if writeErr = ws.Write(ctx, websocket.MessageText, []byte(event)); writeErr != nil {
						break
					}
					var terminal struct {
						Type     string `json:"type"`
						Response struct {
							ID string `json:"id"`
						} `json:"response"`
					}
					if json.Unmarshal([]byte(event), &terminal) == nil && terminal.Type == "response.completed" {
						lastCompleted = terminal.Response.ID
					}
				}
				action.result <- writeErr
				if writeErr != nil {
					return
				}
			case <-request.abortSignal:
				return
			case <-ctx.Done():
				return
			}
		}
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *wsManagerTestBackend) next(t *testing.T) *wsManagerTestRequest {
	t.Helper()
	select {
	case request := <-b.requests:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for websocket request")
		return nil
	}
}

func (b *wsManagerTestBackend) call(t *testing.T, m *WSManager, bucket string, req *codex.Request) (*http.Response, WSInfo, error) {
	t.Helper()
	return m.Call(context.Background(), b.server.URL, testSession(t), bucket, req)
}

func makeWSManagerTestRequest(key string, input ...any) *codex.Request {
	return &codex.Request{
		Model:             "gpt-5",
		Stream:            true,
		Input:             input,
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Include:           []string{"reasoning.encrypted_content"},
		PromptCacheKey:    key,
	}
}

func wsManagerUser(text string) codex.MessageItem {
	return codex.MessageItem{Type: "message", Role: "user", Content: []codex.ContentPart{{Type: "input_text", Text: text}}}
}

func wsManagerRead(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp == nil || resp.Body == nil {
		t.Fatal("nil response body")
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
}

func wsManagerInput(t *testing.T, request *wsManagerTestRequest) []json.RawMessage {
	t.Helper()
	var input []json.RawMessage
	if err := json.Unmarshal(request.payload["input"], &input); err != nil {
		t.Fatalf("decode input in frame %s: %v", request.raw, err)
	}
	return input
}

func wsManagerPrevious(request *wsManagerTestRequest) string {
	var previous string
	_ = json.Unmarshal(request.payload["previous_response_id"], &previous)
	return previous
}

func wsManagerWait(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

func TestWSManagerFullDeltaToolLoop(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 8, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: 10 * time.Minute})
	backend := newWSManagerTestBackend(t)
	baseInput := wsManagerUser(strings.Repeat("base history ", 256))
	first := makeWSManagerTestRequest("loop-key", baseInput)
	resp, info, err := backend.call(t, m, "loop", first)
	if err != nil {
		t.Fatal(err)
	}
	firstWire := backend.next(t)
	if info.Mode != "full" || info.Reused || info.Reason != "new connection" || info.SentBytes != len(firstWire.raw) {
		t.Fatalf("first info = %+v, frame bytes = %d", info, len(firstWire.raw))
	}
	if got := wsManagerInput(t, firstWire); len(got) != 1 || !strings.Contains(string(got[0]), `"base history`) || wsManagerPrevious(firstWire) != "" {
		t.Fatalf("first full frame input/previous = %s / %q", firstWire.payload["input"], wsManagerPrevious(firstWire))
	}
	if err := firstWire.complete(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)

	assistant := codex.MessageItem{Type: "message", Role: "assistant", Content: []codex.ContentPart{{Type: "output_text", Text: "answer"}}}
	second := makeWSManagerTestRequest("loop-key", baseInput, assistant, wsManagerUser("second"))
	resp, info, err = backend.call(t, m, "loop", second)
	if err != nil {
		t.Fatal(err)
	}
	secondWire := backend.next(t)
	if info.Mode != "delta" || !info.Reused || info.Reason != "" || info.SentBytes != len(secondWire.raw) {
		t.Fatalf("second info = %+v, bytes = %d", info, len(secondWire.raw))
	}
	fullSecond, err := wschain.WSCreatePayload(second, "", second.Input)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondWire.raw) >= len(fullSecond) {
		t.Fatalf("second delta frame is not smaller than full frame: delta=%d full=%d", len(secondWire.raw), len(fullSecond))
	}
	if got := wsManagerInput(t, secondWire); len(got) != 1 || !strings.Contains(string(got[0]), `"second"`) || wsManagerPrevious(secondWire) != firstWire.responseID {
		t.Fatalf("assistant delta frame input/previous = %s / %q", secondWire.payload["input"], wsManagerPrevious(secondWire))
	}
	if err := secondWire.complete(`{"type":"function_call","call_id":"call-2","name":"lookup","arguments":"{}"}`); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)

	functionCall := codex.FunctionCallItem{Type: "function_call", CallID: "call-2", Name: "lookup", Arguments: "{}"}
	third := makeWSManagerTestRequest("loop-key", baseInput, assistant, wsManagerUser("second"), functionCall,
		codex.FunctionOutputItem{Type: "function_call_output", CallID: "call-2", Output: "found"}, wsManagerUser("third"))
	resp, info, err = backend.call(t, m, "loop", third)
	if err != nil {
		t.Fatal(err)
	}
	thirdWire := backend.next(t)
	fullThird, err := wschain.WSCreatePayload(third, "", third.Input)
	if err != nil {
		t.Fatal(err)
	}
	if len(thirdWire.raw) >= len(fullThird) {
		t.Fatalf("tool-loop delta frame is not smaller than full frame: delta=%d full=%d", len(thirdWire.raw), len(fullThird))
	}
	gotInput := wsManagerInput(t, thirdWire)
	if info.Mode != "delta" || !info.Reused || info.SentBytes != len(thirdWire.raw) {
		t.Fatalf("tool-loop info = %+v", info)
	}
	if len(gotInput) != 2 || !strings.Contains(string(gotInput[0]), `"function_call_output"`) || !strings.Contains(string(gotInput[1]), `"third"`) {
		t.Fatalf("tool-loop delta serialized full history: %s", thirdWire.payload["input"])
	}
	if wsManagerPrevious(thirdWire) != secondWire.responseID {
		t.Fatalf("tool-loop previous_response_id = %q, want %q", wsManagerPrevious(thirdWire), secondWire.responseID)
	}
	if info.SentBytes != len(thirdWire.raw) {
		t.Fatalf("tool-loop SentBytes = %d, frame = %d", info.SentBytes, len(thirdWire.raw))
	}
	if err := thirdWire.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
	stats := m.TakeStats()
	if stats.Full != 1 || stats.Delta != 2 || stats.Dials != 1 || stats.Fallbacks != 0 || stats.SentBytes != int64(info.SentBytes+len(secondWire.raw)+len(firstWire.raw)) {
		t.Fatalf("loop stats = %+v", stats)
	}
}

func TestWSManagerDroppedConnectionUsesFull(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	resp, _, err := backend.call(t, m, "drop", makeWSManagerTestRequest("drop-key", wsManagerUser("one")))
	if err != nil {
		t.Fatal(err)
	}
	first := backend.next(t)
	if err := first.complete(`{"type":"message","role":"assistant","content":[]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	firstConn := m.testEntry("drop").conn
	m.mu.Unlock()
	if err := first.abort(); err != nil {
		t.Fatal(err)
	}
	wsManagerWait(t, func() bool { return firstConn.Err() != nil }, "broken idle connection detection")

	resp2, info, err := backend.call(t, m, "drop", makeWSManagerTestRequest("drop-key", wsManagerUser("two")))
	if err != nil {
		t.Fatal(err)
	}
	second := backend.next(t)
	if info.Mode != "full" || info.Reused || info.Reason != "connection broken" || second.connection == first.connection || wsManagerPrevious(second) != "" {
		t.Fatalf("replacement info=%+v connection=%d previous=%q (old=%d)", info, second.connection, wsManagerPrevious(second), first.connection)
	}
	if got := len(wsManagerInput(t, second)); got != 1 {
		t.Fatalf("fresh connection sent %d input items, want full one", got)
	}
	if err := second.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp2)
}

func TestWSManagerDivergentHistoryFallsBackOnSameConnection(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	firstReq := makeWSManagerTestRequest("branch-key", wsManagerUser("original"))
	resp, _, err := backend.call(t, m, "branch", firstReq)
	if err != nil {
		t.Fatal(err)
	}
	first := backend.next(t)
	if err := first.complete(`{"type":"message","role":"assistant","content":[]}`); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)

	diverged := makeWSManagerTestRequest("branch-key", wsManagerUser("other branch"), wsManagerUser("new"))
	resp, info, err := backend.call(t, m, "branch", diverged)
	if err != nil {
		t.Fatal(err)
	}
	second := backend.next(t)
	if info.Mode != "full" || !info.Reused || info.Reason != "history diverged at item 0 of 1" || first.connection != second.connection {
		t.Fatalf("divergent fallback info=%+v connections %d/%d", info, first.connection, second.connection)
	}
	if d := info.Divergence; d == nil || d.At != 0 || d.Had != 1 || d.Has != 2 || d.Was.Kind != "user message" || d.Now.Kind != "user message" ||
		d.SameHead < 0 || !d.Reply || d.ReplyAt != -1 || d.Since < 0 {
		t.Fatalf("divergence = %+v", info.Divergence)
	}
	if previous := wsManagerPrevious(second); previous != "" {
		t.Fatalf("full fallback used previous_response_id %q", previous)
	}
	if got := len(wsManagerInput(t, second)); got != 2 {
		t.Fatalf("full fallback sent %d items, want 2", got)
	}
	if err := second.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
	stats := m.TakeStats()
	if stats.Fallbacks != 1 || stats.Full != 2 || stats.Dials != 1 {
		t.Fatalf("fallback stats = %+v", stats)
	}
}

func TestWSManagerBusyAndIndependentBuckets(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 3, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	respA, _, err := backend.call(t, m, "same", makeWSManagerTestRequest("same-key", wsManagerUser("held")))
	if err != nil {
		t.Fatal(err)
	}
	requestA := backend.next(t)
	if _, _, err := backend.call(t, m, "same", makeWSManagerTestRequest("same-key", wsManagerUser("must reject"))); !errors.Is(err, ErrWSBusy) {
		t.Fatalf("same-bucket concurrent call error = %v, want ErrWSBusy", err)
	}
	respB, _, err := backend.call(t, m, "other", makeWSManagerTestRequest("other-key", wsManagerUser("parallel")))
	if err != nil {
		t.Fatalf("independent bucket was blocked: %v", err)
	}
	requestB := backend.next(t)
	if requestA.connection == requestB.connection {
		t.Fatalf("independent buckets shared connection %d", requestA.connection)
	}
	if err := requestB.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, respB)
	if err := requestA.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, respA)
	if stats := m.TakeStats(); stats.Busy != 1 || stats.Dials != 2 {
		t.Fatalf("busy stats = %+v", stats)
	}
}

func TestWSManagerMaxAgeAndIdleReplacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		idle time.Duration
	}{
		{name: "max age", age: time.Minute, idle: time.Hour},
		{name: "max idle", age: time.Hour, idle: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, clock := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: tc.age, maxIdle: tc.idle, janitorEvery: time.Hour, disabledFor: time.Minute})
			backend := newWSManagerTestBackend(t)
			resp, _, err := backend.call(t, m, "aged", makeWSManagerTestRequest("age-key", wsManagerUser("one")))
			if err != nil {
				t.Fatal(err)
			}
			first := backend.next(t)
			if err := first.complete(); err != nil {
				t.Fatal(err)
			}
			wsManagerRead(t, resp)
			clock.Add(2 * time.Minute)
			resp, info, err := backend.call(t, m, "aged", makeWSManagerTestRequest("age-key", wsManagerUser("two")))
			if err != nil {
				t.Fatal(err)
			}
			second := backend.next(t)
			if info.Reused || info.Mode != "full" || info.Reason != "connection too old" || second.connection == first.connection || wsManagerPrevious(second) != "" {
				t.Fatalf("replacement info=%+v old/new connections=%d/%d", info, first.connection, second.connection)
			}
			if err := second.complete(); err != nil {
				t.Fatal(err)
			}
			wsManagerRead(t, resp)
		})
	}
}

func TestWSManagerLRUEvictionAndAllBusy(t *testing.T) {
	t.Run("least recently used", func(t *testing.T) {
		m, clock := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
		backend := newWSManagerTestBackend(t)
		respA, _, err := backend.call(t, m, "a", makeWSManagerTestRequest("a", wsManagerUser("a1")))
		if err != nil {
			t.Fatal(err)
		}
		a1 := backend.next(t)
		if err := a1.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, respA)
		clock.Add(time.Minute)
		respB, _, err := backend.call(t, m, "b", makeWSManagerTestRequest("b", wsManagerUser("b1")))
		if err != nil {
			t.Fatal(err)
		}
		b1 := backend.next(t)
		if err := b1.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, respB)
		clock.Add(time.Minute)
		respA, info, err := backend.call(t, m, "a", makeWSManagerTestRequest("a", wsManagerUser("a1"), codex.MessageItem{Type: "message", Role: "assistant"}, wsManagerUser("a2")))
		if err != nil {
			t.Fatal(err)
		}
		a2 := backend.next(t)
		if !info.Reused {
			t.Fatalf("A not reused before eviction: %+v", info)
		}
		if err := a2.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, respA)
		clock.Add(time.Minute)
		respC, _, err := backend.call(t, m, "c", makeWSManagerTestRequest("c", wsManagerUser("c1")))
		if err != nil {
			t.Fatal(err)
		}
		c1 := backend.next(t)
		if err := c1.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, respC)
		respB, info, err = backend.call(t, m, "b", makeWSManagerTestRequest("b", wsManagerUser("b2")))
		if err != nil {
			t.Fatal(err)
		}
		b2 := backend.next(t)
		if info.Reused || b2.connection == b1.connection || wsManagerPrevious(b2) != "" {
			t.Fatalf("least-used B was not evicted: info=%+v connections=%d/%d", info, b1.connection, b2.connection)
		}
		if err := b2.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, respB)
	})

	t.Run("default entries", func(t *testing.T) {
		m, clock := newWSManagerForTest(t, wsManagerTestConfig{useDefaults: true})
		backend := newWSManagerTestBackend(t)
		var first *wsManagerTestRequest
		for i := 0; i < wsManagerMaxConnections; i++ {
			clock.Add(time.Second)
			bucket := fmt.Sprintf("default-%d", i)
			resp, _, err := backend.call(t, m, bucket, makeWSManagerTestRequest(bucket, wsManagerUser(bucket)))
			if err != nil {
				t.Fatal(err)
			}
			request := backend.next(t)
			if i == 0 {
				first = request
			}
			if err := request.complete(); err != nil {
				t.Fatal(err)
			}
			wsManagerRead(t, resp)
		}
		m.mu.Lock()
		entries := len(m.entries)
		m.mu.Unlock()
		if entries != 16 {
			t.Fatalf("default entry count = %d, want 16", entries)
		}
		clock.Add(time.Second)
		resp, _, err := backend.call(t, m, "default-new", makeWSManagerTestRequest("default-new", wsManagerUser("new")))
		if err != nil {
			t.Fatal(err)
		}
		newReq := backend.next(t)
		if err := newReq.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, resp)
		clock.Add(time.Second)
		resp, info, err := backend.call(t, m, "default-0", makeWSManagerTestRequest("default-0", wsManagerUser("again")))
		if err != nil {
			t.Fatal(err)
		}
		replacement := backend.next(t)
		if info.Reused || replacement.connection == first.connection {
			t.Fatalf("default capacity did not evict oldest entry: %+v, conn %d -> %d", info, first.connection, replacement.connection)
		}
		if err := replacement.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, resp)
	})

	t.Run("all entries busy", func(t *testing.T) {
		m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
		backend := newWSManagerTestBackend(t)
		resp, _, err := backend.call(t, m, "held-1", makeWSManagerTestRequest("held-1", wsManagerUser("held-1")))
		if err != nil {
			t.Fatal(err)
		}
		request := backend.next(t)
		resp2, _, err := backend.call(t, m, "held-2", makeWSManagerTestRequest("held-2", wsManagerUser("held-2")))
		if err != nil {
			t.Fatal(err)
		}
		request2 := backend.next(t)
		if _, _, err := backend.call(t, m, "new", makeWSManagerTestRequest("new", wsManagerUser("new"))); !errors.Is(err, ErrWSBusy) {
			t.Fatalf("capacity exhaustion error = %v, want ErrWSBusy", err)
		}
		if err := request.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, resp)
		if err := request2.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, resp2)
		resp, _, err = backend.call(t, m, "new", makeWSManagerTestRequest("new", wsManagerUser("new")))
		if err != nil {
			t.Fatalf("call after lease released: %v", err)
		}
		request = backend.next(t)
		if err := request.complete(); err != nil {
			t.Fatal(err)
		}
		wsManagerRead(t, resp)
	})
}

// wsDisabledUntil is the no-websocket window of the only owner a test used.
func wsDisabledUntil(t *testing.T, m *WSManager) time.Time {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.disabled) > 1 {
		t.Fatalf("%d owners disabled, want at most 1", len(m.disabled))
	}
	for _, until := range m.disabled {
		return until
	}
	return time.Time{}
}

func TestWSManagerDialFailureWindows(t *testing.T) {
	cases := []struct {
		status int
		pause  time.Duration // 0: no window, the next request dials again
	}{
		{http.StatusNotFound, 10 * time.Minute},
		{http.StatusMethodNotAllowed, 10 * time.Minute},
		{http.StatusUpgradeRequired, 10 * time.Minute},
		{http.StatusNotImplemented, 10 * time.Minute},
		{http.StatusForbidden, 30 * time.Second},
		{http.StatusTooManyRequests, 0},
		{http.StatusServiceUnavailable, 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			m, clock := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: 10 * time.Minute})
			var handshakes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if handshakes.Add(1) == 1 {
					w.WriteHeader(tc.status)
					return
				}
				ws := wsAccept(t, w, r)
				if ws == nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "")
				_ = readWSRequest(t, ws)
				writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"after"}}`)
			}))
			defer server.Close()
			_, _, err := m.Call(context.Background(), server.URL, testSession(t), "b", makeWSManagerTestRequest("b", wsManagerUser("one")))
			var handshake *wsconn.WSHandshakeError
			if !errors.As(err, &handshake) || handshake.Status != tc.status {
				t.Fatalf("first error = %v, want handshake %d", err, tc.status)
			}
			until := wsDisabledUntil(t, m)
			if tc.pause == 0 {
				if !until.IsZero() {
					t.Fatalf("%d opened a no-websocket window until %s", tc.status, until)
				}
			} else {
				if !errors.Is(err, ErrWSUnavailable) {
					t.Fatalf("error = %v, want ErrWSUnavailable", err)
				}
				if want := clock.Now().Add(tc.pause); !until.Equal(want) {
					t.Fatalf("disabled until %s, want %s", until, want)
				}
				if _, _, err := m.Call(context.Background(), server.URL, testSession(t), "b", makeWSManagerTestRequest("b", wsManagerUser("in window"))); !errors.Is(err, ErrWSUnavailable) {
					t.Fatalf("call in the window = %v", err)
				}
				if got := handshakes.Load(); got != 1 {
					t.Fatalf("call in the window dialed; handshakes=%d", got)
				}
				clock.Add(tc.pause)
			}
			resp, _, err := m.Call(context.Background(), server.URL, testSession(t), "b", makeWSManagerTestRequest("b", wsManagerUser("again")))
			if err != nil {
				t.Fatalf("call after the window: %v", err)
			}
			if got := handshakes.Load(); got != 2 {
				t.Fatalf("handshakes = %d, want 2", got)
			}
			wsManagerRead(t, resp)
		})
	}
}

func TestWSManagerNetworkFailureBacksOffBriefly(t *testing.T) {
	m, clock := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: 10 * time.Minute})
	server := httptest.NewServer(http.NotFoundHandler())
	dead := server.URL
	server.Close()
	_, _, err := m.Call(context.Background(), dead, testSession(t), "net", makeWSManagerTestRequest("net", wsManagerUser("one")))
	if !errors.Is(err, ErrWSUnavailable) {
		t.Fatalf("error = %v, want ErrWSUnavailable", err)
	}
	if until, want := wsDisabledUntil(t, m), clock.Now().Add(30*time.Second); !until.Equal(want) {
		t.Fatalf("network failure window until %s, want %s (short backoff, not the unsupported window)", until, want)
	}
}

func TestWSManagerWindowIsPerOwner(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: 10 * time.Minute})
	refusing := httptest.NewServer(http.NotFoundHandler())
	defer refusing.Close()
	if _, _, err := m.Call(context.Background(), refusing.URL, testSession(t), "b", makeWSManagerTestRequest("b", wsManagerUser("one"))); !errors.Is(err, ErrWSUnavailable) {
		t.Fatalf("refusing endpoint error = %v", err)
	}
	backend := newWSManagerTestBackend(t)
	resp, _, err := backend.call(t, m, "b", makeWSManagerTestRequest("b", wsManagerUser("other endpoint")))
	if err != nil {
		t.Fatalf("another endpoint is not disabled by the first one's refusal: %v", err)
	}
	request := backend.next(t)
	if err := request.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
}
