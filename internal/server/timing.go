package server

import (
	"crypto/tls"
	"fmt"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

// A stream is logged as slow when the backend's reply headers take longer
// than slowHeaders or the whole reply takes longer than slowTotal.
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
		GetConn: func(string) { at(func(time.Time) { u.calls++ }) },
		GotConn: func(i httptrace.GotConnInfo) {
			at(func(time.Time) { u.reused, u.idle = i.Reused, i.IdleTime })
		},
		DNSStart:          func(httptrace.DNSStartInfo) { at(func(t time.Time) { u.dnsStart = t }) },
		DNSDone:           func(httptrace.DNSDoneInfo) { at(func(t time.Time) { u.dns = t.Sub(u.dnsStart) }) },
		ConnectStart:      func(string, string) { at(func(t time.Time) { u.connStart = t }) },
		ConnectDone:       func(string, string, error) { at(func(t time.Time) { u.connect = t.Sub(u.connStart) }) },
		TLSHandshakeStart: func() { at(func(t time.Time) { u.tlsStart = t }) },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			at(func(t time.Time) { u.tls = t.Sub(u.tlsStart) })
		},
		GotFirstResponseByte: func() { at(func(t time.Time) { u.headers = t }) },
	}
}

// slow reports whether a reply that ended at end is worth a log line.
func (u *upstreamTrace) slow(end time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return (!u.headers.IsZero() && u.headers.Sub(u.start) > slowHeaders) || end.Sub(u.start) > slowTotal
}

// summary describes the call: the connection, the waits from the start of the
// call, the request size and the tokens.
func (u *upstreamTrace) summary(tr *codex.Translator, end time.Time, bodyBytes int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	r := func(d time.Duration) string { return d.Round(time.Millisecond).String() }
	conn := "conn new (dns " + r(u.dns) + ", connect " + r(u.connect) + ", tls " + r(u.tls) + ")"
	if u.reused {
		conn = "conn reused (idle " + r(u.idle) + ")"
	}
	if u.calls > 1 {
		conn += fmt.Sprintf(", %d upstream calls", u.calls)
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
