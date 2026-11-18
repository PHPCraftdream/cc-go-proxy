package connpool

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
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

// tlsServer counts the connections it accepts.
func tlsServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &conns
}

func TestLaneSwitchesToAWarmPool(t *testing.T) {
	srv, conns := tlsServer(t)
	base := srv.Client().Transport.(*http.Transport)
	tr := New(time.Minute, 1, 3, base.Clone)
	c := withClock(tr)
	tr.spawn = func(f func()) { f() }
	client := &http.Client{Transport: tr}

	var handshakes []string
	traced := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		TLSHandshakeStart: func() { handshakes = append(handshakes, "start") },
		TLSHandshakeDone:  func(_ tls.ConnectionState, err error) { handshakes = append(handshakes, fmt.Sprint("done ", err)) },
	})
	getCtx(t, client, srv.URL, traced) // dialed by connpool's own TLS dialer
	// The real handshake first; the transport then reports its own, empty one.
	if !strings.HasPrefix(strings.Join(handshakes, ","), "start,done <nil>") {
		t.Fatalf("TLS trace = %v, want start and a clean done first", handshakes)
	}
	first := tr.lanes[0].cur
	c.t = c.t.Add(time.Minute)
	get(t, client, srv.URL) // due: the next pool warms up and takes the lane at once, this request stays on the old one
	if tr.lanes[0].cur == first || tr.lanes[0].warming != nil || !tr.lanes[0].cur.ready {
		t.Fatal("the lane did not switch to the warm pool as soon as it was open")
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("connections = %d, want 2 (old + warm)", n)
	}
	if len(tr.draining) != 0 {
		t.Fatalf("the old pool still drains after its only request ended: %v", tr.draining)
	}
	switched := tr.lanes[0].cur
	warmAge := time.Duration(-1)
	getCtx(t, client, srv.URL, WithObserver(context.Background(), &Observer{WarmConn: func(age time.Duration) { warmAge = age }}))
	if warmAge < 0 {
		t.Fatal("observer not told about the warm connection")
	}
	if tr.lanes[0].cur != switched || tr.lanes[0].warming != nil {
		t.Fatal("the lane left the warm pool")
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("connections = %d, want 2: the request did not use the warm connection", n)
	}
	if st := tr.Stats(); st.Dials != 2 || st.HTTP1 != 2 || st.WarmOpened != 1 || st.WarmUsed != 1 || st.WarmFailed != 0 {
		t.Errorf("stats = %+v", st)
	}
	if st := tr.Stats(); st != (Stats{}) {
		t.Errorf("stats not reset: %+v", st)
	}
}

func TestFailedWarmUpSwitchesAfterWait(t *testing.T) {
	srv, _ := tlsServer(t)
	base := srv.Client().Transport.(*http.Transport)
	var dials atomic.Int32
	tr := New(time.Minute, 1, 3, failingDials(&dials, 2, base.Clone))
	c := withClock(tr)
	tr.spawn = func(f func()) { f() }
	client := &http.Client{Transport: tr}

	get(t, client, srv.URL)
	first := tr.lanes[0].cur
	c.t = c.t.Add(time.Minute)
	get(t, client, srv.URL) // the warm-up (dial 2) fails
	if w := tr.lanes[0].warming; w == nil || !w.failed {
		t.Fatal("failed warm-up not recorded")
	}
	get(t, client, srv.URL)
	if tr.lanes[0].cur != first || dials.Load() != 2 {
		t.Fatalf("switched before warmWait (dials %d)", dials.Load())
	}
	c.t = c.t.Add(warmWait)
	get(t, client, srv.URL)
	if tr.lanes[0].cur == first || dials.Load() != 3 {
		t.Fatalf("no direct switch after warmWait (dials %d)", dials.Load())
	}
	if st := tr.Stats(); st.WarmFailed != 1 || st.WarmOpened != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestNoWarmUpThroughAProxy(t *testing.T) {
	target := &url.URL{Scheme: "https", Host: "example.com"}
	p := &pool{t: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://proxy:8080") }}}
	installWarmDialer(p, target, nil, nil)
	if p.warm != nil || p.t.DialTLSContext != nil {
		t.Fatal("warm dialer installed behind a proxy")
	}
	q := &pool{t: &http.Transport{}}
	installWarmDialer(q, target, nil, nil)
	if q.warm == nil || q.t.DialTLSContext == nil {
		t.Fatal("no warm dialer without a proxy")
	}
}

// connectProxy counts the CONNECT tunnels it opens.
func connectProxy(t *testing.T) (*url.URL, *atomic.Int32) {
	var tunnels atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		tunnels.Add(1)
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		_, _ = io.Copy(up, c)
		_ = up.Close()
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u, &tunnels
}

func TestWarmUpGoesThroughTheProxy(t *testing.T) {
	srv, conns := tlsServer(t)
	proxy, tunnels := connectProxy(t)
	base := srv.Client().Transport.(*http.Transport)
	tr := New(time.Minute, 1, 3, base.Clone)
	tr.choice = proxydial.Choice{URL: proxy}
	c := withClock(tr)
	tr.spawn = func(f func()) { f() }
	client := &http.Client{Transport: tr}
	get(t, client, srv.URL)
	c.t = c.t.Add(time.Minute)
	get(t, client, srv.URL) // warms the next pool up through the proxy
	get(t, client, srv.URL) // and uses it
	if n, m := tunnels.Load(), conns.Load(); n != 2 || m != 2 {
		t.Fatalf("tunnels %d, server connections %d: want 2 and 2 (first + warm, both proxied)", n, m)
	}
}

func TestStatsString(t *testing.T) {
	if s := (Stats{}).String(); s != "" {
		t.Errorf("empty stats = %q", s)
	}
	s := Stats{Dials: 3, H2: 2, HTTP1: 1, WarmOpened: 2, WarmUsed: 1, ConnectRetries: 1}.String()
	want := "dials 3 (h2 2, http/1.1 1); warm 2 opened, 1 used, 0 failed; retries 1 after connect failures, 0 after broken connections"
	if s != want {
		t.Errorf("String = %q, want %q", s, want)
	}
}

func newBlackhole(t *testing.T, target string) *blackhole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &blackhole{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.accepted.Add(1)
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if n > 0 && !b.dropping.Load() {
						_, _ = dst.Write(buf[:n])
					}
					if err != nil {
						_ = dst.Close()
						return
					}
				}
			}
			go pipe(up, c)
			go pipe(c, up)
		}
	}()
	return b
}

func TestHealthPingDropsADeadConnectionBeforeUse(t *testing.T) {
	oldPing, oldTimeout := healthPing, healthTimeout
	healthPing, healthTimeout = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { healthPing, healthTimeout = oldPing, oldTimeout })

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	bh := newBlackhole(t, strings.TrimPrefix(srv.URL, "https://"))

	base := srv.Client().Transport.(*http.Transport)
	tr := New(time.Hour, 1, 1, base.Clone)
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	url := "https://" + bh.ln.Addr().String()
	get(t, client, url)
	if n := bh.accepted.Load(); n != 1 {
		t.Fatalf("connections = %d, want 1", n)
	}

	bh.dropping.Store(true) // the connection dies silently while it is idle
	time.Sleep(time.Second)
	bh.dropping.Store(false)

	start := time.Now()
	get(t, client, url)
	if n := bh.accepted.Load(); n != 2 {
		t.Fatalf("connections = %d, want 2: the dead one was reused", n)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the next request took %s", d)
	}
}

func TestHardenKeepsOwnHTTP2Settings(t *testing.T) {
	own := &http.Transport{HTTP2: &http.HTTP2Config{SendPingTimeout: time.Hour}}
	Harden(own, time.Second, time.Second)
	if own.HTTP2.SendPingTimeout != time.Hour {
		t.Errorf("own settings replaced: %+v", own.HTTP2)
	}
	plain := &http.Transport{}
	Harden(plain, 3*time.Second, 2*time.Second)
	if plain.HTTP2 == nil || plain.HTTP2.SendPingTimeout != 3*time.Second || plain.HTTP2.PingTimeout != 2*time.Second {
		t.Errorf("settings = %+v", plain.HTTP2)
	}
	tr := New(0, 1, 1, nil)
	p := tr.acquire(nil)
	if p.t.HTTP2 == nil || p.t.HTTP2.SendPingTimeout != healthPing {
		t.Errorf("a pool is built without pings: %+v", p.t.HTTP2)
	}
	tr.release(p)
}

// api.z.ai closes an HTTP/2 connection that was quiet for 30 s (measured: a
// ping at 25 s is answered, at 30 s there is a GOAWAY), and a ping keeps it
// open: the first one has to come well before, or no request ever reuses one.
func TestHealthPingComesBeforeAShortIdleLimit(t *testing.T) {
	if healthPing > 20*time.Second {
		t.Errorf("first health ping after %s of silence, want 20 s at most", healthPing)
	}
}

// Discard drops the pool of the latest attempt: the next request opens a new
// connection, where without it the idle one is used again.
func TestDiscardGivesTheLaneAFreshConnection(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	tr := New(time.Hour, 1, 1, nil)
	obs := &Observer{}
	ctx := WithObserver(context.Background(), obs)
	client := &http.Client{Transport: tr}

	getCtx(t, client, srv.URL, ctx)
	getCtx(t, client, srv.URL, ctx)
	if n := conns.Load(); n != 1 {
		t.Fatalf("connections = %d before Discard, want the one reused", n)
	}
	obs.Discard()
	getCtx(t, client, srv.URL, ctx)
	if n := conns.Load(); n != 2 {
		t.Fatalf("connections = %d after Discard, want a new one", n)
	}
}

// An observer that saw no attempt has nothing to drop.
func TestDiscardWithoutAnAttemptIsANoOp(t *testing.T) {
	(&Observer{}).Discard()
}

func TestDiscardReplacesThePoolOfItsLane(t *testing.T) {
	tr := New(time.Hour, 2, 4, plain)
	p := tr.acquire(nil)
	tr.release(p)
	tr.discard(p)
	for _, l := range tr.lanes {
		if l.cur == p {
			t.Fatal("the pool is still in its lane")
		}
	}
	if len(tr.draining) != 0 {
		t.Fatalf("an idle discarded pool is kept draining: %v", tr.draining)
	}
}

// A pool no lane holds any more (already renewed) is left alone.
func TestDiscardIgnoresAPoolOutOfTheWindow(t *testing.T) {
	tr := New(time.Hour, 1, 1, plain)
	p := tr.acquire(nil)
	tr.release(p)
	tr.discard(&pool{})
	if q := tr.acquire(nil); q != p {
		t.Fatal("a pool of no lane replaced the lane's pool")
	} else {
		tr.release(q)
	}
}

// A request still running on the discarded pool finishes there.
func TestDiscardKeepsARunningRequestOnItsPool(t *testing.T) {
	tr := New(time.Hour, 1, 2, plain)
	p := tr.acquire(nil)
	tr.discard(p)
	if len(tr.draining) != 1 || tr.draining[0] != p {
		t.Fatalf("the busy pool is not draining: %v", tr.draining)
	}
	tr.release(p)
	if len(tr.draining) != 0 {
		t.Fatalf("the pool stays after its last request: %v", tr.draining)
	}
}

// A lane nobody picked for long is renewed lazily: the request that finds it due
// stays on the old pool while the next one warms up. Past staleFactor*maxAge the
// old connection is about to be closed by the far end, so the request must not
// go there: the lane gets a fresh pool at once.
func TestStaleLaneTakesNoRequestOnTheOldPool(t *testing.T) {
	// idleLane: one lane with a used pool, whose warm-up never opens, left alone for age.
	idleLane := func(age time.Duration) (*Transport, *lane, *pool, *clock) {
		tr := New(time.Minute, 1, 3, plain)
		c := withClock(tr)
		tr.target = &url.URL{Scheme: "https", Host: "warm.invalid:443"}
		tr.spawn = func(func()) {}
		first := tr.acquire(nil)
		tr.release(first)
		c.t = c.t.Add(age)
		return tr, tr.lanes[0], first, c
	}

	for _, age := range []time.Duration{time.Minute, 2 * time.Minute} { // due, and exactly at the bound
		tr, l, first, _ := idleLane(age)
		p := tr.acquire(nil)
		tr.release(p)
		if p != first || l.warming == nil {
			t.Fatalf("age %v: a due lane must serve on its old pool and warm the next (same %v, warming %v)", age, p == first, l.warming != nil)
		}
	}

	tr, l, first, c := idleLane(2*time.Minute + time.Second)
	p := tr.acquire(nil)
	if p == first || l.cur != p {
		t.Fatal("a stale lane served a request on its old pool")
	}
	if l.warming != nil {
		t.Fatal("the stale lane kept a warm-up, which could switch it again")
	}
	if !l.born.Equal(c.t) {
		t.Fatalf("the fresh pool is born at %v, want now", l.born)
	}
	if len(tr.draining) != 0 {
		t.Fatalf("an idle stale pool is kept draining: %v", tr.draining)
	}
	tr.release(p)
}

// A stale pool with a request still running drains, and the request is not cut.
func TestStaleLaneKeepsItsRunningRequest(t *testing.T) {
	tr := New(time.Minute, 1, 3, plain)
	c := withClock(tr)

	long := tr.acquire(nil) // a long request, never released yet
	c.t = c.t.Add(3 * time.Minute)
	p := tr.acquire(nil)
	if p == long {
		t.Fatal("a stale lane served a request on its old pool")
	}
	if len(tr.draining) != 1 || tr.draining[0] != long {
		t.Fatalf("the stale pool with a running request must drain: %v", tr.draining)
	}
	tr.release(long)
	if len(tr.draining) != 0 {
		t.Fatalf("drained pool kept after its last request: %v", tr.draining)
	}
	tr.release(p)
}

// On real connections: a lane that is merely due sends the request down its old
// connection while the next one warms up, but one left alone past
// staleFactor*maxAge opens a connection of its own, drops the pending warm-up,
// and that warm-up, finishing later, must not switch the lane again.
func TestStaleLaneOnRealConnections(t *testing.T) {
	srv, conns := tlsServer(t)
	base := srv.Client().Transport.(*http.Transport)
	tr := New(time.Minute, 1, 3, base.Clone)
	c := withClock(tr)
	var warmUps []func() // run by the test, when it says
	tr.spawn = func(f func()) { warmUps = append(warmUps, f) }
	client := &http.Client{Transport: tr}

	reused := func() bool {
		var got bool
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(i httptrace.GotConnInfo) { got = i.Reused },
		})
		getCtx(t, client, srv.URL, ctx)
		return got
	}

	if reused() {
		t.Fatal("the first request cannot reuse a connection")
	}
	first := tr.lanes[0].cur

	// Due late, not stale: the warm-up starts at 1m50s, so that the lane is
	// stale 11 s later, well inside warmWait, where only the stale rule can
	// still switch it.
	c.t = c.t.Add(110 * time.Second)
	if !reused() || tr.lanes[0].cur != first {
		t.Fatal("a due lane must still serve on its old connection")
	}
	if tr.lanes[0].warming == nil || len(warmUps) != 1 {
		t.Fatalf("a due lane must warm the next pool (warming %v, warm-ups %d)", tr.lanes[0].warming != nil, len(warmUps))
	}

	c.t = c.t.Add(11 * time.Second) // 2m1s old: stale
	if reused() {
		t.Fatal("a stale lane sent a request down its old connection")
	}
	fresh := tr.lanes[0].cur
	if fresh == first || tr.lanes[0].warming != nil {
		t.Fatalf("a stale lane keeps its old pool or its warm-up (new pool %v, warming %v)", fresh != first, tr.lanes[0].warming != nil)
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("connections = %d, want 2 (old, new)", n)
	}

	warmUps[0]() // the dropped warm-up finishes now
	if tr.lanes[0].cur != fresh {
		t.Fatal("a dropped warm-up switched the lane again")
	}
	if st := tr.Stats(); st.WarmOpened != 0 {
		t.Errorf("a dropped warm-up counts as opened: %+v", st)
	}
	if !reused() || tr.lanes[0].cur != fresh {
		t.Error("the fresh pool's connection is not kept for the next request")
	}
}

func TestConnectRetrySkipsStaleReadyLanes(t *testing.T) {
	srv := okServer(t)
	var dials atomic.Int32
	tr := New(time.Minute, 2, 2, failingDials(&dials, 3, plain))
	c := withClock(tr)
	client := &http.Client{Transport: tr}

	get(t, client, srv.URL) // lane 1: first ready pool
	get(t, client, srv.URL) // lane 0: second ready pool
	old := [2]*pool{tr.lanes[0].cur, tr.lanes[1].cur}
	for i, p := range old {
		if !p.ready || p.inflight != 0 {
			t.Fatalf("lane %d is not idle and ready", i)
		}
	}

	c.t = c.t.Add(2*time.Minute + time.Second)
	for i, l := range tr.lanes {
		if c.now().Sub(l.born) <= staleFactor*tr.maxAge {
			t.Fatalf("lane %d is not stale", i)
		}
	}

	get(t, client, srv.URL) // fresh dial fails; retry must not use either old pool
	if n := dials.Load(); n != 4 {
		t.Fatalf("dials = %d, want 4 (two old pools, failed fresh dial, retried fresh dial)", n)
	}
	if st := tr.Stats(); st.ConnectRetries != 1 {
		t.Fatalf("stats = %+v, want one connect retry", st)
	}
	if tr.lanes[0].cur != old[0] || tr.lanes[1].cur == old[1] {
		t.Fatal("retry did not leave the unused stale lane alone and replace the failed lane")
	}
}
