package connpool

import (
	"context"
	"sync"
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
