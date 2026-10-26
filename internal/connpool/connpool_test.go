package connpool

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func withClock(tr *Transport) *clock {
	c := &clock{t: time.Unix(1000, 0)}
	tr.now = c.now
	return c
}

func TestPoolRenewedAfterMaxAge(t *testing.T) {
	var made int
	tr := New(time.Minute, 1, func() *http.Transport { made++; return &http.Transport{} })
	c := withClock(tr)
	l := tr.lanes[0]

	first := tr.pool(l, nil)
	c.t = c.t.Add(59 * time.Second)
	if tr.pool(l, nil) != first {
		t.Fatal("pool renewed before maxAge")
	}
	c.t = c.t.Add(time.Second)
	second := tr.pool(l, nil)
	if second == first || made != 2 {
		t.Fatalf("pool not renewed at maxAge (made %d)", made)
	}
	if len(l.retired) != 1 || l.retired[0].t != first {
		t.Fatalf("retired = %v", l.retired)
	}
	c.t = c.t.Add(time.Minute)
	tr.pool(l, nil)
	if len(l.retired) != 1 || l.retired[0].t != second {
		t.Fatalf("old retired pool kept: %v", l.retired)
	}
}

func TestLanesRenewInTurn(t *testing.T) {
	tr := New(time.Minute, 2, func() *http.Transport { return &http.Transport{} })
	c := withClock(tr)
	a, b := tr.lanes[0], tr.lanes[1]
	pa, pb := tr.pool(a, nil), tr.pool(b, nil)

	c.t = c.t.Add(30 * time.Second)
	if tr.pool(a, nil) != pa {
		t.Fatal("lane 0 renewed at maxAge/2")
	}
	if tr.pool(b, nil) == pb {
		t.Fatal("lane 1 not renewed at maxAge/2")
	}
	c.t = c.t.Add(30 * time.Second)
	if tr.pool(a, nil) == pa {
		t.Fatal("lane 0 not renewed at maxAge")
	}
}

func TestFailedPoolIsReplaced(t *testing.T) {
	tr := New(time.Hour, 1, func() *http.Transport { return &http.Transport{} })
	l := tr.lanes[0]
	p := tr.pool(l, nil)
	if q := tr.pool(l, p); q == p {
		t.Fatal("failed pool reused")
	}
	q := tr.pool(l, nil)
	if tr.pool(l, p) != q {
		t.Fatal("a stale failure replaced the fresh pool")
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

	tr := New(time.Hour, 3, nil)
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
	for i, l := range tr.lanes {
		if n := l.inflight.Load(); n != 1 {
			t.Errorf("lane %d has %d requests in flight, want 1", i, n)
		}
	}
	release <- struct{}{}
	release <- struct{}{}
	release <- struct{}{}
	for range 3 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	for i, l := range tr.lanes {
		if n := l.inflight.Load(); n != 0 {
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

	tr := New(0, 1, nil)
	resp, err := (&http.Client{Transport: tr}).Post(srv.URL, "text/plain", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "ok" || calls.Load() != 2 {
		t.Fatalf("body %q after %d calls", b, calls.Load())
	}
	if n := tr.lanes[0].inflight.Load(); n != 0 {
		t.Fatalf("inflight = %d after the retried request ended", n)
	}
}

func TestNoRetryOnHeaderTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	tr := New(0, 1, func() *http.Transport { return &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond} })
	resp, err := (&http.Client{Transport: tr}).Post(srv.URL, "text/plain", bytes.NewReader([]byte("x")))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("no error from a stalled backend")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	if n := tr.lanes[0].inflight.Load(); n != 0 {
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

	tr := New(time.Minute, 1, nil)
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
