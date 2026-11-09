package connpool

import (
	"net/http"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

const routeIdleTimeout = 30 * time.Minute

// Router is the RoundTripper of an upstream client: it sends each request over
// the Transport of the way out the request's context names (proxydial.ChoiceFor):
// the global proxy, a profile's own, none, or the environment's. Every way out
// has its own lanes and pools, with the same limits.
type Router struct {
	maxAge time.Duration
	newT   func() *http.Transport
	now    func() time.Time

	mu            sync.Mutex
	ts            map[string]*Transport
	lastUse       map[string]time.Time
	refs          map[string]int
	active, total int
}

// NewRouter returns a Router whose Transports are built like New builds them.
func NewRouter(maxAge time.Duration, active, total int, newT func() *http.Transport) *Router {
	if newT == nil {
		newT = func() *http.Transport { return http.DefaultTransport.(*http.Transport).Clone() }
	}
	r := &Router{
		maxAge: maxAge, newT: newT, now: time.Now,
		ts: map[string]*Transport{}, lastUse: map[string]time.Time{}, refs: map[string]int{},
	}
	r.active, r.total = normalizeLimits(active, total)
	return r
}

func (r *Router) transportLocked(c proxydial.Choice) *Transport {
	k := c.Key()
	t := r.ts[k]
	if t == nil {
		t = New(r.maxAge, r.active, r.total, r.newT)
		t.choice = c
		// Let the router lifecycle and transport pools use the same clock.
		t.now = r.now
		t.lanes = nil
		t.SetLimits(r.active, r.total)
		r.ts[k] = t
		r.lastUse[k] = r.now()
	}
	return t
}

// transport is kept as a helper for checks that need to inspect a route.
func (r *Router) transport(c proxydial.Choice) *Transport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.transportLocked(c)
}

// pruneLocked removes inactive routes. The current default route is retained,
// and a route with an admitted request stays until its response body is closed.
func (r *Router) pruneLocked(now time.Time) []*Transport {
	var retired []*Transport
	defaultKey := (proxydial.Choice{URL: proxydial.Default()}).Key()
	for k, t := range r.ts {
		if k == defaultKey || r.refs[k] != 0 || now.Sub(r.lastUse[k]) < routeIdleTimeout {
			continue
		}
		delete(r.ts, k)
		delete(r.lastUse, k)
		delete(r.refs, k)
		retired = append(retired, t)
	}
	return retired
}

func (r *Router) release(k string) {
	r.mu.Lock()
	if r.refs[k] > 0 {
		r.refs[k]--
		if r.refs[k] == 0 {
			delete(r.refs, k)
		}
		r.lastUse[k] = r.now()
	}
	r.mu.Unlock()
}

// RoundTrip implements http.RoundTripper. Route admission and pruning share the
// router lock so a selected Transport cannot be retired before it acquires a
// request slot.
func (r *Router) RoundTrip(req *http.Request) (*http.Response, error) {
	c := proxydial.ChoiceFor(req.Context())
	k := c.Key()
	r.mu.Lock()
	retired := r.pruneLocked(r.now())
	t := r.transportLocked(c)
	r.refs[k]++
	r.mu.Unlock()
	for _, old := range retired {
		old.retire()
	}

	resp, err := t.RoundTrip(req)
	if err != nil {
		r.release(k)
		return nil, err
	}
	if resp.Body == nil {
		r.release(k)
	} else {
		resp.Body = &body{ReadCloser: resp.Body, done: func() { r.release(k) }}
	}
	return resp, nil
}

// SetLimits sets the lane limits of every way out, present and future. It also
// prunes inactive routes; pruning otherwise happens on RoundTrip, not on a
// background timer.
func (r *Router) SetLimits(active, total int) {
	r.mu.Lock()
	r.active, r.total = normalizeLimits(active, total)
	retired := r.pruneLocked(r.now())
	for _, t := range r.ts {
		t.SetLimits(r.active, r.total)
	}
	r.mu.Unlock()
	for _, t := range retired {
		t.retire()
	}
}

// Limits returns the active window size and the total limit of each way out.
func (r *Router) Limits() (active, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active, r.total
}

// Pool returns a pool built the way the Router's Transports build them (for checks).
func (r *Router) Pool() *http.Transport { return r.newT() }

// Stats returns the counts of all ways out since the previous call.
func (r *Router) Stats() Stats {
	r.mu.Lock()
	ts := make([]*Transport, 0, len(r.ts))
	for _, t := range r.ts {
		ts = append(ts, t)
	}
	r.mu.Unlock()
	var sum Stats
	for _, t := range ts {
		sum.add(t.Stats())
	}
	return sum
}

// CloseIdleConnections closes the idle connections of every way out.
func (r *Router) CloseIdleConnections() {
	r.mu.Lock()
	ts := make([]*Transport, 0, len(r.ts))
	for _, t := range r.ts {
		ts = append(ts, t)
	}
	r.mu.Unlock()
	for _, t := range ts {
		t.CloseIdleConnections()
	}
}
