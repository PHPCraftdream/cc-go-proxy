package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
)

func TestSlowStreamIsLoggedWithTiming(t *testing.T) {
	old := slowHeaders
	// 0, not 1ns: on Windows the clock moves in ticks, so a local reply can take 0.
	slowHeaders = 0
	t.Cleanup(func() { slowHeaders = old })

	events := []string{
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.output_item.added","item":{"id":"m","type":"message"}}`,
		`{"type":"response.output_text.delta","item_id":"m","delta":"hi"}`,
		`{"type":"response.output_item.done","item":{"id":"m","type":"message"}}`,
		`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":5}}}}`,
	}
	_, body, _, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req_9")
		for _, e := range events {
			_, _ = w.Write([]byte("data: " + e + "\n\n"))
		}
	}), time.Minute)
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("stream did not complete: %q", body)
	}
	for _, want := range []string{
		"slow stream ", "[codex] claude-opus-5-5 -> ", ": conn new (dns ", "| headers ", ", first token ", ", total ",
		"| request ", "| tokens in 100 (cached 40) out 20 (reasoning 5)", "| x-request-id=req_9\n",
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("slow line %q lacks %q", stream, want)
		}
	}
}

func TestTraceSummary(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &upstreamTrace{start: start, calls: 2, reused: true, idle: 3 * time.Second, headers: start.Add(25 * time.Second)}
	tr := &codex.Translator{FirstDelta: start.Add(40 * time.Second)}
	got := u.summary(tr, start.Add(3*time.Minute), 3<<20)
	want := "conn reused (idle 3s), 2 upstream calls | headers 25s, first token 40s, total 3m0s | request 3.0 MiB"
	if got != want {
		t.Errorf("summary = %q\nwant      %q", got, want)
	}
	if !u.slow(start.Add(time.Minute)) {
		t.Error("25s to headers is not slow")
	}
	fast := &upstreamTrace{start: start, headers: start.Add(time.Second)}
	if fast.slow(start.Add(time.Minute)) || !fast.slow(start.Add(3*time.Minute)) {
		t.Error("slow() ignores the thresholds")
	}
}

func TestSizeString(t *testing.T) {
	for n, want := range map[int]string{512: "512 B", 2048: "2 KiB", 3 << 20: "3.0 MiB"} {
		if got := sizeString(n); got != want {
			t.Errorf("sizeString(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTraceSummaryWarmConnAndRetry(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &upstreamTrace{start: start}
	ct, o := u.clientTrace(), u.observer()
	ct.GetConn("h:443")
	o.Retry(errors.New("unexpected EOF"))
	ct.GetConn("h:443") // the retry
	o.WarmConn(45 * time.Second)
	got := u.summary(&codex.Translator{}, start.Add(time.Second), 10)
	want := "conn warm (opened 45s before), 2 upstream calls, retried after: unexpected EOF | total 1s | request 10 B"
	if got != want {
		t.Errorf("summary = %q\nwant      %q", got, want)
	}
	ct.GetConn("h:443")
	if u.warm {
		t.Error("a new attempt keeps the warm flag of the last one")
	}
}

// A retry names the kind of connection the first try was on: whether the ones
// that fail are the long-idle ones shows in the log.
func TestTraceSummaryNamesTheFirstConnection(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &upstreamTrace{start: start}
	ct, o := u.clientTrace(), u.observer()
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{Reused: true, IdleTime: 73 * time.Second})
	o.Retry(errors.New("wsasend: aborted"))
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{})
	got := u.summary(&codex.Translator{}, start.Add(time.Second), 10)
	want := "conn new (dns 0s, connect 0s, tls 0s), 2 upstream calls, retried after: wsasend: aborted (the first try was on a reused, idle 1m13s connection) | total 1s | request 10 B"
	if got != want {
		t.Errorf("summary = %q\nwant      %q", got, want)
	}

	w := &upstreamTrace{start: start}
	ct, o = w.clientTrace(), w.observer()
	ct.GetConn("h:443")
	o.WarmConn(40 * time.Second)
	ct.GotConn(httptrace.GotConnInfo{})
	if w.first != "warm, opened 40s before" {
		t.Errorf("first = %q", w.first)
	}
}

// The pool's age and every retry reach the log: a connection that dies young
// or after a long quiet spell is a different problem.
func TestTraceReportsPoolAgeAndEveryRetry(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &upstreamTrace{start: start}
	type hear struct {
		n    int
		err  string
		conn string
	}
	var heard []hear
	u.onRetry = func(n int, err error, _ time.Duration, conn, _ string) {
		heard = append(heard, hear{n, err.Error(), conn})
	}
	ct, o := u.clientTrace(), u.observer()

	o.Pool(8*time.Minute + 12*time.Second + 300*time.Millisecond)
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{Reused: true, IdleTime: 5 * time.Second})
	o.Retry(errors.New("http2: client conn is closed"))
	o.Pool(2 * time.Second)
	ct.GetConn("h:443") // a new connection: the reuse flag of the last attempt must not stay
	o.Retry(errors.New("TLS handshake timeout"))

	want := []hear{
		{1, "http2: client conn is closed", "reused, idle 5s, pool 8m12s"},
		{2, "TLS handshake timeout", "new, pool 2s"},
	}
	if len(heard) != len(want) || heard[0] != want[0] || heard[1] != want[1] {
		t.Fatalf("heard %+v, want %+v", heard, want)
	}

	ct.GotConn(httptrace.GotConnInfo{})
	got := u.summary(&codex.Translator{}, start.Add(time.Second), 10)
	if !strings.Contains(got, "conn new (dns 0s, connect 0s, tls 0s, pool 2s)") || !strings.Contains(got, "retried after: TLS handshake timeout") {
		t.Errorf("summary = %q", got)
	}
	// Without connpool there is no pool age and nothing is invented.
	v := &upstreamTrace{start: start}
	if s := v.summary(&codex.Translator{}, start, 0); strings.Contains(s, "pool") {
		t.Errorf("summary without a pool: %q", s)
	}
}

// Only a connection opened for the request is marked in the request line; the
// share of them is what the warm-up should keep near zero.
func TestNewConnText(t *testing.T) {
	u := &upstreamTrace{start: time.Unix(1000, 0)}
	ct, o := u.clientTrace(), u.observer()
	if got := u.newConnText(); got != "" {
		t.Errorf("before any attempt = %q", got)
	}
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{})
	if got := u.newConnText(); got != "conn new (tls 0s)" {
		t.Errorf("new = %q", got)
	}
	o.Pool(21 * time.Minute)
	if got := u.newConnText(); got != "conn new (tls 0s, pool 21m0s)" {
		t.Errorf("new on a pool = %q", got)
	}
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{Reused: true, IdleTime: time.Second})
	if got := u.newConnText(); got != "" {
		t.Errorf("reused = %q", got)
	}
	ct.GetConn("h:443")
	o.WarmConn(10 * time.Second)
	ct.GotConn(httptrace.GotConnInfo{})
	if got := u.newConnText(); got != "" {
		t.Errorf("warm = %q", got)
	}
}

func TestAccessLineMarksANewConnection(t *testing.T) {
	_, _, access, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []string{
			`{"type":"response.created","response":{"id":"r"}}`,
			`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":1}}}`,
		} {
			_, _ = w.Write([]byte("data: " + e + "\n\n"))
		}
	}), time.Minute)
	if !strings.Contains(access, " 200 - ") || !strings.Contains(access, " | conn new (tls ") {
		t.Errorf("access log = %q", access)
	}
}

// A "connection lost" after one health-ping limit of silence is named, others not.
func TestPingHint(t *testing.T) {
	now := time.Unix(5000, 0)
	lost := errors.New(`Post "https://x": http2: client connection lost`)
	limit := connpool.HealthLimit()
	for _, c := range []struct {
		name string
		err  error
		ran  time.Duration
		want bool
	}{
		{"at the limit", lost, limit, true},
		{"a second over", lost, limit + time.Second, true},
		{"too early", lost, 10 * time.Second, false},
		{"too late", lost, limit + 10*time.Second, false},
		{"other error", errors.New("unexpected EOF"), limit, false},
		{"no error", nil, limit, false},
	} {
		u := &upstreamTrace{}
		u.clientTrace().GetConn("h:443")
		u.attempt = now.Add(-c.ran)
		got := u.pingHint(c.err, now)
		if (got != "") != c.want {
			t.Errorf("%s: hint = %q, want hint %v", c.name, got, c.want)
		}
		if c.want && !strings.Contains(got, "health-ping") {
			t.Errorf("%s: hint = %q", c.name, got)
		}
	}
}

// The time the request took to leave tells a slow upload from a slow backend.
func TestSummaryShowsHowLongTheRequestTookToWrite(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &upstreamTrace{start: start}
	ct := u.clientTrace()
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{Reused: true})
	if got := u.summary(&codex.Translator{}, start.Add(time.Second), 10); strings.Contains(got, "written in") {
		t.Errorf("summary before the request was written = %q", got)
	}
	ct.WroteRequest(httptrace.WroteRequestInfo{})
	u.got, u.wrote = start.Add(time.Second), start.Add(4*time.Second+500*time.Millisecond)
	if got := u.summary(&codex.Translator{}, start.Add(30*time.Second), 5<<20); !strings.Contains(got, "| request 5.0 MiB (written in 3.5s)") {
		t.Errorf("summary = %q", got)
	}
	ct.GetConn("h:443") // a new attempt forgets the last one's times
	if !u.got.IsZero() || !u.wrote.IsZero() {
		t.Error("a new attempt keeps the write times of the last one")
	}
}

// A big prompt with nothing read from the cache is marked; small ones and
// cached ones are not.
func TestSummaryMarksACacheMiss(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, c := range []struct {
		name  string
		usage codex.Usage
		want  bool
	}{
		{"big, nothing cached", codex.Usage{Input: 575000, Cached: 0, Output: 10}, true},
		{"big, mostly cached", codex.Usage{Input: 575000, Cached: 470000, Output: 10}, false},
		{"small, nothing cached", codex.Usage{Input: 900, Output: 10}, false},
		{"at the limit", codex.Usage{Input: cacheMissMin}, true},
	} {
		u := &upstreamTrace{start: start}
		got := u.summary(&codex.Translator{Usage: c.usage}, start.Add(time.Second), 10)
		if has := strings.Contains(got, "| cache miss"); has != c.want {
			t.Errorf("%s: cache miss marked = %v, want %v: %q", c.name, has, c.want, got)
		}
	}
}

// How long the reply had been silent when it failed tells a health-ping close
// (about one limit of silence) from a reset in the middle of the data.
func TestSilenceText(t *testing.T) {
	end := time.Unix(9000, 0)
	limit := connpool.HealthLimit()
	for _, c := range []struct {
		name   string
		silent time.Duration
		want   string
		ping   bool
	}{
		{"mid data", 300 * time.Millisecond, "silent for 300ms", false},
		{"a long think", 20 * time.Second, "silent for 20s", false},
		{"at the limit", limit, "silent for " + limit.String(), true},
		{"just under", limit - time.Second, "silent for 44s", true},
		{"far over", limit + 10*time.Second, "silent for 55s", false},
	} {
		u := &upstreamTrace{read: end.Add(-c.silent)}
		got := u.silenceText(end)
		if !strings.HasPrefix(got, c.want) || strings.Contains(got, "health-ping") != c.ping {
			t.Errorf("%s: %q, want prefix %q, health-ping hint %v", c.name, got, c.want, c.ping)
		}
	}
	if got := (&upstreamTrace{}).silenceText(end); got != "" {
		t.Errorf("no byte read = %q, want nothing", got)
	}
}

func TestStampedBodyRecordsTheLastRead(t *testing.T) {
	u := &upstreamTrace{}
	body := u.stamp(io.NopCloser(strings.NewReader("abc")))
	if !u.read.IsZero() {
		t.Fatal("read time set before any read")
	}
	if n, _ := body.Read(make([]byte, 2)); n != 2 || u.read.IsZero() {
		t.Fatalf("after a read: n=%d, read=%v", n, u.read)
	}
	u.clientTrace().GetConn("h:443") // a new attempt forgets the last one's reads
	if !u.read.IsZero() {
		t.Error("a new attempt keeps the read time of the last one")
	}
}
