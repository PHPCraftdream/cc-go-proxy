package connpool

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Observer hears what the Transport did for one request; set it on the
// request's context with WithObserver. Every field is optional.
type Observer struct {
	// WarmConn: the request got a connection opened ahead, age ago.
	WarmConn func(age time.Duration)
	// Retry: the first attempt failed with err and the request is sent again.
	Retry func(err error)
	// Pool: the pool the next attempt goes to was opened age ago. Its
	// connection is at most that old; a new connection on an old pool means
	// the pool's earlier connection was lost.
	Pool func(age time.Duration)

	mu sync.Mutex
	t  *Transport
	p  *pool // the pool of the latest attempt
}

// bind remembers the pool an attempt goes to, for Discard.
func (o *Observer) bind(t *Transport, p *pool) {
	o.mu.Lock()
	o.t, o.p = t, p
	o.mu.Unlock()
}

// Discard drops the pool the latest attempt went to: its lane gets a fresh pool
// (and so a new connection) at once. For a connection the far end's edge turned
// away, whose route is bad: the next request must not go there. A request still
// running on the pool finishes there.
func (o *Observer) Discard() {
	o.mu.Lock()
	t, p := o.t, o.p
	o.mu.Unlock()
	if t != nil {
		t.discard(p)
	}
}

type observerKey struct{}

// WithObserver returns ctx carrying o.
func WithObserver(ctx context.Context, o *Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, o)
}

func observer(ctx context.Context) *Observer {
	o, _ := ctx.Value(observerKey{}).(*Observer)
	return o
}

// Stats counts what a Transport did since the previous Stats call.
type Stats struct {
	Dials          int64 // connections opened by connpool's TLS dialer (warm-ups included)
	H2, HTTP1      int64 // the protocol they agreed on
	Resumed        int64 // of them, TLS sessions resumed from an earlier connection (a short handshake)
	WarmOpened     int64 // warm-ups that connected
	WarmUsed       int64 // warm connections handed to a request
	WarmFailed     int64 // warm-ups that could not connect
	ConnectRetries int64 // requests sent again after their connection could not open
	BreakRetries   int64 // requests sent again after their connection broke before the reply
}

// String is the log form; "" when nothing happened.
func (s Stats) String() string {
	if s == (Stats{}) {
		return ""
	}
	var parts []string
	if s.Dials > 0 {
		resumed := ""
		if s.Resumed > 0 {
			resumed = fmt.Sprintf(", %d resumed", s.Resumed)
		}
		parts = append(parts, fmt.Sprintf("dials %d (h2 %d, http/1.1 %d%s)", s.Dials, s.H2, s.HTTP1, resumed))
	}
	if s.WarmOpened+s.WarmUsed+s.WarmFailed > 0 {
		parts = append(parts, fmt.Sprintf("warm %d opened, %d used, %d failed", s.WarmOpened, s.WarmUsed, s.WarmFailed))
	}
	if s.ConnectRetries+s.BreakRetries > 0 {
		parts = append(parts, fmt.Sprintf("retries %d after connect failures, %d after broken connections", s.ConnectRetries, s.BreakRetries))
	}
	return strings.Join(parts, "; ")
}

type counters struct {
	dials, h2, http1, resumed, warmOpened, warmUsed, warmFailed, connectRetries, breakRetries atomic.Int64
}

func (c *counters) dialed(cs tls.ConnectionState) {
	c.dials.Add(1)
	if cs.DidResume {
		c.resumed.Add(1)
	}
	if cs.NegotiatedProtocol == "h2" {
		c.h2.Add(1)
	} else {
		c.http1.Add(1)
	}
}

// Stats returns the counts since the previous call and starts over.
func (t *Transport) Stats() Stats {
	c := &t.counts
	return Stats{
		Dials: c.dials.Swap(0), H2: c.h2.Swap(0), HTTP1: c.http1.Swap(0), Resumed: c.resumed.Swap(0),
		WarmOpened: c.warmOpened.Swap(0), WarmUsed: c.warmUsed.Swap(0), WarmFailed: c.warmFailed.Swap(0),
		ConnectRetries: c.connectRetries.Swap(0), BreakRetries: c.breakRetries.Swap(0),
	}
}

func (s *Stats) add(o Stats) {
	s.Dials += o.Dials
	s.H2 += o.H2
	s.HTTP1 += o.HTTP1
	s.Resumed += o.Resumed
	s.WarmOpened += o.WarmOpened
	s.WarmUsed += o.WarmUsed
	s.WarmFailed += o.WarmFailed
	s.ConnectRetries += o.ConnectRetries
	s.BreakRetries += o.BreakRetries
}
