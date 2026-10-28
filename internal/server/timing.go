package server

import (
	"crypto/tls"
	"fmt"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
)

// A stream is logged as slow when the backend's reply headers take at least
// slowHeaders or the whole reply at least slowTotal.
var (
	slowHeaders = 20 * time.Second
	slowTotal   = 2 * time.Minute
)

// upstreamTrace records where the time of one upstream call goes.
type upstreamTrace struct {
	mu      sync.Mutex
	start   time.Time
	calls   int // upstream calls: >1 after a retry or a token refresh
	reused  bool
	idle    time.Duration // how long a reused connection had been idle
	dns     time.Duration
	connect time.Duration
	tls     time.Duration
	headers time.Time
	warm    bool          // the connection was opened ahead (connpool)
	warmAge time.Duration // how long before
	retry   string        // why the first attempt was sent again
	first   string        // the first attempt's connection: new, warm or reused (idle for)
	retries int           // attempts sent again
	pool    time.Duration // age of the pool the attempt went to
	hasPool bool
	attempt time.Time // start of the last attempt

	// onRetry hears every retry: its number, the error, the time since the start
	// of the call and the connection the failed attempt was on.
	onRetry func(n int, err error, since time.Duration, conn string)

	dnsStart, connStart, tlsStart time.Time
}

func newUpstreamTrace() *upstreamTrace { return &upstreamTrace{start: time.Now()} }

func (u *upstreamTrace) clientTrace() *httptrace.ClientTrace {
	at := func(f func(now time.Time)) {
		u.mu.Lock()
		f(time.Now())
		u.mu.Unlock()
	}
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			// The connection details describe the last attempt.
			at(func(t time.Time) {
				u.calls++
				u.attempt = t
				u.warm, u.reused, u.idle, u.dns, u.connect, u.tls = false, false, 0, 0, 0, 0
			})
		},
		GotConn: func(i httptrace.GotConnInfo) {
			at(func(time.Time) {
				u.reused, u.idle = i.Reused, i.IdleTime
				if u.calls == 1 {
					u.first = u.connKind()
				}
			})
		},
		DNSStart:          func(httptrace.DNSStartInfo) { at(func(t time.Time) { u.dnsStart = t }) },
		DNSDone:           func(httptrace.DNSDoneInfo) { at(func(t time.Time) { u.dns = t.Sub(u.dnsStart) }) },
		ConnectStart:      func(string, string) { at(func(t time.Time) { u.connStart = t }) },
		ConnectDone:       func(string, string, error) { at(func(t time.Time) { u.connect = t.Sub(u.connStart) }) },
		TLSHandshakeStart: func() { at(func(t time.Time) { u.tlsStart = t }) },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			// Summed: with connpool's own TLS dialer the transport reports a
			// second, empty handshake after the real one.
			at(func(t time.Time) { u.tls += t.Sub(u.tlsStart) })
		},
		GotFirstResponseByte: func() { at(func(t time.Time) { u.headers = t }) },
	}
}

// connKind describes the current attempt's connection; the caller holds u.mu.
func (u *upstreamTrace) connKind() string {
	r := func(d time.Duration) string { return d.Round(time.Millisecond).String() }
	switch {
	case u.reused:
		return "reused, idle " + r(u.idle) + u.poolText(", ")
	case u.warm:
		return "warm, opened " + r(u.warmAge) + " before"
	}
	return "new" + u.poolText(", ")
}

// poolText is sep and the pool's age, or nothing when it is not known; the
// caller holds u.mu. A new connection on an old pool means the pool lost its
// earlier one.
func (u *upstreamTrace) poolText(sep string) string {
	if !u.hasPool {
		return ""
	}
	return sep + "pool " + u.pool.Round(time.Second).String()
}

// observer reports the warm connections and retries of connpool.
func (u *upstreamTrace) observer() *connpool.Observer {
	return &connpool.Observer{
		WarmConn: func(age time.Duration) {
			u.mu.Lock()
			u.warm, u.warmAge = true, age
			u.mu.Unlock()
		},
		Pool: func(age time.Duration) {
			u.mu.Lock()
			u.pool, u.hasPool = age, true
			u.mu.Unlock()
		},
		Retry: func(err error) {
			u.mu.Lock()
			u.retry = err.Error()
			u.retries++
			n, since, conn, hook := u.retries, time.Since(u.start), u.connKind(), u.onRetry
			u.mu.Unlock()
			if hook != nil {
				hook(n, err, since, conn)
			}
		},
	}
}

// newConnText describes the last attempt's connection for the request line when
// it was opened for the request (not reused, not warmed up ahead); else "".
func (u *upstreamTrace) newConnText() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.calls == 0 || u.reused || u.warm {
		return ""
	}
	return "conn new (tls " + u.tls.Round(time.Millisecond).String() + u.poolText(", ") + ")"
}

// pingHint marks a "connection lost" failure whose last attempt ended about one
// health-ping limit after it began: Go does not say that its ping closed the
// connection, so the time is the only sign. "" for any other failure.
func (u *upstreamTrace) pingHint(err error, now time.Time) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err == nil || u.attempt.IsZero() || !strings.Contains(err.Error(), "client connection lost") {
		return ""
	}
	limit := connpool.HealthLimit()
	d := now.Sub(u.attempt)
	if d < limit-2*time.Second || d > limit+2*time.Second {
		return ""
	}
	return "(after " + d.Round(100*time.Millisecond).String() + " of silence: the time of a health-ping close, limit " + limit.String() + ")"
}

// slow reports whether a reply that ended at end is worth a log line.
func (u *upstreamTrace) slow(end time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return (!u.headers.IsZero() && u.headers.Sub(u.start) >= slowHeaders) || end.Sub(u.start) >= slowTotal
}

// summary describes the call: the connection, the waits from the start of the
// call, the request size and the tokens.
func (u *upstreamTrace) summary(tr *codex.Translator, end time.Time, bodyBytes int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	r := func(d time.Duration) string { return d.Round(time.Millisecond).String() }
	conn := "conn new (dns " + r(u.dns) + ", connect " + r(u.connect) + ", tls " + r(u.tls) + u.poolText(", ") + ")"
	switch {
	case u.reused:
		conn = "conn reused (idle " + r(u.idle) + u.poolText(", ") + ")"
	case u.warm:
		conn = "conn warm (opened " + r(u.warmAge) + " before)"
	}
	if u.calls > 1 {
		conn += fmt.Sprintf(", %d upstream calls", u.calls)
	}
	if u.retry != "" {
		conn += ", retried after: " + oneLine(u.retry, 200)
		if u.first != "" {
			conn += " (the first try was on a " + u.first + " connection)"
		}
	}
	waits := []string{}
	if !u.headers.IsZero() {
		waits = append(waits, "headers "+r(u.headers.Sub(u.start)))
	}
	if !tr.FirstDelta.IsZero() {
		waits = append(waits, "first token "+r(tr.FirstDelta.Sub(u.start)))
	}
	waits = append(waits, "total "+r(end.Sub(u.start)))
	parts := []string{conn, strings.Join(waits, ", "), "request " + sizeString(bodyBytes)}
	if us := tr.Usage; us != (codex.Usage{}) {
		parts = append(parts, fmt.Sprintf("tokens in %d (cached %d) out %d (reasoning %d)", us.Input, us.Cached, us.Output, us.Reasoning))
	}
	return strings.Join(parts, " | ")
}

func sizeString(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
