// Package connpool gives upstream HTTP clients connections that are spread over
// several lanes and renewed before the far end's connection lifetime runs out.
//
// Backends (or proxies on the way) close a connection some time after it was
// opened, killing every request on it at once; over HTTP/2 that is every
// request in flight, and many requests on one connection also slow each other
// down. Transport keeps several lanes, each its own pool, and sends a request to
// the lane with the fewest requests in flight. It works like a flowing pond:
// each lane renews its pool every maxAge, the lanes at staggered times; an old
// pool takes nothing new, its requests finish there and its connections close as
// soon as they are idle. A request whose connection breaks before the reply
// starts is sent once more on a fresh pool. Callers see none of it.
package connpool

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// DefaultMaxAge stays well under the 30 minutes after which connections to
// chatgpt.com were seen to drop, leaving room for long streams.
const DefaultMaxAge = 10 * time.Minute

// DefaultLanes is the number of connections requests to one host are spread over.
const DefaultLanes = 8

// Transport is an http.RoundTripper over lanes of renewed pools.
type Transport struct {
	maxAge time.Duration
	newT   func() *http.Transport
	now    func() time.Time
	lanes  []*lane

	pickMu sync.Mutex
	next   int
}

// lane is one pool and the pools it retired.
type lane struct {
	inflight atomic.Int32

	mu      sync.Mutex
	cur     *http.Transport
	born    time.Time
	offset  time.Duration // staggers the renewals of the lanes
	retired []retiredPool
}

// retiredPool takes no new requests; it is drained until its requests end.
type retiredPool struct {
	t     *http.Transport
	since time.Time
}

// New returns a Transport with the given number of lanes (0: DefaultLanes)
// whose pools come from newT (nil: a clone of http.DefaultTransport) and are
// renewed every maxAge (0: DefaultMaxAge).
func New(maxAge time.Duration, lanes int, newT func() *http.Transport) *Transport {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	if lanes <= 0 {
		lanes = DefaultLanes
	}
	if newT == nil {
		newT = func() *http.Transport { return http.DefaultTransport.(*http.Transport).Clone() }
	}
	t := &Transport{maxAge: maxAge, newT: newT, now: time.Now}
	for i := range lanes {
		t.lanes = append(t.lanes, &lane{offset: maxAge * time.Duration(i) / time.Duration(lanes)})
	}
	return t
}

// Pool returns a pool built the way this Transport builds them (for checks).
func (t *Transport) Pool() *http.Transport { return t.newT() }

// pick takes a request slot on the lane with the fewest requests in flight;
// ties go round-robin.
func (t *Transport) pick() *lane {
	t.pickMu.Lock()
	defer t.pickMu.Unlock()
	t.next++
	var best *lane
	for i := range t.lanes {
		l := t.lanes[(t.next+i)%len(t.lanes)]
		if best == nil || l.inflight.Load() < best.inflight.Load() {
			best = l
		}
	}
	best.inflight.Add(1)
	return best
}

// pool returns the lane's current pool, renewing it when it is too old or when
// it is failed, the pool a request just broke on.
func (t *Transport) pool(l *lane, failed *http.Transport) *http.Transport {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := t.now()
	switch {
	case l.cur == nil:
		// The first pool counts as older by offset, so the lanes renew in turn.
		l.cur, l.born = t.newT(), now.Add(-l.offset)
	case now.Sub(l.born) >= t.maxAge || failed == l.cur:
		l.retired = append(l.retired, retiredPool{l.cur, now})
		l.cur, l.born = t.newT(), now
	}
	t.drainLocked(l, now)
	return l.cur
}

// drainLocked closes the idle connections of the lane's retired pools; busy ones
// keep serving their requests. A pool is forgotten after maxAge: by then
// IdleConnTimeout closes whatever is left.
func (t *Transport) drainLocked(l *lane, now time.Time) {
	keep := l.retired[:0]
	for _, r := range l.retired {
		r.t.CloseIdleConnections()
		if now.Sub(r.since) < t.maxAge {
			keep = append(keep, r)
		}
	}
	clear(l.retired[len(keep):])
	l.retired = keep
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	l := t.pick()
	p := t.pool(l, nil)
	resp, err := p.RoundTrip(req)
	if err != nil && retryable(req, err) {
		again := req.Clone(req.Context())
		if req.GetBody != nil {
			rb, berr := req.GetBody()
			if berr != nil {
				l.inflight.Add(-1)
				return nil, err
			}
			again.Body = rb
		}
		p = t.pool(l, p)
		resp, err = p.RoundTrip(again)
	}
	if err != nil {
		l.inflight.Add(-1)
		return nil, err
	}
	resp.Body = &body{ReadCloser: resp.Body, done: func() { t.release(l, p) }}
	return resp, nil
}

// release ends a request on lane l and pool p; if p is retired, its idle
// connections close, so an old connection goes away with its last request.
func (t *Transport) release(l *lane, p *http.Transport) {
	l.inflight.Add(-1)
	l.mu.Lock()
	retired := p != l.cur
	l.mu.Unlock()
	if retired {
		p.CloseIdleConnections()
	}
}

// CloseIdleConnections closes the idle connections of all pools.
func (t *Transport) CloseIdleConnections() {
	now := t.now()
	for _, l := range t.lanes {
		l.mu.Lock()
		if l.cur != nil {
			l.cur.CloseIdleConnections()
		}
		t.drainLocked(l, now)
		l.mu.Unlock()
	}
}

// body calls done once after the response body is closed.
type body struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *body) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}

// retryable reports whether req failed because its connection broke before a
// reply, and can be sent again.
func retryable(req *http.Request, err error) bool {
	if req.Context().Err() != nil {
		return false
	}
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false // a stalled backend: fail fast, do not wait twice
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"connection reset", "forcibly closed", "connection was aborted", "broken pipe",
		"connection lost", "goaway", "use of closed network connection", "transport connection broken",
		"server closed idle connection", "eof",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
