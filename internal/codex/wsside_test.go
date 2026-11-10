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

func TestLastItemSideKeyUsesTheStartOfTheEncoding(t *testing.T) {
	long := strings.Repeat("a", 300)
	key := func(text string) (uint64, int, bool) {
		return lastItemSideKey(makeWSManagerTestRequest("k", wsManagerUser("x"), wsManagerUser(text)))
	}
	a, size, ok := key(long + "1")
	if !ok || size < 300 {
		t.Fatalf("key of a long message: %d bytes, ok %v", size, ok)
	}
	if b, _, _ := key(long + "2"); b != a {
		t.Error("messages that differ only after the start have different keys")
	}
	if c, _, _ := key("b" + long); c == a {
		t.Error("messages that differ at the start have the same key")
	}
	if short1, _, _ := key("short"); short1 == a {
		t.Error("a short message has the key of a long one")
	}
	if _, _, ok := lastItemSideKey(makeWSManagerTestRequest("")); ok {
		t.Error("a request without a cache key has no side key")
	}
	if _, _, ok := lastItemSideKey(nil); ok {
		t.Error("nil has no side key")
	}
}

// The background message of Claude Code is a template of about 700 bytes of
// which a part changes from request to request; learning the whole message
// would learn it again every time, and each time break the conversation's chain.
func TestWSManagerLearnsATemplateWhoseMiddleVaries(t *testing.T) {
	m, backend, learned := newWSSideManager(t)
	main := wsManagerUser("main question")
	template := func(variable string) messageItem {
		return wsManagerUser(strings.Repeat("T", 200) + variable + strings.Repeat("E", 200))
	}
	side1, side2 := template(" variable one "), template(" variable 2 ")

	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main), "a1")
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), side1), "s1")
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsManagerUser("real next")), "a2")
	if len(*learned) != 1 {
		t.Fatalf("learned %d times, want once", len(*learned))
	}
	// Another instance of the template is recognised without a divergence of its own.
	if !m.SideRequest(makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsManagerUser("real next"), wsSideAssistant("a2"), side2)) {
		t.Error("another instance of the template was not recognised")
	}
	if m.SideRequest(makeWSManagerTestRequest("k", main, wsManagerUser(strings.Repeat("U", 200)+" variable one "+strings.Repeat("E", 200)))) {
		t.Error("a message with another start was taken for the template")
	}
	// A message over the size bound is never a side request, whatever its start.
	huge := wsManagerUser(strings.Repeat("T", 200) + strings.Repeat("x", wsSideMaxSize))
	if m.SideRequest(makeWSManagerTestRequest("k", main, huge)) {
		t.Error("a message over the size bound was taken for a side request")
	}
}
