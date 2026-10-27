// Package connpool gives upstream HTTP clients connections that are spread over
// several lanes and renewed before the far end's connection lifetime runs out.
//
// Backends (or proxies on the way) close a connection some time after it was
// opened, killing every request on it at once; over HTTP/2 that is every
// request in flight, and many requests on one connection also slow each other
// down. Transport works like a flowing pond. An active window of lanes, each its
// own pool, takes new requests; a request goes to the lane with the fewest in
// flight. Each lane renews its pool every maxAge, the lanes at staggered times:
// the next pool opens its connection in the background (warm.go) and takes over
// once it is up; the old pool takes nothing new, its requests finish there and
// its connections close with the last one. The total limit caps active,
// warming and draining pools together: a renewal waits for room, a running
// request is never cut. A request whose connection could not be opened goes to
// a lane that has a working one; a request whose connection breaks before the
// reply starts is sent once more on a fresh pool. Callers see none of it.
package connpool

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// Defaults: pools take new requests for DefaultMaxAge (well under the 30
// minutes after which connections to chatgpt.com were seen to drop), DefaultActive
// lanes take new requests, DefaultTotal caps active plus warming and draining pools.
const (
	DefaultMaxAge = 10 * time.Minute
	DefaultActive = 7
	DefaultTotal  = 10
)

// forgetAfter drops a draining pool from the count even if a response body
// was never closed, so a leak cannot block renewals for good.
const forgetAfter = 3 * DefaultMaxAge

// A request whose connection could not be opened (it was never sent) is tried
// again on other connections until retryBudget has passed since its start, with
// retryBackoff pauses between the attempts; one whose connection broke before
// the reply is tried again at most maxBreakRetries times; a refused connection
// (nobody listens) is tried once more only. Vars so tests can shorten them.
var (
	retryBudget     = 30 * time.Second
	retryBackoff    = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}
	maxBreakRetries = 2
)

// warmWait is how long a lane keeps its old pool while the next one cannot
// open its connection; then it switches anyway and requests connect themselves.
const warmWait = time.Minute

// Transport is an http.RoundTripper over a window of renewed pools.
type Transport struct {
	maxAge time.Duration
	newT   func() *http.Transport
	now    func() time.Time
	spawn  func(func()) // runs a warm-up; tests run it inline

	mu       sync.Mutex
	lanes    []*lane // the active window
	draining []*pool // retired, still serving their requests
	total    int
	next     int
	target   *url.URL         // scheme and host of the latest https request: what to warm up
	choice   proxydial.Choice // the way out of every pool of this Transport
	counts   counters
}

// lane is one slot of the active window.
type lane struct {
	cur       *pool
	born      time.Time
	warming   *pool // the next pool, opening its connection
	warmSince time.Time
}

// pool is one http.Transport (its own connections) and its requests in flight.
type pool struct {
	t        *http.Transport
	inflight int
	retired  time.Time // zero while in a lane
	created  time.Time
	ready    bool      // has a working connection: served a reply or holds a warm one
	failed   bool      // its warm-up could not connect
	warm     *warmSlot // nil: no warm-up possible (no https target yet, or a proxy)
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
	t := &Transport{maxAge: maxAge, newT: newT, now: time.Now, spawn: func(f func()) { go f() }}
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

// normalizeLimits fills the defaults in and keeps total at or above active.
func normalizeLimits(active, total int) (int, int) {
	if active <= 0 {
		active = DefaultActive
	}
	if total <= 0 {
		total = DefaultTotal
	}
	return active, max(total, active)
}

// SetLimits resizes the active window (0: DefaultActive) and sets the total
// limit (0: DefaultTotal; never below active). Lanes leaving the window drain
// like renewed pools.
func (t *Transport) SetLimits(active, total int) {
	active, total = normalizeLimits(active, total)
	t.mu.Lock()
	var idle []*pool
	now := t.now()
	for len(t.lanes) > active {
		l := t.lanes[len(t.lanes)-1]
		t.lanes = t.lanes[:len(t.lanes)-1]
		if l.cur != nil {
			idle = t.retireLocked(l.cur, now, idle)
		}
		if l.warming != nil {
			idle = append(idle, l.warming)
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

// newPoolLocked builds a pool; with an https target known and no proxy in the
// way it can take a connection opened ahead (see warm.go).
func (t *Transport) newPoolLocked() *pool {
	p := &pool{t: t.newT(), created: t.now()}
	Harden(p.t, healthPing, healthTimeout)
	proxydial.Apply(p.t, t.choice)
	if t.target != nil {
		installWarmDialer(p, t.target, &t.counts)
	}
	return p
}

// poolsLocked counts the pools that hold connections: lanes, warming, draining.
func (t *Transport) poolsLocked() int {
	n := len(t.lanes) + len(t.draining)
	for _, l := range t.lanes {
		if l.warming != nil {
			n++
		}
	}
	return n
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

// renewLocked looks after a lane whose pool is due: it switches to a warm next
// pool, starts warming one when there is room, or, without a warm-up (or after
// warmWait), switches to a fresh pool directly. It returns a warm-up to start.
func (t *Transport) renewLocked(l *lane, now time.Time, idle []*pool) ([]*pool, func()) {
	room := l.cur.inflight == 0 || t.poolsLocked() < t.total
	switch {
	case l.warming != nil && l.warming.ready:
		idle = t.retireLocked(l.cur, now, idle)
		l.cur, l.born, l.warming = l.warming, now, nil
	case l.warming != nil:
		if now.Sub(l.warmSince) >= warmWait {
			idle = append(idle, l.warming)
			l.warming = nil
			idle = t.retireLocked(l.cur, now, idle)
			l.cur, l.born = t.newPoolLocked(), now
		}
	case room:
		next := t.newPoolLocked()
		if next.warm == nil {
			idle = t.retireLocked(l.cur, now, idle)
			l.cur, l.born = next, now
			break
		}
		l.warming, l.warmSince = next, now
		target := *t.target
		return idle, func() { t.warmUp(l, next, &target) }
	}
	return idle, nil
}

// acquire takes a request slot. failed is the pool a request just broke on: its
// lane gets a fresh pool even without room. Otherwise the request goes to the
// lane with the fewest in flight (ties round-robin), renewed first when due.
func (t *Transport) acquire(failed *pool) *pool {
	t.mu.Lock()
	now := t.now()
	idle := t.sweepLocked(now, nil)
	var warm func()
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
		l.cur, l.born = t.newPoolLocked(), now
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
			l.cur = t.newPoolLocked()
		case now.Sub(l.born) >= t.maxAge:
			idle, warm = t.renewLocked(l, now, idle)
		}
	}
	p := l.cur
	p.inflight++
	t.mu.Unlock()
	closeIdle(idle)
	if warm != nil {
		t.spawn(warm)
	}
	return p
}

// acquireLive takes a slot after failed could not open a connection: on the
// least busy lane whose pool has a working one, else on a fresh pool.
func (t *Transport) acquireLive(failed *pool) *pool {
	t.mu.Lock()
	var best *lane
	for _, l := range t.lanes {
		if l.cur != nil && l.cur != failed && l.cur.ready && (best == nil || l.load() < best.load()) {
			best = l
		}
	}
	if best == nil {
		t.mu.Unlock()
		return t.acquire(failed)
	}
	p := best.cur
	p.inflight++
	t.mu.Unlock()
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
		if p.warm != nil {
			p.warm.close()
		}
		p.t.CloseIdleConnections()
	}
}

// learn remembers req's https host as the warm-up target.
func (t *Transport) learn(req *http.Request) {
	if req.URL == nil || req.URL.Scheme != "https" {
		return
	}
	t.mu.Lock()
	if t.target == nil || t.target.Host != req.URL.Host {
		t.target = &url.URL{Scheme: "https", Host: req.URL.Host}
	}
	t.mu.Unlock()
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.learn(req)
	start := time.Now()
	p := t.acquire(nil)
	breaks, refusals := 0, 0
	for attempt := 0; ; attempt++ {
		r := req
		if attempt > 0 {
			r = req.Clone(req.Context())
			if req.GetBody != nil {
				rb, berr := req.GetBody()
				if berr != nil {
					t.release(p)
					return nil, berr
				}
				r.Body = rb
			}
		}
		t.reportPool(req, p)
		resp, err := p.t.RoundTrip(r)
		if err == nil {
			t.mu.Lock()
			p.ready = true
			t.mu.Unlock()
			resp.Body = &body{ReadCloser: resp.Body, done: func() { t.release(p) }}
			return resp, nil
		}
		connect := connectFailure(err)
		refused := connect && connectRefused(err)
		if !retryable(req, err) || (!connect && breaks >= maxBreakRetries) || (refused && refusals >= 1) ||
			time.Since(start) >= retryBudget {
			t.release(p)
			return nil, err
		}
		switch {
		case refused:
			refusals++ // nobody listens there: one more try, not thirty seconds of them
		case !connect:
			breaks++
		}
		if attempt > 0 && !sleep(req.Context(), backoff(attempt-1)) {
			t.release(p)
			return nil, err
		}
		if o := observer(req.Context()); o != nil && o.Retry != nil {
			o.Retry(err)
		}
		var q *pool
		if connect {
			t.counts.connectRetries.Add(1)
			q = t.acquireLive(p)
		} else {
			t.counts.breakRetries.Add(1)
			q = t.acquire(p)
		}
		t.release(p)
		p = q
	}
}

// reportPool tells the request's observer how old p is.
func (t *Transport) reportPool(req *http.Request, p *pool) {
	if o := observer(req.Context()); o != nil && o.Pool != nil {
		o.Pool(t.now().Sub(p.created))
	}
}

// backoff is the pause before retry n (0 is the second retry; the first one
// goes out at once).
func backoff(n int) time.Duration {
	if n >= len(retryBackoff) {
		return retryBackoff[len(retryBackoff)-1]
	}
	return retryBackoff[n]
}

// sleep waits d, false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-ctx.Done():
		return false
	}
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
	for _, p := range ps {
		p.t.CloseIdleConnections()
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

// connectFailure reports whether err means no connection could be opened, so
// the request was never sent.
func connectFailure(err error) bool {
	var ce *connectError
	if errors.As(err, &ce) {
		return true
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "tls handshake timeout") || strings.Contains(msg, "proxyconnect")
}

// connectRefused reports whether err says the far end refused the connection:
// an answer, not a hiccup of the network, so it is not worth a long wait.
func connectRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "actively refused")
}

// retryable reports whether req failed because its connection could not be
// opened or broke before a reply, and can be sent again.
func retryable(req *http.Request, err error) bool {
	if req.Context().Err() != nil {
		return false
	}
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return false
	}
	if connectFailure(err) {
		return true // nothing was sent
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
		"server closed idle connection", "client conn is closed", "client conn not usable", "eof",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
