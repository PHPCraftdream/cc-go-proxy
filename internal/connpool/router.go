package connpool

import (
	"net/http"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// Router is the RoundTripper of an upstream client: it sends each request over
// the Transport of the way out the request's context names (proxydial.ChoiceFor):
// the global proxy, a profile's own, none, or the environment's. Every way out
// has its own lanes and pools, with the same limits.
type Router struct {
	maxAge time.Duration
	newT   func() *http.Transport

	mu            sync.Mutex
	ts            map[string]*Transport
	active, total int
}

// NewRouter returns a Router whose Transports are built like New builds them.
func NewRouter(maxAge time.Duration, active, total int, newT func() *http.Transport) *Router {
	r := &Router{maxAge: maxAge, newT: newT, ts: map[string]*Transport{}}
	r.active, r.total = normalizeLimits(active, total)
	return r
}

func (r *Router) transport(c proxydial.Choice) *Transport {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := c.Key()
	t := r.ts[k]
	if t == nil {
		t = New(r.maxAge, r.active, r.total, r.newT)
		t.choice = c
		r.ts[k] = t
	}
	return t
}

// RoundTrip implements http.RoundTripper.
func (r *Router) RoundTrip(req *http.Request) (*http.Response, error) {
	return r.transport(proxydial.ChoiceFor(req.Context())).RoundTrip(req)
}

// SetLimits sets the lane limits of every way out, present and future.
func (r *Router) SetLimits(active, total int) {
	r.mu.Lock()
	r.active, r.total = normalizeLimits(active, total)
	ts := make([]*Transport, 0, len(r.ts))
	for _, t := range r.ts {
		ts = append(ts, t)
	}
	a, n := r.active, r.total
	r.mu.Unlock()
	for _, t := range ts {
		t.SetLimits(a, n)
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
