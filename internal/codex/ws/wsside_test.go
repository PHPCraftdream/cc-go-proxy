package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/catalog"
	wschain "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws/chain"
	wsconn "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws/conn"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/coder/websocket"
)

func wsSideAssistant(text string) codex.MessageItem {
	return codex.MessageItem{Type: "message", Role: "assistant", Content: []codex.ContentPart{{Type: "output_text", Text: text}}}
}

func wsSideOutput(text string) string {
	return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`
}

// wsSideExchange sends req on bucket "conv" and answers it with one assistant message.
func wsSideExchange(t *testing.T, m *WSManager, backend *wsManagerTestBackend, req *codex.Request, answer string) WSInfo {
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
	chain := wschain.NewChain(makeWSManagerTestRequest("k", wsManagerUser("m"), wsManagerUser("template")), "id", []json.RawMessage{json.RawMessage(wsSideOutput("r"))})
	d := &wschain.WSDivergence{At: 1, Had: 2, Reply: true, ReplyAt: -1, Was: wschain.ItemShape{Kind: "user message", Size: 50}}
	m.learnSide(chain, d)
	m.mu.Lock()
	n := len(m.side)
	m.mu.Unlock()
	if n != 1 {
		t.Errorf("remembered %d messages after the cap, want the table restarted with 1", n)
	}
}

func isSide(m *WSManager, req *codex.Request) bool {
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
	two := codex.MessageItem{Type: "message", Role: "user", Content: []codex.ContentPart{{Type: "input_text", Text: head + " middle "}, {Type: "input_text", Text: tail}}}
	if k, size, ok := side(two); !ok || k != key(head+" middle "+tail) || size <= 2*wsSideEnd {
		t.Errorf("a message in two parts: key %x ok %v size %d", k, ok, size)
	}
	for name, item := range map[string]any{
		"assistant message": wsSideAssistant("x"),
		"tool output":       codex.FunctionOutputItem{Type: "function_call_output", CallID: "c", Output: "x"},
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
func learnWSSide(t *testing.T, m *WSManager, backend *wsManagerTestBackend, main, side codex.MessageItem) {
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
	template := func(variable string) codex.MessageItem {
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

func TestAgentKeyFollowsTheFirstUserMessage(t *testing.T) {
	system := codex.MessageItem{Type: "message", Role: "developer", Content: []codex.ContentPart{{Type: "input_text", Text: "system"}}}
	task := wsManagerUser("task of agent A")
	grown := makeWSManagerTestRequest("k", system, task, wsSideAssistant("a1"), wsManagerUser("more"), codex.FunctionOutputItem{Type: "function_call_output", CallID: "c", Output: "x"})
	first := makeWSManagerTestRequest("k", system, task)

	key := wschain.AgentKey(first)
	if len(key) != 8 {
		t.Fatalf("AgentKey = %q, want 8 hex digits", key)
	}
	if wschain.AgentKey(grown) != key {
		t.Error("the key changed while the agent's history grew")
	}
	if wschain.AgentKey(makeWSManagerTestRequest("other-key", task)) != key {
		t.Error("the key depends on the cache key; the thread already carries that")
	}
	if wschain.AgentKey(makeWSManagerTestRequest("k", system, wsManagerUser("task of agent B"))) == key {
		t.Error("two agents with different tasks got one key")
	}
	if wschain.AgentKey(makeWSManagerTestRequest("k", system, wsSideAssistant("a1"), task)) != key {
		t.Error("an item before the first user message changed the key")
	}
}

func TestAgentKeyIsEmptyWithoutAUserMessage(t *testing.T) {
	if wschain.AgentKey(nil) != "" || wschain.AgentKey(makeWSManagerTestRequest("k")) != "" {
		t.Error("a nil or empty request has no agent key")
	}
	if got := wschain.AgentKey(makeWSManagerTestRequest("k", wsSideAssistant("a"), codex.FunctionCallItem{Type: "function_call", CallID: "c"})); got != "" {
		t.Errorf("AgentKey without a user message = %q", got)
	}
	// Items that came back from JSON (maps) are recognised too.
	if got := wschain.AgentKey(makeWSManagerTestRequest("k", map[string]any{"type": "message", "role": "user", "content": []any{}})); got == "" {
		t.Error("a user message given as a map was not recognised")
	}
}

func TestWSManagerReportsEveryBreakWithItsAge(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 2, maxAge: 25 * time.Minute, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	var got []WSLost
	m.OnLost = func(l WSLost) { got = append(got, l) }
	m.observeLost("owner", 12*time.Minute, 1001)
	m.observeLost("owner", 2*time.Minute, -1)
	m.observeLost("owner", 25*time.Minute, 1006)
	want := []WSLost{
		{Age: 12 * time.Minute, Status: 1001, Planned: 25 * time.Minute, Counts: true},
		{Age: 2 * time.Minute, Status: -1, Planned: 25 * time.Minute, Counts: false},
		{Age: 25 * time.Minute, Status: 1006, Planned: 25 * time.Minute, Counts: false},
	}
	if len(got) != len(want) {
		t.Fatalf("reports = %+v, want %d", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("report %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if m.TakeStats().Lost != 3 {
		t.Error("the breaks were not counted")
	}
}

var lifetimeT0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

const lifetimeDefault = 25 * time.Minute

func TestWSDefaultAgeIsTwentyMinutes(t *testing.T) {
	if wsManagerMaxAge != 20*time.Minute {
		t.Fatalf("default connection age = %s, want 20m", wsManagerMaxAge)
	}
}

func TestWSLifetimeLowersByFiveMinutesAfterTwoEarlyBreaks(t *testing.T) {
	var l wsLifetime
	now := lifetimeT0
	// One early break is not enough: it can be the network.
	if limit, lowered := l.observe(lifetimeDefault, 12*time.Minute, now); limit != lifetimeDefault || lowered {
		t.Fatalf("one early break: %s %v", limit, lowered)
	}
	// The second one lowers the age by one step.
	now = now.Add(time.Minute)
	if limit, lowered := l.observe(lifetimeDefault, 13*time.Minute, now); limit != 20*time.Minute || !lowered {
		t.Fatalf("two early breaks: %s %v, want 20m lowered", limit, lowered)
	}
	if got := l.current(lifetimeDefault, now); got != 20*time.Minute {
		t.Fatalf("current = %s", got)
	}
	// Each further pair of breaks younger than the new age lowers it again, to the floor.
	for _, want := range []time.Duration{15 * time.Minute, 10 * time.Minute, 5 * time.Minute} {
		now = now.Add(wsLowerBurst)
		l.observe(lifetimeDefault, 5*time.Minute+time.Second, now)
		now = now.Add(wsLowerBurst)
		limit, lowered := l.observe(lifetimeDefault, 5*time.Minute+time.Second, now)
		if limit != want || !lowered {
			t.Fatalf("step to %s: got %s %v", want, limit, lowered)
		}
	}
	// At the floor nothing is younger than the age: breaks change nothing.
	if limit, lowered := l.observe(lifetimeDefault, 5*time.Minute+time.Second, now); limit != 5*time.Minute || lowered {
		t.Fatalf("below the floor: %s %v", limit, lowered)
	}
}

func TestWSLifetimeIgnoresBreaksThatAreNotEarly(t *testing.T) {
	var l wsLifetime
	for _, age := range []time.Duration{
		20 * time.Second, 3 * time.Minute, 4*time.Minute + 59*time.Second, // too young to be an age limit
		25 * time.Minute, 28 * time.Minute, 30 * time.Minute, // not before the planned age
	} {
		for i := 0; i < 5; i++ {
			if limit, lowered := l.observe(lifetimeDefault, age, lifetimeT0); limit != lifetimeDefault || lowered {
				t.Fatalf("a break at %s lowered the age to %s", age, limit)
			}
		}
	}
}

func TestWSLifetimeEarlyBreaksFarApartDoNotAddUp(t *testing.T) {
	var l wsLifetime
	l.observe(lifetimeDefault, 12*time.Minute, lifetimeT0)
	if limit, lowered := l.observe(lifetimeDefault, 12*time.Minute, lifetimeT0.Add(wsLowerWindow+time.Minute)); limit != lifetimeDefault || lowered {
		t.Fatalf("breaks more than %s apart: %s %v", wsLowerWindow, limit, lowered)
	}
}

func TestWSLifetimeGoesBackToTheDefault(t *testing.T) {
	var l wsLifetime
	l.observe(lifetimeDefault, 12*time.Minute, lifetimeT0)
	l.observe(lifetimeDefault, 12*time.Minute, lifetimeT0.Add(wsLowerBurst))
	if got := l.current(lifetimeDefault, lifetimeT0.Add(wsLimitRestore-time.Minute)); got != 20*time.Minute {
		t.Fatalf("restored too early: %s", got)
	}
	if got := l.current(lifetimeDefault, lifetimeT0.Add(wsLimitRestore+2*time.Minute)); got != lifetimeDefault {
		t.Fatalf("not restored after %s without early breaks: %s", wsLimitRestore, got)
	}
	// An early break keeps a lowered age lowered.
	var k wsLifetime
	k.observe(lifetimeDefault, 12*time.Minute, lifetimeT0)
	k.observe(lifetimeDefault, 12*time.Minute, lifetimeT0.Add(wsLowerBurst))
	k.observe(lifetimeDefault, 12*time.Minute, lifetimeT0.Add(5*time.Hour))
	if got := k.current(lifetimeDefault, lifetimeT0.Add(8*time.Hour)); got != 20*time.Minute {
		t.Fatalf("a recent early break did not keep the age lowered: %s", got)
	}
}

func TestWSLifetimeNeverExceedsTheDefault(t *testing.T) {
	l := wsLifetime{limit: 40 * time.Minute, touched: lifetimeT0}
	if got := l.current(lifetimeDefault, lifetimeT0); got != lifetimeDefault {
		t.Fatalf("current = %s, want the default %s", got, lifetimeDefault)
	}
}

func TestWSConnReportsAPeerBreakOnceAndNotOurOwnClose(t *testing.T) {
	cut := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		<-cut
		_ = ws.Close(websocket.StatusGoingAway, "")
	}))
	defer server.Close()
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &codex.Request{Model: "m"}, nil)
	if _, _, ok := conn.TakePeerLost(); ok {
		t.Fatal("a live connection reported lost")
	}
	close(cut)
	wsManagerWait(t, func() bool { return conn.Err() != nil }, "peer close seen")
	age, status, ok := conn.TakePeerLost()
	if !ok || status != int(websocket.StatusGoingAway) || age < 0 {
		t.Fatalf("peer break: age %s status %d ok %v", age, status, ok)
	}
	if _, _, ok := conn.TakePeerLost(); ok {
		t.Fatal("the break was reported twice")
	}

	// Our own close is not the peer's.
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := wsAccept(t, w, r)
		if ws == nil {
			return
		}
		_, _, _ = ws.Read(r.Context())
	}))
	defer quiet.Close()
	mine := dialWSTest(t, context.Background(), quiet.URL, testSession(t), &codex.Request{Model: "m"}, nil)
	_ = mine.Close()
	time.Sleep(50 * time.Millisecond)
	if _, _, ok := mine.TakePeerLost(); ok {
		t.Fatal("our own Close was reported as the peer's")
	}
}

// ownerOf is the owner key of the test session on server.
func ownerOf(server *httptest.Server) string {
	return wsOwnerKey(server.URL, "account:acc", proxydial.ChoiceFor(context.Background()))
}

func lifetimeManager(t *testing.T) (*WSManager, *wsManagerTestClock) {
	return newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: lifetimeDefault, maxIdle: 2 * time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
}

func TestWSManagerReplacesAtTheDefaultAge(t *testing.T) {
	m, clock := lifetimeManager(t)
	backend := newWSManagerTestBackend(t)
	first, _ := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "one")
	clock.Add(lifetimeDefault)
	same, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "two")
	if same != first || !info.Reused {
		t.Fatalf("replaced at exactly the age, not after it: conn %d (was %d), info %+v", same, first, info)
	}
	clock.Add(time.Second)
	next, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "three")
	if next == first || info.Reused || info.Reason != "connection too old" || info.Limit != 0 || info.Mode != "full" {
		t.Fatalf("default age: conn %d (was %d), info %+v", next, first, info)
	}
	if st := m.TakeStats(); st.Early != 0 {
		t.Fatalf("a default-age replacement counted as lowered: %+v", st)
	}
}

func TestWSManagerLowersTheAgeAfterEarlyBreaks(t *testing.T) {
	m, clock := lifetimeManager(t)
	backend := newWSManagerTestBackend(t)
	var lowered []WSLimit
	m.OnLimit = func(l WSLimit) { lowered = append(lowered, l) }
	owner := ownerOf(backend.server)

	m.observeLost(owner, 12*time.Minute, 1001)
	if len(lowered) != 0 {
		t.Fatalf("one early break reported: %+v", lowered)
	}
	clock.Add(wsLowerBurst)
	m.observeLost(owner, 12*time.Minute+5*time.Second, 1001)
	want := WSLimit{Age: 20 * time.Minute, Was: 25 * time.Minute, Broke: 12*time.Minute + 5*time.Second, Status: 1001}
	if len(lowered) != 1 || lowered[0] != want {
		t.Fatalf("lowered %+v, want [%+v]", lowered, want)
	}

	first, _ := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "one")
	clock.Add(20 * time.Minute)
	same, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "two")
	if same != first || !info.Reused {
		t.Fatalf("replaced before the lowered age: conn %d (was %d), info %+v", same, first, info)
	}
	clock.Add(time.Second)
	next, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "three")
	if next == first || info.Reused || info.Reason != "lowered age limit" || info.Limit != 20*time.Minute || info.Mode != "full" {
		t.Fatalf("not replaced at the lowered age: conn %d (was %d), info %+v", next, first, info)
	}
	if st := m.TakeStats(); st.Early != 1 || st.Lost != 2 {
		t.Fatalf("stats %+v, want 1 early, 2 lost", st)
	}
}

func TestWSManagerLoweredAgeIsPerOwner(t *testing.T) {
	m, clock := lifetimeManager(t)
	backend := newWSManagerTestBackend(t)
	other := wsOwnerKey(backend.server.URL, "account:someone-else", proxydial.ChoiceFor(context.Background()))
	m.observeLost(other, 12*time.Minute, -1)
	clock.Add(wsLowerBurst)
	m.observeLost(other, 12*time.Minute, -1)
	first, _ := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "one")
	clock.Add(24 * time.Minute)
	again, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "two")
	if again != first || !info.Reused {
		t.Fatalf("another account's lowered age replaced this one's connection: %+v", info)
	}
}

func TestWSManagerCountsABackendBreak(t *testing.T) {
	m, _ := lifetimeManager(t)
	backend := newWSManagerTestBackend(t)
	resp, _, err := backend.call(t, m, "drop", makeWSManagerTestRequest("drop-key", wsManagerUser("one")))
	if err != nil {
		t.Fatal(err)
	}
	first := backend.next(t)
	if err := first.complete(); err != nil {
		t.Fatal(err)
	}
	wsManagerRead(t, resp)
	m.mu.Lock()
	conn := m.testEntry("drop").conn
	m.mu.Unlock()
	if err := first.abort(); err != nil {
		t.Fatal(err)
	}
	wsManagerWait(t, func() bool { return conn.Err() != nil }, "broken idle connection detection")
	resp, info, err := backend.call(t, m, "drop", makeWSManagerTestRequest("drop-key", wsManagerUser("two")))
	if err != nil {
		t.Fatal(err)
	}
	if info.Reason != "connection broken" {
		t.Fatalf("info %+v", info)
	}
	second := backend.next(t)
	_ = second.complete()
	wsManagerRead(t, resp)
	if st := m.TakeStats(); st.Lost != 1 {
		t.Fatalf("stats %+v, want one lost connection", st)
	}
	// A break after milliseconds says nothing about an age limit.
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.lifetimes {
		if l.limit != 0 || l.early != 0 {
			t.Fatalf("a connection that broke after milliseconds counted: %+v", l)
		}
	}
}

// A network failure ends every connection together: six breaks within seconds,
// at the same young age, are one event and must not lower the age.
func TestWSLifetimeABurstOfBreaksIsOneEvent(t *testing.T) {
	var l wsLifetime
	for i := 0; i < 6; i++ {
		now := lifetimeT0.Add(time.Duration(i) * 10 * time.Second)
		if limit, lowered := l.observe(lifetimeDefault, 5*time.Minute+30*time.Second, now); limit != lifetimeDefault || lowered {
			t.Fatalf("break %d of a burst: %s %v", i, limit, lowered)
		}
	}
	// The burst counted once: one more break, a minute after the first, is the second event.
	if limit, lowered := l.observe(lifetimeDefault, 6*time.Minute, lifetimeT0.Add(wsLowerBurst)); limit != 20*time.Minute || !lowered {
		t.Fatalf("second event: %s %v, want 20m lowered", limit, lowered)
	}
}

func TestWSManagerReportsABurstedBreakAsNotCounted(t *testing.T) {
	m, clock := lifetimeManager(t)
	var got []WSLost
	var limits []WSLimit
	m.OnLost = func(l WSLost) { got = append(got, l) }
	m.OnLimit = func(l WSLimit) { limits = append(limits, l) }
	for i := 0; i < 4; i++ {
		m.observeLost("owner", 5*time.Minute+35*time.Second, -1)
		clock.Add(time.Second)
	}
	if len(limits) != 0 {
		t.Fatalf("a burst lowered the age: %+v", limits)
	}
	if len(got) != 4 || !got[0].Counts || got[0].Burst {
		t.Fatalf("first break = %+v, want counted", got)
	}
	for i := 1; i < 4; i++ {
		if got[i].Counts || !got[i].Burst {
			t.Errorf("break %d = %+v, want a burst, not counted", i, got[i])
		}
	}
}

func testSession(t *testing.T) *oauth.Session {
	t.Helper()
	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return &oauth.Session{Store: store, Client: &oauth.Client{}}
}

func m(slug string, window int64, hidden bool) catalog.Model {
	return catalog.Model{Slug: slug, ContextWindow: window, Hidden: hidden}
}

func dialWSTest(t *testing.T, ctx context.Context, serverURL string, session *oauth.Session, request *codex.Request, routing *codex.Routing) *wsconn.WSConn {
	t.Helper()
	conn, err := wsconn.DialWS(ctx, serverURL, session, wsconn.WSHandshake{Request: request, Routing: routing})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func wsAccept(t *testing.T, w http.ResponseWriter, r *http.Request) *websocket.Conn {
	t.Helper()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Errorf("accept websocket: %v", err)
		return nil
	}
	return conn
}

func readWSRequest(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Errorf("read websocket request: %v", err)
		return nil
	}
	if typ != websocket.MessageText {
		t.Errorf("request frame type = %v, want text", typ)
	}
	return data
}

func writeWSEvent(t *testing.T, conn *websocket.Conn, event string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
		t.Errorf("write event: %v", err)
	}
}
