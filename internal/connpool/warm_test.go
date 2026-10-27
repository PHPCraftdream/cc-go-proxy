package connpool

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// The old pool still serves its running requests when the lane switches to the
// warm one; its connection closes only after the last of them.
func TestWarmSwitchKeepsTheRunningRequestsOfTheOldPool(t *testing.T) {
	srv, _ := tlsServer(t)
	base := srv.Client().Transport.(*http.Transport)
	tr := New(time.Minute, 1, 3, base.Clone)
	c := withClock(tr)
	tr.spawn = func(f func()) { f() }
	client := &http.Client{Transport: tr}

	get(t, client, srv.URL) // learns the target
	old := tr.lanes[0].cur
	held := tr.acquire(nil) // a long request on the old pool
	if held != old {
		t.Fatal("test setup: the held request is not on the old pool")
	}
	c.t = c.t.Add(time.Minute)
	get(t, client, srv.URL) // due: warm-up, switch

	if tr.lanes[0].cur == old {
		t.Fatal("the lane did not switch")
	}
	if len(tr.draining) != 1 || tr.draining[0] != old || old.inflight != 1 {
		t.Fatalf("old pool: draining %d, inflight %d; want it draining with the held request", len(tr.draining), old.inflight)
	}
	tr.release(held)
	if len(tr.draining) != 0 {
		t.Fatalf("old pool kept after its last request: %v", tr.draining)
	}
}

// Among lanes with the same load the one holding a fresh warm connection goes
// first, so the connection is used while it is young.
func TestAcquirePrefersTheLaneWithAFreshWarmConnection(t *testing.T) {
	tr := New(time.Hour, 3, 6, plain)
	tr.release(tr.acquire(nil)) // pools exist
	tr.release(tr.acquire(nil))
	tr.release(tr.acquire(nil))

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	tr.mu.Lock()
	want := tr.lanes[2].cur
	want.warm = &warmSlot{addr: "h:443", conn: a, opened: time.Now()}
	tr.mu.Unlock()

	for i := 0; i < 6; i++ {
		p := tr.acquire(nil)
		if p != want {
			t.Fatalf("acquire %d went to another lane although one holds a warm connection", i)
		}
		tr.release(p)
	}

	// A busy lane still loses to an idle one: load comes first.
	held := tr.acquire(nil)
	if held != want {
		t.Fatal("test setup")
	}
	if p := tr.acquire(nil); p == want {
		t.Error("a busy lane was preferred for its warm connection")
	} else {
		tr.release(p)
	}
	tr.release(held)

	// A stale warm connection is no reason to prefer a lane.
	tr.mu.Lock()
	want.warm.opened = time.Now().Add(-time.Hour)
	tr.mu.Unlock()
	seen := map[*pool]bool{}
	for i := 0; i < 6; i++ {
		p := tr.acquire(nil)
		seen[p] = true
		tr.release(p)
	}
	if len(seen) != 3 {
		t.Errorf("only %d lanes used: a stale warm connection still attracts requests", len(seen))
	}
}

// A warm connection that waited longer than warmMaxAge is closed, not handed
// over: bare TLS that idles that long is what the host software cuts.
func TestStaleWarmConnectionIsClosedAndNotUsed(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	w := &warmSlot{addr: "h:443", conn: a, opened: time.Now().Add(-warmMaxAge - time.Second)}
	if c, _ := w.take("h:443"); c != nil {
		t.Fatal("a stale warm connection was handed over")
	}
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := b.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Errorf("the stale connection was not closed (peer read: %v)", err)
	}

	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	w = &warmSlot{addr: "h:443", conn: a2, opened: time.Now()}
	if !w.fresh() {
		t.Error("a new warm connection is not fresh")
	}
	if c, _ := w.take("h:443"); c != a2 {
		t.Error("a fresh warm connection was not handed over")
	}
	if w.fresh() {
		t.Error("a taken connection is still reported as waiting")
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// End to end: a warm connection nobody asked for in time is replaced by a fresh
// dial, and the request still succeeds.
func TestRequestAfterAStaleWarmConnectionDialsAFreshOne(t *testing.T) {
	old := warmMaxAge
	warmMaxAge = 30 * time.Millisecond
	t.Cleanup(func() { warmMaxAge = old })

	srv, conns := tlsServer(t)
	base := srv.Client().Transport.(*http.Transport)
	tr := New(time.Minute, 1, 3, base.Clone)
	c := withClock(tr)
	tr.spawn = func(f func()) { f() }
	client := &http.Client{Transport: tr}

	get(t, client, srv.URL)
	c.t = c.t.Add(time.Minute)
	get(t, client, srv.URL) // warm-up opens connection 2, the lane switches
	time.Sleep(100 * time.Millisecond)
	get(t, client, srv.URL) // the warm connection is stale: connection 3

	if n := conns.Load(); n != 3 {
		t.Errorf("connections = %d, want 3 (old, stale warm, fresh)", n)
	}
	if st := tr.Stats(); st.WarmOpened != 1 || st.WarmUsed != 0 || st.Dials != 3 {
		t.Errorf("stats = %+v, want 1 warm opened, none used, 3 dials", st)
	}
}

// A lane removed by SetLimits while its warm-up runs must not be switched, and
// the connection the warm-up opened is closed.
func TestWarmUpOfARemovedLaneIsDropped(t *testing.T) {
	srv, _ := tlsServer(t)
	base := srv.Client().Transport.(*http.Transport)
	tr := New(time.Hour, 2, 4, base.Clone)
	u, _ := url.Parse(srv.URL)

	tr.mu.Lock()
	tr.target = &url.URL{Scheme: "https", Host: u.Host}
	gone := tr.lanes[1]
	gone.cur = tr.newPoolLocked()
	next := tr.newPoolLocked()
	gone.warming = next
	tr.mu.Unlock()

	tr.SetLimits(1, 4) // lane 1 leaves while "its warm-up is running"
	tr.warmUp(gone, next, tr.target)

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if gone.cur == next {
		t.Error("a removed lane was switched to its warm pool")
	}
	if n := tr.counts.warmOpened.Load(); n != 0 {
		t.Errorf("warm opened = %d for a lane that is gone", n)
	}
	if next.warm.conn != nil {
		t.Error("the connection of a dropped warm-up is still held")
	}
}

// A new connection resumes the TLS session of an earlier one: the handshake,
// the slow part on a bad network, is short.
func TestNewConnectionResumesTheTLSSession(t *testing.T) {
	srv, _ := tlsServer(t)
	base := srv.Client().Transport.(*http.Transport)
	tr := New(time.Minute, 1, 3, base.Clone)
	c := withClock(tr)
	tr.spawn = func(f func()) { f() }
	client := &http.Client{Transport: tr}

	get(t, client, srv.URL) // a full handshake; the server hands out a session ticket
	if st := tr.Stats(); st.Dials != 1 || st.Resumed != 0 {
		t.Fatalf("first connection: %+v, want 1 dial, none resumed", st)
	}
	c.t = c.t.Add(time.Minute)
	get(t, client, srv.URL) // the next pool's warm-up opens a second connection
	if st := tr.Stats(); st.Dials != 1 || st.Resumed != 1 {
		t.Errorf("second connection: %+v, want 1 dial, resumed", st)
	}
}

func TestResumeTLSOnlyWhereItIsSafe(t *testing.T) {
	cache := tls.NewLRUClientSessionCache(1)

	plain := &http.Transport{}
	resumeTLS(plain, cache)
	if plain.TLSClientConfig != nil {
		t.Error("a transport without a TLS config got one: that would switch HTTP/2 off")
	}

	h2 := &http.Transport{ForceAttemptHTTP2: true}
	resumeTLS(h2, cache)
	if h2.TLSClientConfig == nil || h2.TLSClientConfig.ClientSessionCache != cache {
		t.Error("an HTTP/2 transport got no session cache")
	}

	shared := &tls.Config{ServerName: "x"}
	own := &http.Transport{TLSClientConfig: shared}
	resumeTLS(own, cache)
	if own.TLSClientConfig.ClientSessionCache != cache || own.TLSClientConfig.ServerName != "x" {
		t.Error("an own TLS config lost its settings or got no cache")
	}
	if shared.ClientSessionCache != nil {
		t.Error("the shared config of the base transport was changed")
	}

	mine := tls.NewLRUClientSessionCache(1)
	keep := &http.Transport{TLSClientConfig: &tls.Config{ClientSessionCache: mine}}
	resumeTLS(keep, cache)
	if keep.TLSClientConfig.ClientSessionCache != mine {
		t.Error("an own session cache was replaced")
	}

	dialer := &http.Transport{ForceAttemptHTTP2: true, DialTLSContext: func(context.Context, string, string) (net.Conn, error) { return nil, nil }}
	resumeTLS(dialer, cache)
	if dialer.TLSClientConfig != nil {
		t.Error("a transport with its own TLS dialer was changed")
	}
	resumeTLS(&http.Transport{ForceAttemptHTTP2: true}, nil) // no cache: nothing to do, no panic
}

func TestStatsStringShowsResumedSessions(t *testing.T) {
	got := Stats{Dials: 4, H2: 4, Resumed: 3}.String()
	if want := "dials 4 (h2 4, http/1.1 0, 3 resumed)"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	var sum Stats
	sum.add(Stats{Resumed: 1})
	sum.add(Stats{Resumed: 2})
	if sum.Resumed != 3 {
		t.Errorf("add lost the resumed count: %+v", sum)
	}
}
