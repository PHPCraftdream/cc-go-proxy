package connpool

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
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

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// withClock puts tr on a fake clock and rebuilds its lanes on it.
func withClock(tr *Transport) *clock {
	c := &clock{t: time.Unix(1000, 0)}
	active, total := tr.Limits()
	tr.now = c.now
	tr.lanes = nil
	tr.SetLimits(active, total)
	return c
}

func plain() *http.Transport { return &http.Transport{} }

func TestDefaults(t *testing.T) {
	tr := New(0, 0, 0, nil)
	if a, n := tr.Limits(); a != DefaultActive || n != DefaultTotal {
		t.Fatalf("limits = %d/%d, want %d/%d", a, n, DefaultActive, DefaultTotal)
	}
	tr.SetLimits(5, 3)
	if a, n := tr.Limits(); a != 5 || n != 5 {
		t.Fatalf("total below active: limits = %d/%d, want 5/5", a, n)
	}
}

func TestPoolRenewedAfterMaxAge(t *testing.T) {
	var made int
	tr := New(time.Minute, 1, 2, func() *http.Transport { made++; return plain() })
	c := withClock(tr)

	first := tr.acquire(nil)
	tr.release(first)
	c.t = c.t.Add(59 * time.Second)
	if p := tr.acquire(nil); p != first {
		t.Fatal("pool renewed before maxAge")
	} else {
		tr.release(p)
	}
	c.t = c.t.Add(time.Second)
	second := tr.acquire(nil)
	if second == first || made != 2 {
		t.Fatalf("pool not renewed at maxAge (made %d)", made)
	}
	if len(tr.draining) != 0 {
		t.Fatalf("an idle old pool is kept draining: %v", tr.draining)
	}
	tr.release(second)
}

func TestRenewalWaitsForRoom(t *testing.T) {
	tr := New(time.Minute, 1, 2, plain)
	c := withClock(tr)

	a := tr.acquire(nil) // a long request on the first pool
	c.t = c.t.Add(time.Minute)
	b := tr.acquire(nil) // renewal: the first pool drains (2 of 2)
	if b == a || len(tr.draining) != 1 {
		t.Fatalf("no renewal with room (draining %d)", len(tr.draining))
	}
	c.t = c.t.Add(time.Minute)
	if p := tr.acquire(nil); p != b {
		t.Fatal("renewed with a busy pool beyond the total")
	} else {
		tr.release(p)
	}
	tr.release(a) // the first pool's last request ends: it is gone
	if len(tr.draining) != 0 {
		t.Fatalf("drained pool kept: %v", tr.draining)
	}
	if p := tr.acquire(nil); p == b {
		t.Fatal("renewal still waiting after room was made")
	} else {
		tr.release(p)
	}
	tr.release(b)
}

func TestStuckPoolIsForgotten(t *testing.T) {
	tr := New(time.Minute, 1, 2, plain)
	c := withClock(tr)
	tr.acquire(nil) // never released
	c.t = c.t.Add(time.Minute)
	tr.release(tr.acquire(nil))
	c.t = c.t.Add(forgetAfter)
	tr.release(tr.acquire(nil))
	if len(tr.draining) != 0 {
		t.Fatalf("a pool with a leaked request blocks the total: %v", tr.draining)
	}
}

func TestLanesRenewInTurn(t *testing.T) {
	tr := New(time.Minute, 2, 4, plain)
	c := withClock(tr)
	a, b := tr.lanes[0], tr.lanes[1]
	tr.release(tr.acquire(nil))
	tr.release(tr.acquire(nil))
	pa, pb := a.cur, b.cur

	c.t = c.t.Add(30 * time.Second)
	tr.release(tr.acquire(nil))
	tr.release(tr.acquire(nil))
	if a.cur != pa {
		t.Fatal("lane 0 renewed at maxAge/2")
	}
	if b.cur == pb {
		t.Fatal("lane 1 not renewed at maxAge/2")
	}
	c.t = c.t.Add(30 * time.Second)
	tr.release(tr.acquire(nil))
	tr.release(tr.acquire(nil))
	if a.cur == pa {
		t.Fatal("lane 0 not renewed at maxAge")
	}
}

func TestFailedPoolIsReplaced(t *testing.T) {
	tr := New(time.Hour, 1, 1, plain)
	p := tr.acquire(nil)
	q := tr.acquire(p)
	if q == p {
		t.Fatal("failed pool reused")
	}
	tr.release(p)
	tr.release(q)
	if len(tr.draining) != 0 {
		t.Fatalf("failed pool kept after its requests: %v", tr.draining)
	}
	r := tr.acquire(p) // a stale failure: the lane has moved on
	if r != q {
		t.Fatal("a stale failure replaced the fresh pool")
	}
	tr.release(r)
}

func TestSetLimitsResizesTheWindow(t *testing.T) {
	tr := New(time.Hour, 3, 3, plain)
	ps := []*pool{tr.acquire(nil), tr.acquire(nil), tr.acquire(nil)}
	tr.SetLimits(1, 4)
	if a, n := tr.Limits(); a != 1 || n != 4 {
		t.Fatalf("limits = %d/%d", a, n)
	}
	if len(tr.draining) != 2 {
		t.Fatalf("lanes leaving the window drain: %d draining, want 2", len(tr.draining))
	}
	for _, p := range ps {
		tr.release(p)
	}
	if len(tr.draining) != 0 {
		t.Fatalf("drained lanes kept: %v", tr.draining)
	}
	tr.SetLimits(4, 4)
	if a, _ := tr.Limits(); a != 4 {
		t.Fatalf("active = %d after growing", a)
	}
}

func TestParallelRequestsSpreadOverLanes(t *testing.T) {
	arrived := make(chan struct{}, 3)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	defer close(release)

	tr := New(time.Hour, 3, 3, nil)
	client := &http.Client{Transport: tr}
	errs := make(chan error, 3)
	for range 3 {
		go func() {
			resp, err := client.Get(srv.URL)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				err = resp.Body.Close()
			}
			errs <- err
		}()
	}
	for range 3 {
		<-arrived
	}
	tr.mu.Lock()
	for i, l := range tr.lanes {
		if n := l.load(); n != 1 {
			t.Errorf("lane %d has %d requests in flight, want 1", i, n)
		}
	}
	tr.mu.Unlock()
	for range 3 {
		release <- struct{}{}
	}
	for range 3 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for i, l := range tr.lanes {
		if n := l.load(); n != 0 {
			t.Errorf("lane %d keeps %d requests after they ended", i, n)
		}
	}
}

func TestRetryWhenConnectionDropsBeforeReply(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != "payload" {
			t.Errorf("body = %q", b)
		}
		if calls.Add(1) == 1 {
			c, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = c.Close()
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	tr := New(0, 1, 1, nil)
	var retried error
	ctx := WithObserver(context.Background(), &Observer{Retry: func(err error) { retried = err }})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader([]byte("payload")))
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if retried == nil {
		t.Error("observer not told about the retry")
	}
	if st := tr.Stats(); st.BreakRetries != 1 || st.ConnectRetries != 0 {
		t.Errorf("stats = %+v", st)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "ok" || calls.Load() != 2 {
		t.Fatalf("body %q after %d calls", b, calls.Load())
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if n := tr.lanes[0].load(); n != 0 || len(tr.draining) != 0 {
		t.Fatalf("inflight %d, draining %d after the retried request ended", n, len(tr.draining))
	}
}

func TestNoRetryOnHeaderTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()

	tr := New(0, 1, 1, func() *http.Transport { return &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond} })
	resp, err := (&http.Client{Transport: tr}).Post(srv.URL, "text/plain", bytes.NewReader([]byte("x")))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error from a stalled backend")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if n := tr.lanes[0].load(); n != 0 {
		t.Fatalf("inflight = %d after a failed request", n)
	}
}

func TestRetiredConnectionClosesWhenItsRequestEnds(t *testing.T) {
	var closed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	tr := New(time.Minute, 1, 2, nil)
	c := withClock(tr)
	client := &http.Client{Transport: tr}

	old, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	fresh, err := client.Get(srv.URL) // the first pool retires; its connection is busy
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, fresh.Body)
	_ = fresh.Body.Close()
	if n := closed.Load(); n != 0 {
		t.Fatalf("%d connections closed while in use", n)
	}

	_, _ = io.Copy(io.Discard, old.Body)
	_ = old.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := closed.Load(); n != 1 {
		t.Fatalf("closed = %d, want 1 (the retired connection only)", n)
	}
}

type timeoutErr struct{ msg string }

func (e timeoutErr) Error() string   { return e.msg }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return true }

func TestConnectFailuresAreRetryable(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/x", bytes.NewReader([]byte("b")))
	for _, err := range []error{
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
		timeoutErr{"net/http: TLS handshake timeout"},
		&connectError{errors.New("TLS handshake timeout")},
	} {
		if !connectFailure(err) || !retryable(req, err) {
			t.Errorf("%v: not a retryable connect failure", err)
		}
	}
	if retryable(req, timeoutErr{"net/http: timeout awaiting response headers"}) {
		t.Error("a header timeout is retried")
	}
}

// failingDials returns a pool factory whose dial number n (1-based) fails.
func failingDials(dials *atomic.Int32, n int32, base func() *http.Transport) func() *http.Transport {
	return func() *http.Transport {
		tr := base()
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dials.Add(1) == n {
				return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("refused")}
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		return tr
	}
}

func okServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, c *http.Client, url string) {
	t.Helper()
	getCtx(t, c, url, context.Background())
}

func getCtx(t *testing.T, c *http.Client, url string, ctx context.Context) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestConnectFailureGoesToALiveLane(t *testing.T) {
	srv := okServer(t)
	var dials atomic.Int32
	tr := New(time.Hour, 2, 2, failingDials(&dials, 2, plain))
	client := &http.Client{Transport: tr}
	get(t, client, srv.URL) // one lane connects
	get(t, client, srv.URL) // the other cannot: sent over the first lane's connection
	if n := dials.Load(); n != 2 {
		t.Fatalf("dials = %d, want 2 (no new dial for the retry)", n)
	}
	if st := tr.Stats(); st.ConnectRetries != 1 || st.BreakRetries != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestConnectFailureWithoutALiveLaneTriesAFreshPool(t *testing.T) {
	srv := okServer(t)
	var dials atomic.Int32
	tr := New(time.Hour, 1, 1, failingDials(&dials, 1, plain))
	get(t, &http.Client{Transport: tr}, srv.URL)
	if n := dials.Load(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
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
	get(t, client, srv.URL) // due: the next pool warms up, this request stays on the old one
	if tr.lanes[0].cur != first || tr.lanes[0].warming == nil || !tr.lanes[0].warming.ready {
		t.Fatal("no warm next pool while the old one serves")
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("connections = %d, want 2 (old + warm)", n)
	}
	warmAge := time.Duration(-1)
	getCtx(t, client, srv.URL, WithObserver(context.Background(), &Observer{WarmConn: func(age time.Duration) { warmAge = age }}))
	if warmAge < 0 {
		t.Fatal("observer not told about the warm connection")
	}
	if tr.lanes[0].cur == first || tr.lanes[0].warming != nil {
		t.Fatal("lane did not switch to the warm pool")
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
	installWarmDialer(p, target, nil)
	if p.warm != nil || p.t.DialTLSContext != nil {
		t.Fatal("warm dialer installed behind a proxy")
	}
	q := &pool{t: &http.Transport{}}
	installWarmDialer(q, target, nil)
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

// withChoice returns a request context that goes out the way c says.
func withChoice(c proxydial.Choice) context.Context {
	return proxydial.WithChoice(context.Background(), c)
}

func TestRouterKeepsOneTransportPerWayOut(t *testing.T) {
	target := okServer(t)
	proxyA, tunnelsA := connectProxy(t)
	proxyB, tunnelsB := connectProxy(t)
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	r := NewRouter(time.Hour, 1, 2, plain)
	client := &http.Client{Transport: r}

	get(t, client, target.URL) // no choice, no default: the environment's way, here direct
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{URL: proxyA}))
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{URL: proxyA}))
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{URL: proxyB}))
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{Direct: true}))
	if a, b := tunnelsA.Load(), tunnelsB.Load(); a != 1 || b != 1 {
		t.Fatalf("tunnels A %d, B %d: want 1 and 1 (the second request to A reuses its connection)", a, b)
	}
	if n := len(r.ts); n != 4 {
		t.Errorf("transports = %d, want 4 (env, A, B, direct)", n)
	}

	// Without a choice on the request the default proxy applies.
	proxydial.SetDefault(proxyB)
	get(t, client, target.URL)
	if b := tunnelsB.Load(); b != 1 {
		t.Errorf("B tunnels = %d: the default proxy is the same way out as B, which already has its connection", b)
	}
	proxydial.SetDefault(nil)
}

func TestRouterDirectIgnoresTheEnvironmentProxy(t *testing.T) {
	target := okServer(t)
	proxy, tunnels := connectProxy(t)
	t.Setenv("HTTP_PROXY", proxy.String())
	t.Setenv("http_proxy", proxy.String())
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	base := func() *http.Transport { return &http.Transport{Proxy: http.ProxyURL(proxy)} }
	r := NewRouter(time.Hour, 1, 2, base)
	client := &http.Client{Transport: r}
	getCtx(t, client, target.URL, withChoice(proxydial.Choice{Direct: true}))
	if n := tunnels.Load(); n != 0 {
		t.Fatalf("a direct request went through the proxy (%d tunnels)", n)
	}
}

func TestRouterLimitsReachEveryTransport(t *testing.T) {
	r := NewRouter(time.Hour, 3, 5, plain)
	a := r.transport(proxydial.Choice{})
	r.SetLimits(2, 4)
	b := r.transport(proxydial.Choice{Direct: true}) // created after the change
	for name, tr := range map[string]*Transport{"existing": a, "new": b} {
		if x, y := tr.Limits(); x != 2 || y != 4 {
			t.Errorf("%s transport limits = %d/%d, want 2/4", name, x, y)
		}
	}
	if x, y := r.Limits(); x != 2 || y != 4 {
		t.Errorf("router limits = %d/%d", x, y)
	}
	a.counts.dials.Add(2)
	b.counts.dials.Add(3)
	if st := r.Stats(); st.Dials != 5 {
		t.Errorf("stats dials = %d, want 5 (summed)", st.Dials)
	}
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
