// Package connpool gives upstream HTTP clients connections that are spread over
// several lanes and renewed before the far end's connection lifetime runs out.
//
// Backends (or proxies on the way) close a connection some time after it was
// opened, killing every request on it at once; over HTTP/2 that is every
// request in flight, and many requests on one connection also slow each other
// down. Transport works like a flowing pond. An active window of lanes, each its
// own pool, takes new requests; a request goes to the lane with the fewest in
// flight. Each lane renews its pool every maxAge, the lanes at staggered times;
// the old pool takes nothing new, its requests finish there and its connections
// close with the last one. The total limit caps active and draining pools
// together: a renewal waits for room, a running request is never cut. A request
// whose connection breaks before the reply starts is sent once more on a fresh
// pool. Callers see none of it.
package connpool

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Defaults: pools take new requests for DefaultMaxAge (well under the 30
// minutes after which connections to chatgpt.com were seen to drop), DefaultActive
// lanes take new requests, DefaultTotal caps active plus draining pools.
const (
	DefaultMaxAge = 10 * time.Minute
	DefaultActive = 7
	DefaultTotal  = 10
)

// forgetAfter drops a draining pool from the count even if a response body
// was never closed, so a leak cannot block renewals for good.
const forgetAfter = 3 * DefaultMaxAge

// Transport is an http.RoundTripper over a window of renewed pools.
type Transport struct {
	maxAge time.Duration
	newT   func() *http.Transport
	now    func() time.Time

	mu       sync.Mutex
	lanes    []*lane // the active window
	draining []*pool // retired, still serving their requests
	total    int
	next     int
}

// lane is one slot of the active window.
type lane struct {
	cur  *pool
	born time.Time
}

// pool is one http.Transport (its own connections) and its requests in flight.
type pool struct {
	t        *http.Transport
	inflight int
	retired  time.Time // zero while in a lane
}

// New returns a Transport whose pools come from newT (nil: a clone of
// http.DefaultTransport) and take new requests for maxAge (0: DefaultMaxAge),
// with active lanes and total pools at most (0: the defaults).
func New(maxAge time.Duration, active, total int, newT func() *http.Transport) *Transport {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	if newT == nil {
		newT = func() *http.Transport { return http.DefaultTransport.(*http.Transport).Clone() }
	}
	t := &Transport{maxAge: maxAge, newT: newT, now: time.Now}
	t.SetLimits(active, total)
	return t
}

// Pool returns a pool built the way this Transport builds them (for checks).
func (t *Transport) Pool() *http.Transport { return t.newT() }

// Limits returns the active window size and the total limit.
func (t *Transport) Limits() (active, total int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.lanes), t.total
}

// SetLimits resizes the active window (0: DefaultActive) and sets the total
// limit (0: DefaultTotal; never below active). Lanes leaving the window drain
// like renewed pools.
func (t *Transport) SetLimits(active, total int) {
	if active <= 0 {
		active = DefaultActive
	}
	if total <= 0 {
		total = DefaultTotal
	}
	total = max(total, active)
	t.mu.Lock()
	var idle []*pool
	now := t.now()
	for len(t.lanes) > active {
		l := t.lanes[len(t.lanes)-1]
		t.lanes = t.lanes[:len(t.lanes)-1]
		if l.cur != nil {
			idle = t.retireLocked(l.cur, now, idle)
		}
	}
	for i := len(t.lanes); i < active; i++ {
		// A new lane counts as older by its offset, so the lanes renew in turn.
		t.lanes = append(t.lanes, &lane{born: now.Add(-t.maxAge * time.Duration(i) / time.Duration(active))})
	}
	t.total = total
	t.mu.Unlock()
	closeIdle(idle)
}

// retireLocked moves p out of the window: a pool with requests keeps draining,
// an idle one goes onto idle to be closed.
func (t *Transport) retireLocked(p *pool, now time.Time, idle []*pool) []*pool {
	p.retired = now
	if p.inflight == 0 {
		return append(idle, p)
	}
	t.draining = append(t.draining, p)
	return idle
}

// sweepLocked forgets draining pools older than forgetAfter.
func (t *Transport) sweepLocked(now time.Time, idle []*pool) []*pool {
	keep := t.draining[:0]
	for _, p := range t.draining {
		if now.Sub(p.retired) >= forgetAfter {
			idle = append(idle, p)
		} else {
			keep = append(keep, p)
		}
	}
	clear(t.draining[len(keep):])
	t.draining = keep
	return idle
}

// acquire takes a request slot. failed is the pool a request just broke on: its
// lane gets a fresh pool even without room. Otherwise the request goes to the
// lane with the fewest in flight (ties round-robin), renewed first when due and
// there is room.
func (t *Transport) acquire(failed *pool) *pool {
	t.mu.Lock()
	now := t.now()
	idle := t.sweepLocked(now, nil)
	var l *lane
	if failed != nil {
		for _, c := range t.lanes {
			if c.cur == failed {
				l = c
				break
			}
		}
	}
	if l != nil {
		idle = t.retireLocked(l.cur, now, idle)
		l.cur, l.born = &pool{t: t.newT()}, now
	} else {
		t.next++
		for i := range t.lanes {
			c := t.lanes[(t.next+i)%len(t.lanes)]
			if l == nil || c.load() < l.load() {
				l = c
			}
		}
		switch {
		case l.cur == nil:
			l.cur = &pool{t: t.newT()}
		case now.Sub(l.born) >= t.maxAge && (l.cur.inflight == 0 || len(t.lanes)+len(t.draining) < t.total):
			idle = t.retireLocked(l.cur, now, idle)
			l.cur, l.born = &pool{t: t.newT()}, now
		}
	}
	p := l.cur
	p.inflight++
	t.mu.Unlock()
	closeIdle(idle)
	return p
}

func (l *lane) load() int {
	if l.cur == nil {
		return 0
	}
	return l.cur.inflight
}

// release ends a request on p; a retired pool closes with its last request.
func (t *Transport) release(p *pool) {
	t.mu.Lock()
	p.inflight--
	done := !p.retired.IsZero() && p.inflight == 0
	if done {
		for i, d := range t.draining {
			if d == p {
				t.draining = append(t.draining[:i], t.draining[i+1:]...)
				break
			}
		}
	}
	t.mu.Unlock()
	if done {
		p.t.CloseIdleConnections()
	}
}

func closeIdle(ps []*pool) {
	for _, p := range ps {
		p.t.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	p := t.acquire(nil)
	resp, err := p.t.RoundTrip(req)
	if err != nil && retryable(req, err) {
		again := req.Clone(req.Context())
		if req.GetBody != nil {
			rb, berr := req.GetBody()
			if berr != nil {
				t.release(p)
				return nil, err
			}
			again.Body = rb
		}
		q := t.acquire(p)
		t.release(p)
		p = q
		resp, err = p.t.RoundTrip(again)
	}
	if err != nil {
		t.release(p)
		return nil, err
	}
	resp.Body = &body{ReadCloser: resp.Body, done: func() { t.release(p) }}
	return resp, nil
}

// CloseIdleConnections closes the idle connections of all pools.
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	var ps []*pool
	for _, l := range t.lanes {
		if l.cur != nil {
			ps = append(ps, l.cur)
		}
	}
	ps = append(ps, t.draining...)
	t.mu.Unlock()
	closeIdle(ps)
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
