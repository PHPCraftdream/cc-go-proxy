package obs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http/httptrace"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	codexws "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws"
	wschain "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws/chain"
	wsconn "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws/conn"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
)

// A stream is logged as slow when the backend's reply headers take at least
// slowHeaders or the whole reply at least slowTotal.
var (
	SlowHeaders = 20 * time.Second
	SlowTotal   = 2 * time.Minute
)

// A big prompt that the backend did not read from its cache is marked: the
// first token then takes much longer. Small prompts are never cached anyway.
const cacheMissMin = 10000

// A prompt read from the cache for less than this share is marked as partial.
const cachePartialPct = 90

// upstreamTrace records where the time of one upstream call goes.
type UpstreamTrace struct {
	Mu               sync.Mutex
	Start            time.Time
	Calls            int // upstream calls: >1 after a retry or a token refresh
	Reused           bool
	Idle             time.Duration // how long a reused connection had been idle
	Dns              time.Duration
	Connect          time.Duration
	Tls              time.Duration
	Headers          time.Time
	Warm             bool          // the connection was opened ahead (connpool)
	NonStream        bool          // the client asked for the reply in one piece
	WarmAge          time.Duration // how long before
	Retry            string        // why the first attempt was sent again
	First            string        // the first attempt's connection: new, warm or reused (idle for)
	Retries          int           // attempts sent again
	Pool             time.Duration // age of the pool the attempt went to
	HasPool          bool
	Attempt          time.Time // start of the last attempt
	Got              time.Time // the last attempt got its connection
	Wrote            time.Time // the last attempt's request was written out
	Transport        string
	WsConn           string
	HttpNote         string      // conversation and size of a request sent over HTTP that could have used the websocket
	Conv             string      // websocket conversation tag (wsConvTag), "" over HTTP
	Wire             *codex.Wire // what went on the wire: compressed or not, how big
	Routing          *codex.Routing
	NewTurn          bool
	Read             time.Time // when the last byte of the reply came in (stamp)
	Prompt           string    // the request's fingerprint and what changed since the key's last one (promptText)
	Fp               codex.Fingerprint
	PromptGeneration uint64
	Prev             prevInfo // the key's last request: when it ran, the data centre of its reply
	Edge             string   // data centre of this reply (Cf-Ray)

	// onRetry hears every retry: its number, the error, the time since the start
	// of the call and the connection the failed attempt was on.
	// sent is sentText of the failed attempt.
	OnRetry func(n int, err error, since time.Duration, conn, sent string)

	DnsStart, connStart, tlsStart time.Time
}

func NewUpstreamTrace() *UpstreamTrace { return &UpstreamTrace{Start: time.Now()} }

// traceCall puts a new trace on ctx: the transport's hooks and connpool's
// observer, so the call can tell which connection it used.
func TraceCall(ctx context.Context) (context.Context, *UpstreamTrace) {
	u := NewUpstreamTrace()
	return connpool.WithObserver(httptrace.WithClientTrace(ctx, u.ClientTrace()), u.Observer()), u
}

func (u *UpstreamTrace) ClientTrace() *httptrace.ClientTrace {
	at := func(f func(now time.Time)) {
		u.Mu.Lock()
		f(time.Now())
		u.Mu.Unlock()
	}
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			// The connection details describe the last attempt.
			at(func(t time.Time) {
				u.Calls++
				u.Attempt = t
				u.Got, u.Wrote, u.Read = time.Time{}, time.Time{}, time.Time{}
				u.Warm, u.Reused, u.Idle, u.Dns, u.Connect, u.Tls = false, false, 0, 0, 0, 0
			})
		},
		GotConn: func(i httptrace.GotConnInfo) {
			at(func(t time.Time) {
				u.Got = t
				u.Reused, u.Idle = i.Reused, i.IdleTime
				if u.Calls == 1 {
					u.First = u.connKind()
				}
			})
		},
		DNSStart:          func(httptrace.DNSStartInfo) { at(func(t time.Time) { u.DnsStart = t }) },
		DNSDone:           func(httptrace.DNSDoneInfo) { at(func(t time.Time) { u.Dns = t.Sub(u.DnsStart) }) },
		ConnectStart:      func(string, string) { at(func(t time.Time) { u.connStart = t }) },
		ConnectDone:       func(string, string, error) { at(func(t time.Time) { u.Connect = t.Sub(u.connStart) }) },
		TLSHandshakeStart: func() { at(func(t time.Time) { u.tlsStart = t }) },
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			// Summed: with connpool's own TLS dialer the transport reports a
			// second, empty handshake after the real one.
			at(func(t time.Time) { u.Tls += t.Sub(u.tlsStart) })
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { at(func(t time.Time) { u.Wrote = t }) },
		GotFirstResponseByte: func() { at(func(t time.Time) { u.Headers = t }) },
	}
}

// connKind describes the current attempt's connection; the caller holds u.Mu.
func (u *UpstreamTrace) connKind() string {
	r := func(d time.Duration) string { return d.Round(time.Millisecond).String() }
	switch {
	case u.Reused:
		return "reused, idle " + r(u.Idle) + u.poolText(", ")
	case u.Warm:
		return "warm, opened " + r(u.WarmAge) + " before"
	}
	return "new" + u.poolText(", ")
}

// poolText is sep and the pool's age, or nothing when it is not known; the
// caller holds u.Mu. A new connection on an old pool means the pool lost its
// earlier one.
func (u *UpstreamTrace) poolText(sep string) string {
	if !u.HasPool {
		return ""
	}
	return sep + "pool " + u.Pool.Round(time.Second).String()
}

// observer reports the warm connections and retries of connpool.
func (u *UpstreamTrace) Observer() *connpool.Observer {
	return &connpool.Observer{
		WarmConn: func(age time.Duration) {
			u.Mu.Lock()
			u.Warm, u.WarmAge = true, age
			u.Mu.Unlock()
		},
		Pool: func(age time.Duration) {
			u.Mu.Lock()
			u.Pool, u.HasPool = age, true
			u.Mu.Unlock()
		},
		Retry: func(err error) {
			u.Mu.Lock()
			u.Retry = err.Error()
			u.Retries++
			n, since, conn, hook := u.Retries, time.Since(u.Start), u.connKind(), u.OnRetry
			sent := u.sentLocked(time.Now())
			u.Mu.Unlock()
			if hook != nil {
				hook(n, err, since, conn, sent)
			}
		},
	}
}

// newConnText describes the last attempt's connection for the request line when
// it was opened for the request (not reused, not warmed up ahead); else "".
func (u *UpstreamTrace) NewConnText() string {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	return shared.JoinNonEmpty(u.nonStreamNote(), u.connTextLocked(), u.HttpNote)
}

func (u *UpstreamTrace) connTextLocked() string {
	if u.WsConn != "" {
		request := ""
		if u.Wire != nil && u.Wire.Raw > 0 {
			request = "request " + SizeString(u.Wire.Raw)
		}
		return shared.JoinNonEmpty(u.Transport, u.WsConn, request)
	}
	if u.Calls == 0 || u.Reused || u.Warm {
		return u.Transport // a fallback note still belongs in the access line
	}
	return shared.JoinNonEmpty(u.Transport, "conn new (tls "+u.Tls.Round(time.Millisecond).String()+u.poolText(", ")+")")
}

// sentText says how far the last attempt got with its request when it failed:
// no connection, request still going out, or written and unanswered. It tells
// a stuck upload from a silent backend. "" before any attempt.
func (u *UpstreamTrace) SentText(now time.Time) string {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	return u.sentLocked(now)
}

func (u *UpstreamTrace) sentLocked(now time.Time) string {
	if u.Attempt.IsZero() {
		return ""
	}
	r := func(d time.Duration) string { return d.Round(100 * time.Millisecond).String() }
	size := ""
	if u.Wire != nil && u.Wire.Raw > 0 {
		size = "request " + SizeString(u.Wire.Raw)
		if u.Wire.Encoding != "" {
			size += " (" + u.Wire.Encoding + " " + SizeString(u.Wire.Sent) + ")"
		}
		size += ": "
	}
	if u.Wire != nil && u.Wire.Encoding == "ws" && u.Wire.Raw > 0 {
		return size + "frame sent, no reply for " + r(now.Sub(u.Attempt))
	}
	switch {
	case u.Got.IsZero():
		return size + "no connection after " + r(now.Sub(u.Attempt))
	case u.Wrote.IsZero():
		return size + "not written out, " + r(now.Sub(u.Got)) + " on the connection"
	}
	return size + "written in " + r(u.Wrote.Sub(u.Got)) + ", no reply for " + r(now.Sub(u.Wrote))
}

// pingHint marks a "connection lost" failure whose last attempt ended about one
// health-ping limit after it began: Go does not say that its ping closed the
// connection, so the time is the only sign. "" for any other failure.
func (u *UpstreamTrace) PingHint(err error, now time.Time) string {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	if err == nil || u.Attempt.IsZero() || !strings.Contains(err.Error(), "client connection lost") {
		return ""
	}
	limit := connpool.HealthLimit()
	d := now.Sub(u.Attempt)
	if d < limit-2*time.Second || d > limit+2*time.Second {
		return ""
	}
	return "(after " + d.Round(100*time.Millisecond).String() + " of silence: the time of a health-ping close, limit " + limit.String() + ")"
}

// stamp wraps a reply body so the trace knows when its last byte came in.
func (u *UpstreamTrace) Stamp(rc io.ReadCloser) io.ReadCloser { return &StampedBody{rc, u} }

type StampedBody struct {
	io.ReadCloser
	u *UpstreamTrace
}

func (b *StampedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.u.Mu.Lock()
		b.u.Read = time.Now()
		b.u.Mu.Unlock()
	}
	return n, err
}

// silenceText says how long the reply had been silent when it failed at end:
// about one health-ping limit means ccgp's own ping closed a connection the
// network no longer carried; data right up to the failure means a reset.
// "" when no byte of the last reply came in.
func (u *UpstreamTrace) SilenceText(end time.Time) string {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	if u.Read.IsZero() {
		return ""
	}
	d := end.Sub(u.Read)
	text := "silent for " + d.Round(100*time.Millisecond).String()
	if limit := connpool.HealthLimit(); atHealthLimit(d, limit) {
		text += " (the time of a health-ping close, limit " + limit.String() + ")"
	}
	return text
}

// atHealthLimit says d is within two seconds of the health-ping limit.
func atHealthLimit(d, limit time.Duration) bool {
	return d >= limit-2*time.Second && d <= limit+2*time.Second
}

// pingClosed says the reply had been silent for about one health-ping limit when
// it failed at end.
func (u *UpstreamTrace) PingClosed(end time.Time) bool {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	return !u.Read.IsZero() && atHealthLimit(end.Sub(u.Read), connpool.HealthLimit())
}

// linkSample is what the link stats take from a finished reply: the TLS time of
// a connection opened for it (0 = reused or warm), the bytes sent and how long
// the body took to write.
func (u *UpstreamTrace) LinkSample() (tls time.Duration, sent int, wrote time.Duration) {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	if u.Wire != nil && u.Wire.Encoding == "ws" {
		return 0, 0, 0
	}
	if u.Calls > 0 && !u.Reused && !u.Warm {
		tls = u.Tls
	}
	if u.Wire != nil {
		sent = u.Wire.Sent
	}
	if !u.Got.IsZero() && !u.Wrote.IsZero() && u.Wrote.After(u.Got) {
		wrote = u.Wrote.Sub(u.Got)
	}
	return tls, sent, wrote
}

// slow reports whether a reply that ended at end is worth a log line.
func (u *UpstreamTrace) Slow(end time.Time) bool {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	return (!u.Headers.IsZero() && u.Headers.Sub(u.Start) >= SlowHeaders) || end.Sub(u.Start) >= SlowTotal
}

// connLocked describes the last attempt's connection (new, warm or reused) and
// the number of upstream calls; the caller holds u.Mu.
func (u *UpstreamTrace) connLocked() string {
	r := func(d time.Duration) string { return d.Round(time.Millisecond).String() }
	if u.WsConn != "" {
		return u.WsConn
	}
	conn := "conn new (dns " + r(u.Dns) + ", connect " + r(u.Connect) + ", tls " + r(u.Tls) + u.poolText(", ") + ")"
	switch {
	case u.Reused:
		conn = "conn reused (idle " + r(u.Idle) + u.poolText(", ") + ")"
	case u.Warm:
		conn = "conn warm (opened " + r(u.WarmAge) + " before)"
	}
	if u.Calls > 1 {
		conn += fmt.Sprintf(", %d upstream calls", u.Calls)
	}
	return conn
}

// connText is connLocked for a failed request's line: which connection it was
// on tells a bad route from a dead connection. "" before any attempt.
func (u *UpstreamTrace) ConnText() string {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	if u.Calls == 0 {
		return ""
	}
	return shared.JoinNonEmpty(u.nonStreamNote(), u.Transport, u.connLocked())
}

// summary describes the call: the connection, the waits from the start of the
// call, the request size and the tokens.
func (u *UpstreamTrace) Summary(tr *sse.Translator, end time.Time, bodyBytes int) string {
	u.Mu.Lock()
	defer u.Mu.Unlock()
	r := func(d time.Duration) string { return d.Round(time.Millisecond).String() }
	conn := u.connLocked()
	if u.Retry != "" {
		conn += ", retried after: " + shared.OneLine(u.Retry, 200)
		if u.First != "" {
			conn += " (the first try was on a " + u.First + " connection)"
		}
	}
	waits := []string{}
	if !u.Headers.IsZero() {
		waits = append(waits, "headers "+r(u.Headers.Sub(u.Start)))
	}
	if !tr.FirstDelta.IsZero() {
		waits = append(waits, "first token "+r(tr.FirstDelta.Sub(u.Start)))
	}
	waits = append(waits, "total "+r(end.Sub(u.Start))+", ended "+end.Format("15:04:05"))
	req := "request " + SizeString(bodyBytes)
	var notes []string
	if u.Wire != nil && u.Wire.Encoding != "" {
		notes = append(notes, u.Wire.Encoding+" "+SizeString(u.Wire.Sent))
	}
	if u.Wire == nil || u.Wire.Encoding != "ws" {
		if !u.Got.IsZero() && !u.Wrote.IsZero() {
			notes = append(notes, "written in "+r(u.Wrote.Sub(u.Got)))
		}
	}
	if len(notes) > 0 {
		req += " (" + strings.Join(notes, ", ") + ")"
	}
	parts := []string{conn, strings.Join(waits, ", ")}
	if split := phaseText(u.phasesLocked(tr.FirstDelta, end)); split != "" {
		parts = append(parts, split)
	}
	parts = append(parts, req)
	if us := tr.Usage; us != (sse.Usage{}) {
		tokens := fmt.Sprintf("tokens in %d (cached %d) out %d (reasoning %d)", us.Input, us.Cached, us.Output, us.Reasoning)
		if mark := cacheMark(us); mark != "" {
			tokens += " | " + mark
		}
		parts = append(parts, tokens)
	}
	if u.Prompt != "" {
		prompt := u.Prompt
		if e := edgeText(u.Prev.Edge, u.Edge); e != "" {
			prompt += "; " + e
		}
		if note := turnRoutingNote(u.Routing, u.NewTurn); note != "" {
			prompt += "; " + note
		}
		parts = append(parts, "prompt "+prompt)
	}
	if u.Transport != "" {
		parts = append([]string{u.Transport}, parts...)
	}
	return strings.Join(parts, " | ")
}

// phase is one stretch of a call's time; side says whether it is spent on the
// link (connect, upload) or at the backend.
type phase struct {
	name, side string
	d          time.Duration
}

// phaseMin is the shortest stretch listed in the time split.
const phaseMin = 50 * time.Millisecond

// phasesLocked splits the call, from its start to end, into the stretches the
// trace has stamps for: the time before the last try (earlier tries, token
// refresh), connect, upload, wait for the headers, first token, stream. A
// stretch without both stamps is left out; the caller holds u.Mu.
func (u *UpstreamTrace) phasesLocked(firstToken, end time.Time) []phase {
	if u.Wire != nil && u.Wire.Encoding == "ws" {
		return nil
	}
	ps := u.stretchesLocked(firstToken, end)
	for _, p := range ps {
		if p.d > end.Sub(u.Start) {
			return nil // stamps of different clocks: nothing sensible to say
		}
	}
	return ps
}

func (u *UpstreamTrace) stretchesLocked(firstToken, end time.Time) []phase {
	var ps []phase
	add := func(name, side string, from, to time.Time) {
		if from.IsZero() || to.IsZero() || to.Before(from) {
			return
		}
		ps = append(ps, phase{name, side, to.Sub(from)})
	}
	add(preTryName(u.Calls), "link", u.Start, u.Attempt)
	if u.Wire == nil || u.Wire.Encoding != "ws" {
		add("connect", "link", u.Attempt, u.Got)
		add("upload", "link", u.Got, u.Wrote)
	}
	headers := u.Headers
	if headers.Before(u.Attempt) {
		headers = time.Time{} // the reply of an earlier try
	}
	if headers.IsZero() {
		add("no reply", "backend", u.Wrote, end)
		return ps
	}
	waitFrom := u.Wrote
	for _, t := range []time.Time{u.Got, u.Attempt, u.Start} {
		if waitFrom.IsZero() {
			waitFrom = t
		}
	}
	add("headers wait", "backend", waitFrom, headers)
	last := headers
	if !firstToken.IsZero() && !firstToken.Before(headers) {
		add("first token", "backend", headers, firstToken)
		last = firstToken
	}
	add("stream", "backend", last, end)
	return ps
}

// phaseText lists the stretches of at least phaseMin and names the longest with
// its share, so a slow model is told from a slow link: "time connect 12ms,
// upload 8.2s, stream 2m10s (mostly stream at the backend, 79%)". "" when
// nothing is known.
func phaseText(ps []phase) string {
	var total, longest time.Duration
	best := -1
	for i, p := range ps {
		total += p.d
		if best < 0 || p.d > longest {
			best, longest = i, p.d
		}
	}
	if best < 0 || total <= 0 {
		return ""
	}
	var parts []string
	for _, p := range ps {
		if p.d >= phaseMin {
			parts = append(parts, p.name+" "+RoundDur(p.d))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	side := "at the backend"
	if ps[best].side == "link" {
		side = "on the link"
	}
	return fmt.Sprintf("time %s (mostly %s %s, %d%%)", strings.Join(parts, ", "), ps[best].name, side, int(longest*100/total))
}

func turnRoutingNote(routing *codex.Routing, newTurn bool) string {
	if routing == nil {
		return ""
	}
	var notes []string
	if newTurn {
		notes = append(notes, "new turn")
	}
	if routing.Sent() {
		notes = append(notes, "turn state sent")
	}
	// The backend gives a token with the first reply of a turn; a continuation
	// that sent one and got none back is normal, so only "nothing in hand and
	// nothing came back" is worth a note.
	switch {
	case routing.Returned():
		notes = append(notes, "turn state returned")
	case !routing.Sent():
		notes = append(notes, "no turn state returned")
	}
	return strings.Join(notes, "; ")
}

// cacheMark says when a big prompt was not read from the cache: nothing of it
// (cache miss) or less than cachePartialPct percent (cache partial). "" else.
func cacheMark(us sse.Usage) string {
	if us.Input < cacheMissMin {
		return ""
	}
	if us.Cached == 0 {
		return "cache miss"
	}
	if us.Cached*100 < us.Input*cachePartialPct {
		return fmt.Sprintf("cache partial %d%% (%d of %d)", us.Cached*100/us.Input, us.Cached, us.Input)
	}
	return ""
}

// preTryName names the stretch before the last attempt: with one attempt it is
// the request's preparation (token, conversion, compression), with more it is
// the earlier tries and their pauses.
func preTryName(calls int) string {
	if calls > 1 {
		return "before the last try"
	}
	return "prepare"
}

// nonStreamNote marks a request whose reply goes to the client in one piece,
// at its end: nothing reaches the client while the model works.
func (u *UpstreamTrace) nonStreamNote() string {
	if u.NonStream {
		return "non-stream"
	}
	return ""
}

var (
	CodexWSManagerMu sync.Mutex
	CodexWSManager   *codexws.WSManager
	CodexWSFallbacks atomic.Int64
)

func SafeWSReason(reason string) string {
	switch reason {
	case "new connection", "connection broken", "connection too old", "lowered age limit", "no chain", "no response id", "cache key changed", "model changed", "tools changed", "system changed", "reasoning changed", "tool choice changed", "nothing new", "output mismatch",
		"output missing", "assistant text changed", "reasoning dropped", "reasoning differs", "tool call changed",
		"output unreadable", "output not an assistant message", "output reasoning without content", "output tool call without id", "delta copy failed":
		return reason
	}
	if wsDivergence.MatchString(reason) || wsToolChange.MatchString(reason) || wsItemKind.MatchString(reason) {
		return reason
	}
	if strings.HasPrefix(reason, "history diverged at item ") {
		return "history diverged"
	}
	return "full request"
}

// The numbers of a divergence and the name of the tool whose call changed are
// safe to log; nothing else of the history is.
var (
	wsDivergence = regexp.MustCompile(`^history diverged at item \d+( of \d+)?$`)
	wsToolChange = regexp.MustCompile(`^tool call changed \((id|name|arguments)(, ([A-Za-z0-9_.:-]{1,40}|\?)(, was \d{1,9} B, now \d{1,9} B, first \d{1,9} B and last \d{1,9} B equal)?)?\)$`)
	wsItemKind   = regexp.MustCompile(`^((assistant message|tool call) missing \(found [a-z_]{1,32}( message)?\)|new item not sendable \([a-z_]{1,32}( message)?\)|output item not compared \([a-z_]{1,32}\))$`)
)

// wsDivergenceText says where a request's history left the chain it would have
// continued: the item, what it was and is now (kind and size, never content),
// where the chain's reply is in the new history, and the prompt comparison.
func WsDivergenceText(d wschain.WSDivergence, prompt string) string {
	text := fmt.Sprintf("history diverged at item %d of %d (this request has %d): was %s, ", d.At, d.Had, d.Has, shapeText(d.Was))
	if d.Now.Kind == "" {
		text += "now the request ends there"
	} else {
		text += "now " + shapeText(d.Now)
		if diff := d.Now.Size - d.Was.Size; diff > 0 {
			text += " (+" + SizeString(diff) + ")"
		} else if diff < 0 {
			text += " (-" + SizeString(-diff) + ")"
		}
	}
	if d.SameHead >= 0 {
		text += fmt.Sprintf("; first %s and last %s equal, between them %s -> %s", SizeString(d.SameHead), SizeString(d.SameTail),
			SizeString(d.Was.Size-d.SameHead-d.SameTail), SizeString(d.Now.Size-d.SameHead-d.SameTail))
	}
	switch {
	case !d.Reply:
		text += "; the chain has no reply"
	case d.ReplyAt >= 0:
		text += fmt.Sprintf("; the reply chained on is at item %d", d.ReplyAt)
	default:
		text += "; the reply chained on is not in this request"
	}
	if d.Since > 0 {
		text += "; the chained reply ended " + RoundDur(d.Since) + " ago"
	}
	if prompt != "" {
		text += " | " + prompt
	}
	return text
}

func shapeText(s wschain.ItemShape) string {
	if s.Kind == "" {
		return "nothing"
	}
	text := s.Kind + " " + SizeString(s.Size)
	if s.Parts > 0 {
		text += " in " + Count(s.Parts, "part", "parts")
	}
	return text
}

// wsReasonText is the reason for the request line; a replacement at a lowered
// age says which age.
func WsReasonText(info codexws.WSInfo) string {
	reason := SafeWSReason(info.Reason)
	if reason == "lowered age limit" && info.Limit > 0 {
		return "replaced at the lowered age of " + RoundDur(info.Limit)
	}
	return reason
}

func SafeWSFallbackReason(err error) string {
	if err == nil {
		return "connection failed"
	}
	var handshake *wsconn.WSHandshakeError
	if errors.As(err, &handshake) && handshake.Status >= 100 && handshake.Status <= 599 {
		return fmt.Sprintf("handshake HTTP %d", handshake.Status)
	}
	switch {
	case errors.Is(err, codexws.ErrWSBusy):
		return "connection busy"
	case errors.Is(err, codexws.ErrWSToken):
		return "no usable login"
	case errors.Is(err, codexws.ErrWSUnavailable):
		return "unavailable"
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "request deadline exceeded"
	}
	return "connection failed"
}

// count is "1 reply" or "3 replies".
func Count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// roundDur rounds to 100 ms from one second on, else to a millisecond.
func RoundDur(d time.Duration) string {
	if d >= time.Second {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Millisecond).String()
}

func SizeString(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// logReconnect prints the line for a request ccgp sent again because its
// connection failed; the client saw nothing of it unless the retries ran out.
func LogReconnect(w io.Writer, profile, model string, n int, since time.Duration, reason, conn, sent string) {
	if sent != "" {
		sent = " | " + sent
	}
	_, _ = fmt.Fprintf(w, "[%s] upstream reconnect %d after %s [%s] %s: %s | the failed try was on a %s connection%s\n",
		time.Now().Format("15:04:05"), n, since.Round(time.Millisecond), profile, model, shared.OneLine(reason, shared.MaxLogErr), conn, sent)
}
