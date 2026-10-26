package connpool

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"
)

// warmUpTimeout bounds one background connection attempt.
const warmUpTimeout = 30 * time.Second

// connectError is a failure to open a connection: the request was not sent.
type connectError struct{ err error }

func (e *connectError) Error() string { return "connect: " + e.err.Error() }
func (e *connectError) Unwrap() error { return e.err }

// warmSlot holds a connection opened ahead of the pool's first request.
type warmSlot struct {
	mu     sync.Mutex
	addr   string
	conn   net.Conn
	opened time.Time
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
}

// take returns the warm connection to addr, once, and when it was opened.
func (w *warmSlot) take(addr string) (net.Conn, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil || w.addr != addr {
		return nil, time.Time{}
	}
	c := w.conn
	w.conn = nil
	return c, w.opened
}

func (w *warmSlot) put(addr string, c net.Conn) {
	w.mu.Lock()
	old := w.conn
	w.addr, w.conn, w.opened = addr, c, time.Now()
	w.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// close drops a warm connection nobody took.
func (w *warmSlot) close() {
	w.mu.Lock()
	c := w.conn
	w.conn = nil
	w.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// installWarmDialer makes p's transport open its TLS connections itself, so a
// connection opened ahead for target can be handed to it. Pools with their own
// TLS dialer or a proxy for target keep the transport's dialing.
func installWarmDialer(p *pool, target *url.URL) {
	tr := p.t
	if tr.DialTLSContext != nil || tr.DialTLS != nil { //nolint:staticcheck // DialTLS still counts
		return
	}
	if tr.Proxy != nil {
		if u, err := tr.Proxy(&http.Request{URL: target}); err != nil || u != nil {
			return
		}
	}
	cfg := &tls.Config{}
	if tr.TLSClientConfig != nil {
		cfg = tr.TLSClientConfig.Clone()
	}
	if len(cfg.NextProtos) == 0 {
		cfg.NextProtos = []string{"http/1.1"}
		if tr.ForceAttemptHTTP2 {
			cfg.NextProtos = []string{"h2", "http/1.1"}
		}
	}
	netDial := tr.DialContext
	if netDial == nil {
		netDial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	handshake := tr.TLSHandshakeTimeout
	w := &warmSlot{dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialTLS(ctx, netDial, network, addr, cfg, handshake)
	}}
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if c, opened := w.take(addr); c != nil {
			if o := observer(ctx); o != nil && o.WarmConn != nil {
				o.WarmConn(time.Since(opened))
			}
			return c, nil
		}
		return w.dial(ctx, network, addr)
	}
	p.warm = w
}

// dialTLS opens a TCP connection and completes the TLS handshake within
// handshake (0: no limit of its own). It reports the handshake to the
// request's httptrace, as the transport does for its own dials.
func dialTLS(ctx context.Context, netDial func(context.Context, string, string) (net.Conn, error),
	network, addr string, cfg *tls.Config, handshake time.Duration) (net.Conn, error) {
	raw, err := netDial(ctx, network, addr)
	if err != nil {
		return nil, &connectError{err}
	}
	c := cfg.Clone()
	if c.ServerName == "" {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			c.ServerName = host
		}
	}
	hctx := ctx
	if handshake > 0 {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, handshake)
		defer cancel()
	}
	tc := tls.Client(raw, c)
	trace := httptrace.ContextClientTrace(ctx)
	if trace != nil && trace.TLSHandshakeStart != nil {
		trace.TLSHandshakeStart()
	}
	err = tc.HandshakeContext(hctx)
	if trace != nil && trace.TLSHandshakeDone != nil {
		trace.TLSHandshakeDone(tc.ConnectionState(), err)
	}
	if err != nil {
		_ = raw.Close()
		if ctx.Err() == nil && errors.Is(hctx.Err(), context.DeadlineExceeded) {
			err = errors.New("TLS handshake timeout")
		}
		return nil, &connectError{err}
	}
	return tc, nil
}

// warmUp opens the connection of l's next pool p to target; the lane switches
// to p on a later request once it is ready.
func (t *Transport) warmUp(l *lane, p *pool, target *url.URL) {
	addr := target.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "443")
	}
	ctx, cancel := context.WithTimeout(context.Background(), warmUpTimeout)
	defer cancel()
	c, err := p.warm.dial(ctx, "tcp", addr)
	t.mu.Lock()
	defer t.mu.Unlock()
	if l.warming != p { // given up or the lane is gone
		if c != nil {
			_ = c.Close()
		}
		return
	}
	if err != nil {
		p.failed = true
		return
	}
	p.warm.put(addr, c)
	p.ready = true
}
