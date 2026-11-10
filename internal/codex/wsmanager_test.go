package codex

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

	"github.com/coder/websocket"
)

type wsManagerTestClock struct{ n atomic.Int64 }

func newWSManagerTestClock(t time.Time) *wsManagerTestClock {
	c := &wsManagerTestClock{}
	c.Set(t)
	return c
}

func (c *wsManagerTestClock) Now() time.Time      { return time.Unix(0, c.n.Load()) }
func (c *wsManagerTestClock) Set(t time.Time)     { c.n.Store(t.UnixNano()) }
func (c *wsManagerTestClock) Add(d time.Duration) { c.n.Add(int64(d)) }

type wsManagerTestConfig struct {
	maxConnections int
	maxAge         time.Duration
	maxIdle        time.Duration
	janitorEvery   time.Duration
	disabledFor    time.Duration
	useDefaults    bool
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

func (b *wsManagerTestBackend) call(t *testing.T, m *WSManager, bucket string, req *Request) (*http.Response, WSInfo, error) {
	t.Helper()
	return m.Call(context.Background(), b.server.URL, testSession(t), bucket, req)
}

func makeWSManagerTestRequest(key string, input ...any) *Request {
	return &Request{
		Model:             "gpt-5",
		Stream:            true,
		Input:             input,
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Include:           []string{"reasoning.encrypted_content"},
		PromptCacheKey:    key,
	}
}

func wsManagerUser(text string) messageItem {
	return messageItem{Type: "message", Role: "user", Content: []contentPart{{Type: "input_text", Text: text}}}
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

	assistant := messageItem{Type: "message", Role: "assistant", Content: []contentPart{{Type: "output_text", Text: "answer"}}}
	second := makeWSManagerTestRequest("loop-key", baseInput, assistant, wsManagerUser("second"))
	resp, info, err = backend.call(t, m, "loop", second)
	if err != nil {
		t.Fatal(err)
	}
	secondWire := backend.next(t)
	if info.Mode != "delta" || !info.Reused || info.Reason != "" || info.SentBytes != len(secondWire.raw) {
		t.Fatalf("second info = %+v, bytes = %d", info, len(secondWire.raw))
	}
	fullSecond, err := WSCreatePayload(second, "", second.Input)
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

	functionCall := functionCallItem{Type: "function_call", CallID: "call-2", Name: "lookup", Arguments: "{}"}
	third := makeWSManagerTestRequest("loop-key", baseInput, assistant, wsManagerUser("second"), functionCall,
		functionOutputItem{Type: "function_call_output", CallID: "call-2", Output: "found"}, wsManagerUser("third"))
	resp, info, err = backend.call(t, m, "loop", third)
	if err != nil {
		t.Fatal(err)
	}
	thirdWire := backend.next(t)
	fullThird, err := WSCreatePayload(third, "", third.Input)
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
	firstConn := m.entries["drop"].conn
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
	if info.Mode != "full" || !info.Reused || info.Reason != "history diverged at item 0" || first.connection != second.connection {
		t.Fatalf("divergent fallback info=%+v connections %d/%d", info, first.connection, second.connection)
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
		respA, info, err := backend.call(t, m, "a", makeWSManagerTestRequest("a", wsManagerUser("a1"), messageItem{Type: "message", Role: "assistant"}, wsManagerUser("a2")))
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

	t.Run("default eight entries", func(t *testing.T) {
		m, clock := newWSManagerForTest(t, wsManagerTestConfig{useDefaults: true})
		backend := newWSManagerTestBackend(t)
		var first *wsManagerTestRequest
		for i := 0; i < 8; i++ {
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
		if entries != 8 {
			t.Fatalf("default entry count = %d, want 8", entries)
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

func TestWSManagerForbiddenDisablesWindowButTransientRetries(t *testing.T) {
	t.Run("403 disables for ten minutes", func(t *testing.T) {
		m, clock := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: 10 * time.Minute})
		var handshakes atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if handshakes.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			ws := wsAccept(t, w, r)
			if ws == nil {
				return
			}
			defer ws.Close(websocket.StatusNormalClosure, "")
			request := readWSRequest(t, ws)
			_ = request
			writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"after-window"}}`)
		}))
		defer server.Close()
		_, _, err := m.Call(context.Background(), server.URL, testSession(t), "forbidden", makeWSManagerTestRequest("forbidden", wsManagerUser("one")))
		if !errors.Is(err, ErrWSUnavailable) {
			t.Fatalf("403 error = %v, want ErrWSUnavailable", err)
		}
		if got := handshakes.Load(); got != 1 {
			t.Fatalf("handshakes after 403 = %d", got)
		}
		m.mu.Lock()
		disabledTil := m.disabledTil
		m.mu.Unlock()
		if want := clock.Now().Add(10 * time.Minute); !disabledTil.Equal(want) {
			t.Fatalf("disabled until %s, want %s", disabledTil, want)
		}
		if _, _, err := m.Call(context.Background(), server.URL, testSession(t), "forbidden", makeWSManagerTestRequest("forbidden", wsManagerUser("still disabled"))); !errors.Is(err, ErrWSUnavailable) {
			t.Fatalf("disabled-window call error = %v", err)
		}
		if got := handshakes.Load(); got != 1 {
			t.Fatalf("disabled-window call dialed; handshakes=%d", got)
		}
		clock.Add(10 * time.Minute)
		resp, _, err := m.Call(context.Background(), server.URL, testSession(t), "forbidden", makeWSManagerTestRequest("forbidden", wsManagerUser("retry")))
		if err != nil {
			t.Fatalf("call after disable window: %v", err)
		}
		if got := handshakes.Load(); got != 2 {
			t.Fatalf("handshakes after expiry = %d", got)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	})

	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: 10 * time.Minute})
			var handshakes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if handshakes.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				ws := wsAccept(t, w, r)
				if ws == nil {
					return
				}
				defer ws.Close(websocket.StatusNormalClosure, "")
				_ = readWSRequest(t, ws)
				writeWSEvent(t, ws, `{"type":"response.completed","response":{"id":"retry"}}`)
			}))
			defer server.Close()
			_, _, err := m.Call(context.Background(), server.URL, testSession(t), "retry", makeWSManagerTestRequest("retry", wsManagerUser("first")))
			var handshake *WSHandshakeError
			if !errors.As(err, &handshake) || handshake.Status != status {
				t.Fatalf("first error = %v, want handshake %d", err, status)
			}
			m.mu.Lock()
			disabled := m.disabledTil
			m.mu.Unlock()
			if !disabled.IsZero() {
				t.Fatalf("transient %d disabled websocket until %s", status, disabled)
			}
			resp, _, err := m.Call(context.Background(), server.URL, testSession(t), "retry", makeWSManagerTestRequest("retry", wsManagerUser("again")))
			if err != nil {
				t.Fatalf("transient %d was not retried: %v", status, err)
			}
			if got := handshakes.Load(); got != 2 {
				t.Fatalf("handshakes = %d, want 2", got)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		})
	}
}

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
	m.mu.Lock()
	disabled := m.disabledTil
	m.mu.Unlock()
	if !disabled.IsZero() {
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
	firstConn := m.entries["broken"].conn
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

func TestWSManagerFailedIncompleteAndErrorTerminalsDrop(t *testing.T) {
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
			if info.Mode != "full" || info.Reused || second.connection == first.connection || wsManagerPrevious(second) != "" {
				t.Fatalf("after %s: info=%+v conn=%d/%d previous=%q", terminal, info, first.connection, second.connection, wsManagerPrevious(second))
			}
			if err := second.complete(); err != nil {
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
	firstConn := m.entries["early-close"].conn
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
		err      error
	}
	results := make([]callResult, calls)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].response, _, results[i].err = backend.call(t, m, fmt.Sprintf("stat-%d", i), makeWSManagerTestRequest(fmt.Sprintf("stat-%d", i), wsManagerUser("x")))
		}(i)
	}
	close(start)
	wg.Wait()
	for i, result := range results {
		if result.err != nil {
			t.Fatalf("call %d: %v", i, result.err)
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
	staleEntry := m.entries["canceled-unread"]
	staleConn := staleEntry.conn
	m.mu.Unlock()
	cancel()
	wsManagerWait(t, func() bool { return staleConn.Err() != nil }, "canceled exchange connection failure")
	wsManagerWait(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.entries["canceled-unread"] == nil
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
	freshEntry := m.entries["canceled-unread"]
	freshGeneration := freshEntry.generation
	m.mu.Unlock()
	if err := staleResp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	stillFresh := m.entries["canceled-unread"] == freshEntry && freshEntry.conn != staleConn && freshEntry.busy && freshEntry.generation == freshGeneration
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
	idleConn := m.entries["janitor-0"].conn
	deadConn := m.entries["janitor-1"].conn
	m.mu.Unlock()
	if err := deadConn.Close(); err != nil {
		t.Fatal(err)
	}
	wsManagerWait(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, exists := m.entries["janitor-1"]
		return !exists
	}, "janitor dead-connection eviction")
	m.mu.Lock()
	_, healthyRemains := m.entries["janitor-0"]
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
	resp2, info, err := backend.call(t, m, "stale", makeWSManagerTestRequest("stale", base, messageItem{Type: "message", Role: "assistant"}, wsManagerUser("two")))
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
		messageItem{Type: "message", Role: "assistant"}, wsManagerUser("two"),
		messageItem{Type: "message", Role: "assistant"}, wsManagerUser("three")))
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
		messageItem{Type: "message", Role: "assistant"}, wsManagerUser("next")))
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
				t.Fatalf("counted item MarshalJSON calls = %d, want 3 (chain hash + full initial frame + delta hash)", count)
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
func TestWSManagerZeroValueUsesDefaultCapacity(t *testing.T) {
	var m WSManager
	defer m.Close()
	backend := newWSManagerTestBackend(t)
	for i := 0; i < 9; i++ {
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
	if count != 8 {
		t.Fatalf("zero-value manager entry count = %d, want default 8", count)
	}
	m.Close()
}
