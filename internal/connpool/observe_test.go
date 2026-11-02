package connpool

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

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
