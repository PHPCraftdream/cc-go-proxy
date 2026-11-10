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
		l.observe(lifetimeDefault, 5*time.Minute+time.Second, now)
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
	l.observe(lifetimeDefault, 12*time.Minute, lifetimeT0)
	if got := l.current(lifetimeDefault, lifetimeT0.Add(wsLimitRestore-time.Minute)); got != 20*time.Minute {
		t.Fatalf("restored too early: %s", got)
	}
	if got := l.current(lifetimeDefault, lifetimeT0.Add(wsLimitRestore+time.Minute)); got != lifetimeDefault {
		t.Fatalf("not restored after %s without early breaks: %s", wsLimitRestore, got)
	}
	// An early break keeps a lowered age lowered.
	var k wsLifetime
	k.observe(lifetimeDefault, 12*time.Minute, lifetimeT0)
	k.observe(lifetimeDefault, 12*time.Minute, lifetimeT0)
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
