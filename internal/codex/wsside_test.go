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
	if len(*learned) != 0 || isSide(m, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), side)) {
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

	if !isSide(m, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsSideAssistant("a2"), wsManagerUser("more"), side)) {
		t.Error("a request ending in the learned message was not recognised, whatever its history")
	}
	if isSide(m, makeWSManagerTestRequest("k", main, wsManagerUser("real next"))) {
		t.Error("a request with another last message was taken for a side one")
	}
	if isSide(m, makeWSManagerTestRequest("", side)) || isSide(m, makeWSManagerTestRequest("k")) || isSide(m, nil) {
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
	if len(*learned) != 0 || isSide(m, makeWSManagerTestRequest("k2", main, large)) {
		t.Fatalf("learned a message over %d bytes: %+v", wsSideMaxSize, *learned)
	}
}

func TestWSManagerRemembersAtMostWSSideMaxMessages(t *testing.T) {
	m, _, _ := newWSSideManager(t)
	m.mu.Lock()
	m.side = map[uint64]*wsSideTemplate{}
	for i := uint64(0); i < wsSideMax; i++ {
		m.side[i] = &wsSideTemplate{}
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

func isSide(m *WSManager, req *Request) bool {
	side, _ := m.SideRequest("probe", req)
	return side
}

func TestLastItemSideUsesBothEndsOfTheText(t *testing.T) {
	head, tail := strings.Repeat("h", wsSideEnd), strings.Repeat("t", wsSideEnd)
	side := func(item any) (uint64, int, bool) {
		key, _, size, ok := lastItemSide(makeWSManagerTestRequest("k", wsManagerUser("x"), item))
		return key, size, ok
	}
	key := func(text string) uint64 {
		k, _, ok := side(wsManagerUser(text))
		if !ok {
			t.Fatalf("no key for a user message of %d bytes", len(text))
		}
		return k
	}
	a := key(head + " middle one " + tail)
	if key(head+" another, longer middle "+tail) != a {
		t.Error("messages that differ only in the middle have different keys")
	}
	if key("H"+head[1:]+" middle one "+tail) == a || key(head+" middle one "+tail[1:]+"T") == a {
		t.Error("messages that differ at an end have the same key")
	}
	// The same start, longer than the old 57-byte window, and another end: the
	// system reminder after a tool's result was taken for the template.
	reminder := "<system-reminder>\n" + strings.Repeat("r", 100)
	if key(reminder+" the template's end "+tail) == key(reminder+" an ordinary turn's end, quite another one") {
		t.Error("messages with another end have the same key")
	}
	if key("short") == key("short!") || key("short") == a {
		t.Error("a short message is not keyed by the whole of it")
	}
	// The key is the text's, not the encoding's: parts are joined.
	two := messageItem{Type: "message", Role: "user", Content: []contentPart{{Type: "input_text", Text: head + " middle "}, {Type: "input_text", Text: tail}}}
	if k, size, ok := side(two); !ok || k != key(head+" middle "+tail) || size <= 2*wsSideEnd {
		t.Errorf("a message in two parts: key %x ok %v size %d", k, ok, size)
	}
	for name, item := range map[string]any{
		"assistant message": wsSideAssistant("x"),
		"tool output":       functionOutputItem{Type: "function_call_output", CallID: "c", Output: "x"},
		"long message":      wsManagerUser(strings.Repeat("x", wsSideMaxSize)),
	} {
		if _, _, ok := side(item); ok {
			t.Errorf("%s has a side key", name)
		}
	}
	if _, _, _, ok := lastItemSide(makeWSManagerTestRequest("")); ok {
		t.Error("a request without a cache key has no side key")
	}
	if _, _, _, ok := lastItemSide(nil); ok {
		t.Error("nil has no side key")
	}
}

// learnWSSide makes m learn side after main and its reply a1, on bucket "conv".
func learnWSSide(t *testing.T, m *WSManager, backend *wsManagerTestBackend, main, side messageItem) {
	t.Helper()
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main), "a1")
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), side), "s1")
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsManagerUser("real next")), "a2")
}

// A message taken for a side one by mistake stays in the bucket's next request,
// followed by its reply: it was an ordinary turn, so it is forgotten for good.
func TestWSManagerForgetsASideMessageThatWasAnOrdinaryTurn(t *testing.T) {
	m, backend, learned := newWSSideManager(t)
	main, turn := wsManagerUser("main question"), wsManagerUser("<system-reminder>a turn's text</system-reminder>")
	learnWSSide(t, m, backend, main, turn)
	if len(*learned) != 1 {
		t.Fatalf("learned %d times, want once", len(*learned))
	}

	history := []any{main, wsSideAssistant("a1"), wsManagerUser("real next"), wsSideAssistant("a2")}
	sent := makeWSManagerTestRequest("k", append(history, turn)...)
	for i := 0; i < 2; i++ {
		// A repeat of the request ends in the message as well: no verdict yet.
		if side, wrong := m.SideRequest("conv", sent); !side || wrong != nil {
			t.Fatalf("request %d: side %v, wrong %+v; want a side request", i, side, wrong)
		}
	}
	// Another bucket's request says nothing about this one's.
	if _, wrong := m.SideRequest("other", makeWSManagerTestRequest("k", append(history, turn, wsSideAssistant("r"), wsManagerUser("go on"))...)); wrong != nil {
		t.Fatalf("another bucket's request found the message wrong: %+v", wrong)
	}
	next := makeWSManagerTestRequest("k", append(history, turn, wsSideAssistant("r"), wsManagerUser("go on"))...)
	side, wrong := m.SideRequest("conv", next)
	if side || wrong == nil || wrong.Matches != 2 || wrong.Size != (*learned)[0].Size {
		t.Fatalf("next request: side %v, wrong %+v; want the message found wrong after 2 matches", side, wrong)
	}
	if isSide(m, sent) {
		t.Error("the forgotten message is still taken for a side one")
	}
	if stats := m.TakeStats(); stats.SideWrong != 1 {
		t.Errorf("SideWrong = %d, want 1", stats.SideWrong)
	}
	// The same divergence later does not learn it again.
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", append(history, wsManagerUser("x"), wsSideAssistant("a3"), turn)...), "s2")
	wsSideExchange(t, m, backend, makeWSManagerTestRequest("k", append(history, wsManagerUser("x"), wsSideAssistant("a3"), wsManagerUser("y"))...), "a4")
	if len(*learned) != 1 {
		t.Errorf("learned %d times, want the message never learned again", len(*learned))
	}
}

// A real side request is left out of the next request: the message stays learned.
func TestWSManagerKeepsASideMessageLeftOutOfTheNextRequest(t *testing.T) {
	m, backend, _ := newWSSideManager(t)
	main, side := wsManagerUser("main question"), wsManagerUser("[background template]")
	learnWSSide(t, m, backend, main, side)

	history := []any{main, wsSideAssistant("a1"), wsManagerUser("real next"), wsSideAssistant("a2")}
	if s, _ := m.SideRequest("conv", makeWSManagerTestRequest("k", append(history, side)...)); !s {
		t.Fatal("the side request was not recognised")
	}
	next := makeWSManagerTestRequest("k", append(history, wsManagerUser("user's next"), wsSideAssistant("a3"), wsManagerUser("more"))...)
	if s, wrong := m.SideRequest("conv", next); s || wrong != nil {
		t.Fatalf("next request: side %v, wrong %+v", s, wrong)
	}
	if !isSide(m, makeWSManagerTestRequest("k", append(history, side)...)) {
		t.Error("the side message was forgotten")
	}
	if stats := m.TakeStats(); stats.SideWrong != 0 || stats.Side != 2 {
		t.Errorf("stats = %+v, want 2 side requests and none wrong", stats)
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
	if !isSide(m, makeWSManagerTestRequest("k", main, wsSideAssistant("a1"), wsManagerUser("real next"), wsSideAssistant("a2"), side2)) {
		t.Error("another instance of the template was not recognised")
	}
	if isSide(m, makeWSManagerTestRequest("k", main, wsManagerUser(strings.Repeat("U", 200)+" variable one "+strings.Repeat("E", 200)))) {
		t.Error("a message with another start was taken for the template")
	}
	// A message over the size bound is never a side request, whatever its start.
	huge := wsManagerUser(strings.Repeat("T", 200) + strings.Repeat("x", wsSideMaxSize))
	if isSide(m, makeWSManagerTestRequest("k", main, huge)) {
		t.Error("a message over the size bound was taken for a side request")
	}
}
