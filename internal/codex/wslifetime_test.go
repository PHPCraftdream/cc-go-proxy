package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/coder/websocket"
)

var lifetimeT0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func TestWSReplaceAfter(t *testing.T) {
	for limit, want := range map[time.Duration]time.Duration{
		30 * time.Minute:  27 * time.Minute,  // a tenth: 3 minutes ahead
		60 * time.Minute:  55 * time.Minute,  // capped at 5 minutes
		200 * time.Minute: 195 * time.Minute, // capped at 5 minutes
		6 * time.Minute:   5 * time.Minute,   // at least a minute
	} {
		if got := wsReplaceAfter(limit); got != want {
			t.Errorf("wsReplaceAfter(%s) = %s, want %s", limit, got, want)
		}
	}
}

func TestWSLifetimeLearnsOnlyFromAgreeingBreaks(t *testing.T) {
	var l wsLifetime
	// One break says nothing: it can be the network.
	if limit, _, changed := l.observe(30*time.Minute+6*time.Second, lifetimeT0); limit != 0 || changed {
		t.Fatalf("one break learned a limit: %s %v", limit, changed)
	}
	// Two breaks 6 seconds apart agree; the limit is the earlier one.
	limit, samples, changed := l.observe(30*time.Minute+1*time.Second, lifetimeT0.Add(time.Hour))
	if limit != 30*time.Minute+time.Second || samples != 2 || !changed {
		t.Fatalf("two agreeing breaks: limit %s samples %d changed %v", limit, samples, changed)
	}
	// The same limit again is not news.
	if limit, _, changed := l.observe(30*time.Minute+3*time.Second, lifetimeT0.Add(2*time.Hour)); limit != 30*time.Minute+time.Second || changed {
		t.Fatalf("the same limit again: %s %v", limit, changed)
	}
}

func TestWSLifetimeIgnoresEarlyAndScatteredBreaks(t *testing.T) {
	var l wsLifetime
	for _, age := range []time.Duration{20 * time.Second, 3 * time.Minute, 4*time.Minute + 59*time.Second} {
		if limit, samples, changed := l.observe(age, lifetimeT0); limit != 0 || samples != 0 || changed {
			t.Fatalf("a break at %s (young) counted: %s %d %v", age, limit, samples, changed)
		}
	}
	if len(l.ages) != 0 {
		t.Fatalf("young breaks kept as samples: %v", l.ages)
	}
	// Breaks at unrelated ages do not form a limit.
	for _, age := range []time.Duration{8 * time.Minute, 17 * time.Minute, 41 * time.Minute} {
		if limit, _, changed := l.observe(age, lifetimeT0); limit != 0 || changed {
			t.Fatalf("scattered breaks learned %s", limit)
		}
	}
}

func TestWSLifetimeAgreementAmongNoise(t *testing.T) {
	var l wsLifetime
	for _, age := range []time.Duration{30*time.Minute + 2*time.Second, 9 * time.Minute, 30*time.Minute + 5*time.Second} {
		l.observe(age, lifetimeT0)
	}
	if got := l.current(lifetimeT0); got != 30*time.Minute+2*time.Second {
		t.Fatalf("limit among noise = %s", got)
	}
}

func TestWSLifetimeSampleWindowIsBounded(t *testing.T) {
	var l wsLifetime
	for i := 0; i < 20; i++ {
		l.observe(time.Duration(6+i*3)*time.Minute, lifetimeT0) // never agree
	}
	if len(l.ages) != wsLearnSamples {
		t.Fatalf("kept %d samples, want %d", len(l.ages), wsLearnSamples)
	}
}

func TestWSLifetimeExpiresAndOneBreakBringsItBack(t *testing.T) {
	var l wsLifetime
	l.observe(30*time.Minute, lifetimeT0)
	l.observe(30*time.Minute+time.Second, lifetimeT0)
	if l.current(lifetimeT0.Add(wsLearnExpire-time.Minute)) == 0 {
		t.Fatal("the limit expired too early")
	}
	later := lifetimeT0.Add(wsLearnExpire + time.Minute)
	if got := l.current(later); got != 0 {
		t.Fatalf("the limit did not expire: %s", got)
	}
	limit, _, changed := l.observe(30*time.Minute+2*time.Second, later)
	if limit != 30*time.Minute || !changed {
		t.Fatalf("one more break after expiry: %s %v", limit, changed)
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
	conn := dialWSTest(t, context.Background(), server.URL, testSession(t), &Request{Model: "m"}, nil)
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
	mine := dialWSTest(t, context.Background(), quiet.URL, testSession(t), &Request{Model: "m"}, nil)
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

func TestWSManagerReplacesConnectionsAheadOfTheLearnedLimit(t *testing.T) {
	m, clock := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: 55 * time.Minute, maxIdle: 2 * time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	var learned []WSLimit
	m.OnLimit = func(l WSLimit) { learned = append(learned, l) }
	owner := ownerOf(backend.server)

	m.observeLost(owner, 30*time.Minute+4*time.Second, 1001)
	if len(learned) != 0 {
		t.Fatalf("one break reported a limit: %+v", learned)
	}
	m.observeLost(owner, 30*time.Minute+1*time.Second, 1001)
	want := WSLimit{Limit: 30*time.Minute + time.Second, ReplaceAfter: 27*time.Minute + time.Second, Samples: 2, Status: 1001}
	if len(learned) != 1 || learned[0] != want {
		t.Fatalf("learned %+v, want [%+v]", learned, want)
	}

	first, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "one")
	if info.Reason != "new connection" {
		t.Fatalf("first info %+v", info)
	}
	clock.Add(27 * time.Minute) // not yet 27m01s
	same, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "two")
	if same != first || !info.Reused {
		t.Fatalf("replaced too early: conn %d (was %d), info %+v", same, first, info)
	}
	clock.Add(2 * time.Second)
	next, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "three")
	if next == first || info.Reused || info.Reason != "ahead of backend limit" || info.Limit != 30*time.Minute+time.Second || info.Mode != "full" {
		t.Fatalf("not replaced ahead of the limit: conn %d (was %d), info %+v", next, first, info)
	}
	if st := m.TakeStats(); st.Early != 1 || st.Lost != 2 {
		t.Fatalf("stats %+v, want 1 early, 2 lost", st)
	}
}

func TestWSManagerLearnedLimitIsPerOwner(t *testing.T) {
	m, clock := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: 55 * time.Minute, maxIdle: 2 * time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	other := wsOwnerKey(backend.server.URL, "account:someone-else", proxydial.ChoiceFor(context.Background()))
	m.observeLost(other, 30*time.Minute, -1)
	m.observeLost(other, 30*time.Minute, -1)
	first, _ := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "one")
	clock.Add(40 * time.Minute)
	again, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "two")
	if again != first || !info.Reused {
		t.Fatalf("another account's limit replaced this one's connection: %+v", info)
	}
}

func TestWSManagerDefaultAgeStillAppliesWithoutALearnedLimit(t *testing.T) {
	m, clock := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: 55 * time.Minute, maxIdle: 2 * time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
	backend := newWSManagerTestBackend(t)
	first, _ := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "one")
	clock.Add(56 * time.Minute)
	next, info := wsManagerRoundTrip(t, m, backend, testSession(t), "conv", "two")
	if next == first || info.Reason != "connection too old" || info.Limit != 0 {
		t.Fatalf("default age: conn %d (was %d), info %+v", next, first, info)
	}
}

func TestWSManagerCountsAndLearnsFromABackendBreak(t *testing.T) {
	m, _ := newWSManagerForTest(t, wsManagerTestConfig{maxConnections: 4, maxAge: time.Hour, maxIdle: time.Hour, janitorEvery: time.Hour, disabledFor: time.Minute})
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
	// A young break teaches nothing.
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.lifetimes {
		if l.limit != 0 || len(l.ages) != 0 {
			t.Fatalf("a connection that broke after milliseconds became a sample: %+v", l)
		}
	}
}
