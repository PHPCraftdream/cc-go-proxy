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

// warmMaxAge is how long a warm connection may wait for its first request. A
// connection that idles as bare TLS carries no HTTP/2 traffic or pings and gets
// cut (seen: the host's firewall or antivirus aborting it), so a stale one is
// closed and replaced by a fresh dial.
var warmMaxAge = 20 * time.Second

// fresh reports whether the slot holds a connection young enough to be used.
func (w *warmSlot) fresh() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn != nil && time.Since(w.opened) <= warmMaxAge
}

// take returns the warm connection to addr, once, and when it was opened. A
// stale connection is closed and not returned.
func (w *warmSlot) take(addr string) (net.Conn, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn == nil || w.addr != addr {
		return nil, time.Time{}
	}
	c := w.conn
	w.conn = nil
	if time.Since(w.opened) > warmMaxAge {
		_ = c.Close()
		return nil, time.Time{}
	}
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
func installWarmDialer(p *pool, target *url.URL, counts *counters) {
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
		c, err := dialTLS(ctx, netDial, network, addr, cfg, handshake)
		if tc, ok := c.(*tls.Conn); ok && counts != nil {
			counts.dialed(tc.ConnectionState().NegotiatedProtocol)
		}
		return c, err
	}}
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if c, opened := w.take(addr); c != nil {
			if counts != nil {
				counts.warmUsed.Add(1)
			}
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

// warmUp opens the connection of l's next pool p to target and, once it is
// open, switches the lane to p at once: the old pool drains, and the connection
// does not wait for a request as bare TLS.
func (t *Transport) warmUp(l *lane, p *pool, target *url.URL) {
	addr := target.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "443")
	}
	ctx, cancel := context.WithTimeout(context.Background(), warmUpTimeout)
	defer cancel()
	c, err := p.warm.dial(ctx, "tcp", addr)
	t.mu.Lock()
	if l.warming != p { // given up or the lane is gone
		t.mu.Unlock()
		if c != nil {
			_ = c.Close()
		}
		return
	}
	if err != nil {
		t.counts.warmFailed.Add(1)
		p.failed = true
		t.mu.Unlock()
		return
	}
	t.counts.warmOpened.Add(1)
	p.warm.put(addr, c)
	p.ready = true
	now := t.now()
	idle := t.retireLocked(l.cur, now, nil)
	l.cur, l.born, l.warming = p, now, nil
	t.mu.Unlock()
	closeIdle(idle)
}
