package connpool

import (
	"context"
	"time"
)

// Observer hears what the Transport did for one request; set it on the
// request's context with WithObserver. Every field is optional.
type Observer struct {
	// WarmConn: the request got a connection opened ahead, age ago.
	WarmConn func(age time.Duration)
	// Retry: the first attempt failed with err and the request is sent again.
	Retry func(err error)
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
