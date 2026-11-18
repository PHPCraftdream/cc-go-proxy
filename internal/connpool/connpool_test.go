package connpool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

func (e timeoutErr) Error() string { return e.msg }

func (e timeoutErr) Timeout() bool { return true }

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

// shortRetries shrinks the retry budget and pauses so the tests do not wait.
func shortRetries(t *testing.T, budget time.Duration) {
	t.Helper()
	oldBudget, oldBackoff := retryBudget, retryBackoff
	retryBudget, retryBackoff = budget, []time.Duration{time.Millisecond}
	t.Cleanup(func() { retryBudget, retryBackoff = oldBudget, oldBackoff })
}

// failingFirstDials returns a pool factory whose first n dials fail.
func failingFirstDials(dials *atomic.Int32, n int32, base func() *http.Transport) func() *http.Transport {
	return func() *http.Transport {
		tr := base()
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dials.Add(1) <= n {
				return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("unreachable")}
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		return tr
	}
}

func TestConnectFailuresRetryUntilItWorks(t *testing.T) {
	shortRetries(t, 10*time.Second)
	srv := okServer(t)
	var dials atomic.Int32
	tr := New(time.Hour, 1, 1, failingFirstDials(&dials, 3, plain))
	get(t, &http.Client{Transport: tr}, srv.URL)
	if n := dials.Load(); n != 4 {
		t.Fatalf("dials = %d, want 4 (three failures, then the one that works)", n)
	}
	if st := tr.Stats(); st.ConnectRetries != 3 || st.BreakRetries != 0 {
		t.Errorf("stats = %+v", st)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if n := tr.lanes[0].load(); n != 0 || len(tr.draining) != 0 {
		t.Errorf("inflight %d, draining %d after the request", n, len(tr.draining))
	}
}

func TestConnectRetriesStopAtTheBudget(t *testing.T) {
	shortRetries(t, 50*time.Millisecond)
	srv := okServer(t)
	var dials atomic.Int32
	tr := New(time.Hour, 1, 1, failingFirstDials(&dials, 1<<30, plain))
	start := time.Now()
	resp, err := (&http.Client{Transport: tr}).Post(srv.URL, "text/plain", bytes.NewReader([]byte("x")))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error although no connection can be opened")
	}
	if !connectFailure(err) {
		t.Errorf("err = %v, want the connect failure", err)
	}
	if n := dials.Load(); n < 3 || time.Since(start) > 5*time.Second {
		t.Errorf("dials = %d after %s: want several attempts within the budget", n, time.Since(start))
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if n := tr.lanes[0].load(); n != 0 {
		t.Errorf("inflight = %d after the failed request", n)
	}
}

func TestBrokenConnectionRetriesAreCapped(t *testing.T) {
	shortRetries(t, time.Minute)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	defer srv.Close()
	tr := New(0, 1, 1, nil)
	resp, err := (&http.Client{Transport: tr}).Post(srv.URL, "text/plain", bytes.NewReader([]byte("x")))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error from a server that always drops the connection")
	}
	if n := calls.Load(); n != int32(1+maxBreakRetries) {
		t.Fatalf("calls = %d, want %d (the request and %d retries)", n, 1+maxBreakRetries, maxBreakRetries)
	}
	if st := tr.Stats(); st.BreakRetries != int64(maxBreakRetries) {
		t.Errorf("stats = %+v", st)
	}
}

// A connection that breaks after a long wait is retried although the connect
// budget has long passed: the budget is for connect failures only.
func TestBrokenConnectionIsRetriedAfterALongWait(t *testing.T) {
	shortRetries(t, 20*time.Millisecond)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			_, _ = w.Write([]byte("ok"))
			return
		}
		time.Sleep(150 * time.Millisecond) // past the budget
		if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = c.Close()
		}
	}))
	defer srv.Close()
	tr := New(0, 1, 1, nil)
	resp, err := (&http.Client{Transport: tr}).Post(srv.URL, "text/plain", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("the request failed instead of being retried: %v", err)
	}
	_ = resp.Body.Close()
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
	if st := tr.Stats(); st.BreakRetries != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestRetryPauseEndsWithTheContext(t *testing.T) {
	oldBudget, oldBackoff := retryBudget, retryBackoff
	retryBudget, retryBackoff = time.Hour, []time.Duration{time.Hour}
	t.Cleanup(func() { retryBudget, retryBackoff = oldBudget, oldBackoff })
	srv := okServer(t)
	var dials atomic.Int32
	tr := New(time.Hour, 1, 1, failingFirstDials(&dials, 1<<30, plain))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader([]byte("x")))
	start := time.Now()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the pause did not end with the context (%s)", time.Since(start))
	}
	if n := dials.Load(); n != 2 {
		t.Errorf("dials = %d, want 2: the immediate retry, then the pause", n)
	}
}

func TestBackoffGrowsAndSettles(t *testing.T) {
	old := retryBackoff
	retryBackoff = []time.Duration{time.Second, 2 * time.Second}
	t.Cleanup(func() { retryBackoff = old })
	for n, want := range []time.Duration{time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second} {
		if got := backoff(n); got != want {
			t.Errorf("backoff(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestRefusedConnectionIsTriedOnceMore(t *testing.T) {
	shortRetries(t, time.Minute) // a long budget: only the refusal rule can stop the retries
	var dials atomic.Int32
	tr := New(time.Hour, 1, 1, func() *http.Transport {
		return &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}}
	})
	start := time.Now()
	resp, err := (&http.Client{Transport: tr}).Post("http://127.0.0.1:1/x", "text/plain", bytes.NewReader([]byte("x")))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error from a refused connection")
	}
	if !connectRefused(err) {
		t.Errorf("err = %v, want the refusal", err)
	}
	if n := dials.Load(); n != 2 || time.Since(start) > 5*time.Second {
		t.Errorf("dials = %d after %s: want the request and one retry", n, time.Since(start))
	}
}

func TestConnectRefusedText(t *testing.T) {
	for _, msg := range []string{
		"dial tcp 127.0.0.1:1: connect: connection refused",
		"dial tcp 127.0.0.1:1: connectex: No connection could be made because the target machine actively refused it.",
	} {
		if !connectRefused(errors.New(msg)) {
			t.Errorf("%q is not a refusal", msg)
		}
	}
	if connectRefused(errors.New("net/http: TLS handshake timeout")) || connectRefused(errors.New("dial tcp: i/o timeout")) {
		t.Error("a timeout counts as a refusal")
	}
}

// blackhole forwards TCP to a server until it is told to swallow everything,
// leaving the sockets open: a connection that a middlebox silently killed.
type blackhole struct {
	ln       net.Listener
	accepted atomic.Int32
	dropping atomic.Bool
}

// The observer hears how old the pool of every attempt is: a new connection on
// an old pool tells that the pool's earlier connection was lost.
func TestObserverHearsPoolAge(t *testing.T) {
	srv := okServer(t)
	tr := New(0, 1, 1, nil)
	base := time.Now()
	var offset atomic.Int64
	tr.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }

	var ages []time.Duration
	ctx := WithObserver(context.Background(), &Observer{Pool: func(age time.Duration) { ages = append(ages, age) }})
	client := &http.Client{Transport: tr}
	getCtx(t, client, srv.URL, ctx)
	offset.Store(int64(90 * time.Second))
	getCtx(t, client, srv.URL, ctx)

	if len(ages) != 2 {
		t.Fatalf("heard %d pool ages, want one per attempt: %v", len(ages), ages)
	}
	if ages[0] > time.Second || ages[1] < 90*time.Second || ages[1] > 91*time.Second {
		t.Errorf("ages = %v, want about 0 and about 90s", ages)
	}
}

// A pool must not drop a connection for being idle: the default transport does
// after 90 s, and with a few requests a minute over several lanes most requests
// then opened a new connection.
func TestPoolKeepsIdleConnections(t *testing.T) {
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

	tr := New(time.Hour, 1, 1, func() *http.Transport {
		return &http.Transport{IdleConnTimeout: 50 * time.Millisecond} // as http.DefaultTransport has 90 s
	})
	c := &http.Client{Transport: tr}
	get(t, c, srv.URL)
	time.Sleep(300 * time.Millisecond)
	get(t, c, srv.URL)
	if n := conns.Load(); n != 1 {
		t.Fatalf("server accepted %d connections, want 1: the idle one was closed", n)
	}
}

// transport returns the Transport of a way out, creating it (for checks that inspect a route).
func (r *Router) transport(c proxydial.Choice) *Transport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.transportLocked(c)
}

// The error texts below are the ones seen in the logs of 2026-10-26/27 and in
// earlier incidents. Which of them may be sent again decides whether a user
// sees a 502 or nothing at all.
func TestRetryableClassification(t *testing.T) {
	post := func(ctx context.Context) *http.Request {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/x", bytes.NewReader([]byte("x")))
		return req
	}
	retried := []string{
		// Windows closing a connection under us (Proxifier, antivirus): 26 requests in one night.
		"write tcp 127.0.0.1:59942->8.6.112.5:443: wsasend: An established connection was aborted by the software in your host machine.",
		"read tcp 127.0.0.1:61531->8.6.112.0:443: wsarecv: An established connection was aborted by the software in your host machine.",
		"unexpected EOF",
		"EOF",
		"read tcp 10.0.0.2:5->1.1.1.1:443: read: connection reset by peer",
		"wsarecv: An existing connection was forcibly closed by the remote host.",
		"write tcp: broken pipe",
		"http2: server sent GOAWAY and closed the connection; LastStreamID=7, ErrCode=NO_ERROR, debug=\"\"",
		"http2: client conn is closed",
		"http2: client conn not usable",
		"use of closed network connection",
		"http: server closed idle connection",
		"net/http: HTTP/1.x transport connection broken: unexpected EOF",
		"connect: TLS handshake timeout",
		"dial tcp 8.6.112.0:443: connectex: A socket operation was attempted to an unreachable network.",
	}
	for _, msg := range retried {
		err := errors.New(msg)
		// The connection errors above that are timeouts need the Timeout() method.
		if strings.Contains(msg, "TLS handshake timeout") {
			err = timeoutErr{msg}
		}
		if strings.Contains(msg, "unreachable network") {
			err = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New(msg)}
		}
		if !retryable(post(context.Background()), err) {
			t.Errorf("not retried: %q", msg)
		}
	}

	notRetried := []error{
		timeoutErr{"net/http: timeout awaiting response headers"}, // a stalled backend: fail fast
		errors.New("x509: certificate signed by unknown authority"),
		errors.New("tls: failed to verify certificate: x509: certificate has expired"),
		errors.New("net/http: request canceled"),
		errors.New("proxy 127.0.0.1:1080: socks5: user name or password rejected"),
	}
	for _, err := range notRetried {
		if retryable(post(context.Background()), err) {
			t.Errorf("retried: %v", err)
		}
	}

	// A canceled request is never sent again, whatever the error says.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if retryable(post(ctx), errors.New("unexpected EOF")) {
		t.Error("a canceled request was retried")
	}
	// Neither is one whose body cannot be replayed: it could be sent twice.
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/x", struct{ io.Reader }{strings.NewReader("x")})
	if req.GetBody != nil {
		t.Fatal("test setup: the body is replayable")
	}
	if retryable(req, errors.New("unexpected EOF")) {
		t.Error("a request with a body that cannot be replayed was retried")
	}
}

func TestConnectFailureClassification(t *testing.T) {
	for _, err := range []error{
		timeoutErr{"net/http: TLS handshake timeout"},
		&connectError{errors.New("TLS handshake timeout")},
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")},
		errors.New("proxyconnect tcp: dial tcp 127.0.0.1:1080: connect: connection refused"),
	} {
		if !connectFailure(err) {
			t.Errorf("%v is a connect failure (the request was never sent)", err)
		}
	}
	for _, err := range []error{
		errors.New("unexpected EOF"),
		errors.New("write tcp: wsasend: An established connection was aborted by the software in your host machine."),
		timeoutErr{"net/http: timeout awaiting response headers"},
	} {
		if connectFailure(err) {
			t.Errorf("%v is not a connect failure (the request may have been sent)", err)
		}
	}
}

// A body that cannot be replayed must reach the server once, whatever happens.
func TestNonReplayableBodyIsNeverResent(t *testing.T) {
	shortRetries(t, time.Minute)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	defer srv.Close()
	tr := New(0, 1, 1, nil)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, struct{ io.Reader }{strings.NewReader("payload")})
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error from a server that drops the connection")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the server saw %d requests, want 1", n)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if n := tr.lanes[0].load(); n != 0 {
		t.Errorf("inflight = %d after the failed request", n)
	}
}

// Many requests at once while the lane limits change and the pools are renewed
// all the time: nothing may deadlock, leak a slot or fail. CI runs it with -race.
func TestConcurrentRequestsWhileLimitsAndPoolsChange(t *testing.T) {
	srv := okServer(t)
	tr := New(15*time.Millisecond, 3, 5, nil) // pools renew every 15 ms
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}

	stop := make(chan struct{})
	var changer sync.WaitGroup
	changer.Add(1)
	go func() {
		defer changer.Done()
		limits := [][2]int{{2, 4}, {5, 6}, {1, 1}, {4, 8}, {3, 5}}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				l := limits[i%len(limits)]
				tr.SetLimits(l[0], l[1])
			}
		}
	}()

	var wg sync.WaitGroup
	var failed atomic.Int32
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				resp, err := client.Post(srv.URL, "text/plain", bytes.NewReader([]byte("x")))
				if err != nil {
					failed.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	close(stop)
	changer.Wait()

	if n := failed.Load(); n != 0 {
		t.Errorf("%d of %d requests failed", n, 24*15)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	total := 0
	for _, l := range tr.lanes {
		total += l.load()
	}
	for _, d := range tr.draining {
		total += d.inflight
	}
	if total != 0 {
		t.Errorf("%d request slots leaked", total)
	}
}
