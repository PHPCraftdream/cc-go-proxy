package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func wsSideAssistant(text string) messageItem {
	return messageItem{Type: "message", Role: "assistant", Content: []contentPart{{Type: "output_text", Text: text}}}
}

func wsSideOutput(text string) string {
	return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`
}

// wsSideExchange sends req on bucket "conv" and answers it with one assistant message.
func wsSideExchange(t *testing.T, m *WSManager, backend *wsManagerTestBackend, req *Request, answer string) WSInfo {
	t.Helper()
	resp, info, err := backend.call(t, m, "conv", req)
	if err != nil {
		t.Fatal(err)
	}
	request := backend.next(t)
	if err := request.complete(wsSideOutput(answer)); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
	return info
}

func newWSSideManager(t *testing.T) (*WSManager, *wsManagerTestBackend, *[]WSSide) {
	t.Helper()
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	learned := new([]WSSide)
	m.OnSide = func(s WSSide) { *learned = append(*learned, s) }
	return m, newWSManagerTestBackend(t), learned
}

func TestWSManagerLearnsABackgroundRequestFromTheDivergenceItCauses(t *testing.T) {
	m, backend, learned := newWSSideManager(t)
	main, side := wsManagerUser("main question"), wsManagerUser("[background template]")

	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main), "a1")
	// The background request continues the chain and becomes the latest response.
	if info := wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), side), "s1"); info.Mode != "delta" {
		t.Fatalf("background request went %q, want delta", info.Mode)
	}
	if len(*learned) != 0 || m.SideRequest(makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), side)) {
		t.Fatal("learned a side message before any divergence")
	}
	// The conversation goes on without that message and its reply: the chain is lost.
	info := wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsManagerUser("real next")), "a2")
	if info.Mode != "full" || info.Divergence == nil || info.Divergence.ReplyAt != -1 {
		t.Fatalf("real request info = %+v", info)
	}
	if len(*learned) != 1 || (*learned)[0].Size <= 0 {
		t.Fatalf("learned = %+v, want the side message once", *learned)
	}

	if !m.SideRequest(makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsSideAssistant("a2"), wsManagerUser("more"), side)) {
		t.Error("a request ending in the learned message was not recognised, whatever its history")
	}
	if m.SideRequest(makeWSManagerTestRequest("k", main, wsManagerUser("real next"))) {
		t.Error("a request with another last message was taken for a side one")
	}
	if m.SideRequest(makeWSManagerTestRequest("", side)) || m.SideRequest(makeWSManagerTestRequest("k")) || m.SideRequest(nil) {
		t.Error("a request without a cache key, without items or nil was taken for a side one")
	}
	if got := m.TakeStats().Side; got != 1 {
		t.Errorf("Side = %d, want one counted request (the unrecognised ones are not)", got)
	}

	// Learning the same message again is silent.
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsManagerUser("real next"), wsSideAssistant("a2"), side), "s2")
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsManagerUser("real next"), wsSideAssistant("a2"), wsManagerUser("third")), "a3")
	if len(*learned) != 1 {
		t.Errorf("learned %d times, want once", len(*learned))
	}
}

func TestWSManagerDoesNotLearnAnEditedMessageOrALargeOne(t *testing.T) {
	m, backend, learned := newWSSideManager(t)
	main := wsManagerUser("main question")

	// The client edits the last message it had sent and keeps the reply: not a side request.
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsManagerUser("tail")), "r")
	info := wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsManagerUser("tail edited"), wsSideAssistant("r"), wsManagerUser("again")), "r2")
	if info.Divergence == nil || info.Divergence.ReplyAt < 0 {
		t.Fatalf("edited request info = %+v, want a divergence with the reply in the request", info)
	}
	if len(*learned) != 0 {
		t.Fatalf("learned an edited message: %+v", *learned)
	}

	// A user's long text that was replaced is not a template.
	large := wsManagerUser(strings.Repeat("x", wsSideMaxSize+1))
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k2", main, large), "l1")
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k2", main, wsManagerUser("other")), "l2")
	if len(*learned) != 0 || m.SideRequest(makeWSManagerTestRequest("k2", main, large)) {
		t.Fatalf("learned a message over %d bytes: %+v", wsSideMaxSize, *learned)
	}
}

func TestLastItemHashIsTheHashItemHashesGives(t *testing.T) {
	req := makeWSManagerTestRequest("k", wsManagerUser("one"), wsSideAssistant("two"), wsManagerUser("three"))
	got, ok := lastItemHash(req)
	if want := ItemHashes(req); !ok || got != want[len(want)-1] {
		t.Errorf("lastItemHash = %d, %v; want %d", got, ok, want[len(want)-1])
	}
	if _, ok := lastItemHash(makeWSManagerTestRequest("")); ok {
		t.Error("a request without a cache key has no hash")
	}
}

func TestWSManagerRemembersAtMostWSSideMaxMessages(t *testing.T) {
	m, _, _ := newWSSideManager(t)
	m.mu.Lock()
	m.side = map[uint64]struct{}{}
	for i := uint64(0); i < wsSideMax; i++ {
		m.side[i] = struct{}{}
	}
	m.mu.Unlock()
	chain := NewChain(makeWSManagerTestRequest("k", wsManagerUser("m"), wsManagerUser("template")), "id", []json.RawMessage{json.RawMessage(wsSideOutput("r"))})
	d := &WSDivergence{At: 1, Had: 2, Reply: true, ReplyAt: -1, Was: ItemShape{Kind: "user message", Size: 50}}
	m.learnSide(chain, d)
	m.mu.Lock()
	n := len(m.side)
	m.mu.Unlock()
	if n != 1 {
		t.Errorf("remembered %d messages after the cap, want the table restarted with 1", n)
	}
}
