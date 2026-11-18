package obs

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
)

func TestCacheWatchMarksRunningHistoryBranches(t *testing.T) {
	tests := []struct {
		name       string
		prev, cur  []uint64
		complete   bool
		wantBranch bool
	}{
		{"active mismatch", []uint64{1, 2, 3}, []uint64{1, 9, 3}, false, true},
		{"identical", []uint64{1, 2}, []uint64{1, 2}, false, false},
		{"extension", []uint64{1, 2}, []uint64{1, 2, 3}, false, false},
		{"truncation", []uint64{1, 2, 3}, []uint64{1, 2}, false, false},
		{"no previous hashes", nil, []uint64{1}, false, false},
		{"no current hashes", []uint64{1}, nil, false, false},
		{"completed mismatch", []uint64{1, 2}, []uint64{1, 9}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &CacheWatch{}
			f := fp("branch-key", "s", "t", len(tt.prev))
			prev := w.See(f, tt.prev)
			if tt.complete {
				w.Finish(f.Key, prev.Generation)
			}
			current := fp(f.Key, "s", "t", len(tt.cur))
			reg := w.See(current, tt.cur)
			if reg.branch != tt.wantBranch {
				t.Errorf("branch = %t, want %t", reg.branch, tt.wantBranch)
			}
			text := PromptText(current, reg, time.Now())
			marker := "branch: history diverges from the last request while it is running"
			if strings.Contains(text, marker) != tt.wantBranch {
				t.Errorf("formatted branch marker in %q, want present=%t", text, tt.wantBranch)
			}
		})
	}
}

func TestDayWriterMarksEachDay(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 1, 23, 59, 0, 0, time.UTC)
	d := &dayWriter{w: &buf, now: func() time.Time { return now }}

	write := func(s string) {
		t.Helper()
		if n, err := d.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	write("[23:59:00] a\n")
	write("[23:59:30] b\n")
	now = now.Add(2 * time.Minute) // 00:01 on the 2nd
	write("[00:01:00] c\n")
	write("[00:02:00] d\n")

	want := "--- 2026-11-01 ---\n[23:59:00] a\n[23:59:30] b\n--- 2026-11-02 ---\n[00:01:00] c\n[00:02:00] d\n"
	if got := buf.String(); got != want {
		t.Errorf("log:\n%s\nwant:\n%s", got, want)
	}
}

// A line stamped later than the clock is of a request that began before midnight:
// it stays above the new day's mark, which waits for a line of the new day (or one
// without a stamp).
func TestDayWriterKeepsALateStampedLineUnderTheOldDay(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 1, 23, 59, 0, 0, time.UTC)
	d := &dayWriter{w: &buf, now: func() time.Time { return now }}
	write := func(s string) {
		t.Helper()
		if _, err := d.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	write("[23:59:00] a\n")
	now = time.Date(2026, 11, 2, 0, 0, 5, 0, time.UTC)
	write("[23:59:57] b\n") // began yesterday, written after midnight
	write("[00:00:01] c\n") // began today
	now = time.Date(2026, 11, 3, 0, 0, 2, 0, time.UTC)
	write("✅ banner\n") // no stamp: written now

	want := "--- 2026-11-01 ---\n[23:59:00] a\n[23:59:57] b\n--- 2026-11-02 ---\n[00:00:01] c\n--- 2026-11-03 ---\n✅ banner\n"
	if got := buf.String(); got != want {
		t.Errorf("log:\n%s\nwant:\n%s", got, want)
	}
}

func TestBeganEarlier(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 5, 0, 0, time.UTC)
	for line, want := range map[string]bool{
		"[23:59:57] 200 - x\n": true,
		"[00:05:01] x\n":       true,
		"[00:05:00] x\n":       false,
		"[00:00:01] x\n":       false,
		"[25:00:00] x\n":       false,
		"[ab:cd:ef] x\n":       false,
		"no stamp\n":           false,
		"[1:2:3]\n":            false,
		"":                     false,
	} {
		if got := beganEarlier([]byte(line), now); got != want {
			t.Errorf("beganEarlier(%q) = %v, want %v", line, got, want)
		}
	}
}

// The mark opens a process's log even before its first line.
func TestNewDayWriterMarksAtOnce(t *testing.T) {
	var buf bytes.Buffer
	d := NewDayWriter(&buf)
	if got := buf.String(); !strings.HasPrefix(got, "--- ") || !strings.HasSuffix(got, " ---\n") || strings.Count(got, "\n") != 1 {
		t.Fatalf("log = %q, want one date line", got)
	}
	_, _ = d.Write([]byte("x\n"))
	if strings.Count(buf.String(), "---") != 2 {
		t.Errorf("the same day was marked twice: %q", buf.String())
	}
}

func TestLinkCause(t *testing.T) {
	for _, tc := range []struct {
		msg    string
		ping   bool
		cause  string
		wantOK bool
	}{
		{`Post "https://chatgpt.com/backend-api/codex/responses": connect: TLS handshake timeout`, false, causeConnect, true},
		{"request 3.8 MiB: no connection after 10s", false, causeConnect, true},
		{"dial tcp 1.2.3.4:443: i/o timeout", false, causeConnect, true},
		{"codex: stream read error: http2: client connection lost", false, CauseLost, true},
		{"codex: stream read error: http2: client connection lost", true, causePing, true},
		{"net/http: timeout awaiting response headers", false, causeNoReply, true},
		{"read tcp 127.0.0.1:1->8.6.112.0:443: wsarecv: An existing connection was forcibly closed by the remote host.", false, causeReset, true},
		{"stream read error: unexpected EOF", false, causeReset, true},
		{"stream ended before completion", false, causeOther, true},
		{"context canceled", false, "", false},
		{"codex: not logged in", false, "", false},
	} {
		got, ok := LinkCause(tc.msg, tc.ping)
		if got != tc.cause || ok != tc.wantOK {
			t.Errorf("linkCause(%q, %v) = %q, %v; want %q, %v", tc.msg, tc.ping, got, ok, tc.cause, tc.wantOK)
		}
	}
}

func TestLinkStatsTake(t *testing.T) {
	s := &LinkStats{}
	if got := s.Take(); got != "" {
		t.Fatalf("empty stats = %q", got)
	}
	// Three new connections (median is the middle one), a reused one, a warm one.
	s.Reply(900*time.Millisecond, 100, 0)
	s.Reply(2700*time.Millisecond, 3<<20, 3*time.Second) // 1 MiB/s
	s.Reply(1200*time.Millisecond, 1<<20, 4*time.Second) // 256 KiB/s
	s.Reply(0, 5<<10, 20*time.Millisecond)               // small: not an upload sample
	s.Reply(0, 2<<20, 10*time.Millisecond)               // too fast to time: not a sample
	s.Break(causeConnect, true)
	s.Break(causeConnect, true)
	s.Break(causePing, false)
	got := s.Take()
	for _, want := range []string{
		"5 replies",
		"3 new connections, TLS median 1.2s, max 2.7s",
		"upload of 2 big bodies 585 KiB/s overall, slowest 256 KiB/s",
		"3 broken calls (2 sent again by ccgp, 1 reached the client): 2 connect timeout, 1 silence (health-ping close)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("link text %q lacks %q", got, want)
		}
	}
	if again := s.Take(); again != "" {
		t.Errorf("a second Take = %q, want a fresh window", again)
	}
	s.Reply(0, 0, 0)
	if got := s.Take(); got != "1 reply; no broken calls" {
		t.Errorf("quiet window = %q", got)
	}
}

// One of a kind is singular, more is plural.
func TestLinkTextAgreesInNumber(t *testing.T) {
	for n, want := range map[int]string{0: "0 replies", 1: "1 reply", 2: "2 replies"} {
		if got := Count(n, "reply", "replies"); got != want {
			t.Errorf("count(%d) = %q, want %q", n, got, want)
		}
	}
	s := &LinkStats{}
	s.Reply(900*time.Millisecond, 3<<20, 3*time.Second)
	s.Break(CauseLost, false)
	got := s.Take()
	for _, want := range []string{"1 reply;", "1 new connection,", "upload of 1 big body ", "1 broken call ("} {
		if !strings.Contains(got, want) {
			t.Errorf("link text %q lacks %q", got, want)
		}
	}
	for _, bad := range []string{"1 replies", "1 new connections", "1 big bodies", "1 broken calls"} {
		if strings.Contains(got, bad) {
			t.Errorf("link text %q has %q", got, bad)
		}
	}
}

func TestWriteLinkStatsPrintsOnlyWhenSomethingHappened(t *testing.T) {
	old := LinkHealth
	LinkHealth = &LinkStats{}
	t.Cleanup(func() { LinkHealth = old })
	var b bytes.Buffer
	now := time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)
	WriteLinkStats(&b, now)
	if b.Len() != 0 {
		t.Fatalf("an idle window printed %q", b.String())
	}
	NoteBreak("context canceled", false, false) // the client left: not the link's doing
	WriteLinkStats(&b, now)
	if b.Len() != 0 {
		t.Fatalf("a canceled call counted as a break: %q", b.String())
	}
	NoteBreak("http2: client connection lost", false, true)
	WriteLinkStats(&b, now)
	if want := "[18:00:00] upstream link [codex]: 0 replies; 1 broken call (1 sent again by ccgp, 0 reached the client): 1 connection lost\n"; b.String() != want {
		t.Errorf("link line = %q\nwant        %q", b.String(), want)
	}
}

// A call connpool sent again is a break the client never saw: counted as sent
// again, and still logged.
func TestReconnectIsCountedAsASentAgainBreak(t *testing.T) {
	old, oldLog := LinkHealth, shared.LogOut
	LinkHealth = &LinkStats{}
	var log bytes.Buffer
	shared.LogOut = &log
	t.Cleanup(func() { LinkHealth, shared.LogOut = old, oldLog })

	u := NewUpstreamTrace()
	u.OnRetry = u.ReconnectHook("codex", "claude-opus-5-5 -> gpt-6.1-sol")
	u.Observer().Retry(errors.New(`connect: TLS handshake timeout`))
	if !strings.Contains(log.String(), "upstream reconnect 1 after") {
		t.Errorf("reconnect not logged: %q", log.String())
	}
	if got := LinkHealth.Take(); !strings.Contains(got, "1 broken call (1 sent again by ccgp, 0 reached the client): 1 connect timeout") {
		t.Errorf("link stats = %q", got)
	}
}

func TestPhaseText(t *testing.T) {
	start := time.Unix(1000, 0)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	// A slow link: the body takes most of the time.
	u := &UpstreamTrace{Start: start, Calls: 1, Attempt: at(0), Got: at(300 * time.Millisecond), Wrote: at(22 * time.Second), Headers: at(30 * time.Second)}
	got := phaseText(u.phasesLocked(at(32*time.Second), at(40*time.Second)))
	want := "time connect 300ms, upload 21.7s, headers wait 8s, first token 2s, stream 8s (mostly upload on the link, 54%)"
	if got != want {
		t.Errorf("slow link: %q\nwant %q", got, want)
	}

	// A slow model: fast upload, long wait for the first token and the stream.
	u = &UpstreamTrace{Start: start, Calls: 1, Attempt: at(0), Got: at(10 * time.Millisecond), Wrote: at(3 * time.Second), Headers: at(7 * time.Second)}
	got = phaseText(u.phasesLocked(at(34*time.Second), at(2*time.Minute+45*time.Second)))
	if !strings.Contains(got, "(mostly stream at the backend, 79%)") {
		t.Errorf("slow model: %q", got)
	}

	// Time before the last try (an earlier failed attempt and its backoff) counts as link.
	u = &UpstreamTrace{Start: start, Calls: 2, Attempt: at(30 * time.Second), Got: at(31 * time.Second), Wrote: at(32 * time.Second), Headers: at(33 * time.Second)}
	got = phaseText(u.phasesLocked(time.Time{}, at(34*time.Second)))
	if !strings.Contains(got, "before the last try 30s") || !strings.Contains(got, "(mostly before the last try on the link, 88%)") {
		t.Errorf("earlier tries: %q", got)
	}

	// With a single attempt the time before it is the request's preparation, not earlier tries.
	u = &UpstreamTrace{Start: start, Calls: 1, Attempt: at(2 * time.Second), Got: at(2*time.Second + 10*time.Millisecond), Wrote: at(3 * time.Second), Headers: at(4 * time.Second)}
	got = phaseText(u.phasesLocked(time.Time{}, at(5*time.Second)))
	if !strings.Contains(got, "prepare 2s") || strings.Contains(got, "before the last try") || !strings.Contains(got, "(mostly prepare on the link, 40%)") {
		t.Errorf("single attempt: %q", got)
	}

	// The reply of an earlier try is not this try's headers.
	u = &UpstreamTrace{Start: start, Calls: 2, Attempt: at(20 * time.Second), Got: at(21 * time.Second), Wrote: at(22 * time.Second), Headers: at(5 * time.Second)}
	got = phaseText(u.phasesLocked(time.Time{}, at(30*time.Second)))
	if !strings.Contains(got, "no reply 8s") || strings.Contains(got, "headers wait") {
		t.Errorf("stale headers: %q", got)
	}

	// Stamps that cannot belong to one call (a stretch longer than the call) are dropped.
	u = &UpstreamTrace{Start: start, Calls: 1, Attempt: at(24 * time.Hour), Got: at(25 * time.Hour)}
	if got := phaseText(u.phasesLocked(time.Time{}, at(time.Second))); got != "" {
		t.Errorf("inconsistent stamps: %q", got)
	}

	// Nothing known, nothing printed.
	if got := phaseText((&UpstreamTrace{}).phasesLocked(time.Time{}, time.Time{})); got != "" {
		t.Errorf("no stamps: %q", got)
	}
	if got := phaseText([]phase{{"connect", "link", time.Millisecond}}); got != "" {
		t.Errorf("only sub-minimum stretches: %q", got)
	}
}

func TestSpeedKindFromTransportNote(t *testing.T) {
	for note, want := range map[string]string{
		"":                                       speedHTTP,
		"fallback to HTTP (handshake HTTP 404)":  speedHTTP,
		"transport websocket (request canceled)": speedHTTP,
		"transport websocket (full: new connection, sent 4.7 MiB)": speedWSFull,
		"transport websocket (full: connection failed)":            speedWSFull,
		"transport websocket (delta: sent 3.0 KiB of 4.7 MiB)":     speedWSDelta,
	} {
		if got := SpeedKind(note); got != want {
			t.Errorf("speedKind(%q) = %q, want %q", note, got, want)
		}
	}
}

func TestSpeedStatsLine(t *testing.T) {
	s := &speedStats{}
	if got := s.Take(); got != "" {
		t.Fatalf("empty stats = %q", got)
	}
	s.Add(speedWSDelta, 900*time.Millisecond, 7*time.Second)
	s.Add(speedHTTP, 3*time.Second, 9*time.Second)
	s.Add(speedHTTP, 1*time.Second, 8*time.Second)
	s.Add(speedHTTP, 2*time.Second, 20*time.Second)
	s.Add(speedWSFull, 0, 5*time.Second) // no content: not a sample
	want := "first token / whole reply, median: http 2s / 9s (3 replies), websocket delta 900ms / 7s (1 reply)"
	if got := s.Take(); got != want {
		t.Fatalf("line = %q\nwant   %q", got, want)
	}
	if got := s.Take(); got != "" {
		t.Fatalf("Take did not start again: %q", got)
	}
}

func TestSpeedStatsBounded(t *testing.T) {
	s := &speedStats{}
	for i := 0; i < speedSamplesMax+10; i++ {
		s.Add(speedHTTP, time.Second, 2*time.Second)
	}
	if n := len(s.first[speedHTTP]); n != speedSamplesMax {
		t.Fatalf("kept %d samples, want %d", n, speedSamplesMax)
	}
}

func TestWriteSpeedStatsSilentWhenEmpty(t *testing.T) {
	old := ReplySpeed
	ReplySpeed = &speedStats{}
	t.Cleanup(func() { ReplySpeed = old })
	var buf bytes.Buffer
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	WriteSpeedStats(&buf, now)
	if buf.Len() != 0 {
		t.Fatalf("wrote %q with nothing to say", buf.String())
	}
	ReplySpeed.Add(speedWSFull, 1500*time.Millisecond, 4*time.Second)
	WriteSpeedStats(&buf, now)
	want := "[12:00:00] upstream speed [codex]: first token / whole reply, median: websocket full 1.5s / 4s (1 reply)\n"
	if buf.String() != want {
		t.Fatalf("line = %q, want %q", buf.String(), want)
	}
}

func TestTraceSummary(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &UpstreamTrace{Start: start, Calls: 2, Reused: true, Idle: 3 * time.Second, Headers: start.Add(25 * time.Second)}
	tr := &sse.Translator{FirstDelta: start.Add(40 * time.Second)}
	end := start.Add(3 * time.Minute)
	got := u.Summary(tr, end, 3<<20)
	want := "conn reused (idle 3s), 2 upstream calls | headers 25s, first token 40s, total 3m0s, ended " + end.Format("15:04:05") +
		" | time headers wait 25s, first token 15s, stream 2m20s (mostly stream at the backend, 77%) | request 3.0 MiB"
	if got != want {
		t.Errorf("summary = %q\nwant      %q", got, want)
	}
	if !u.Slow(start.Add(time.Minute)) {
		t.Error("25s to headers is not slow")
	}
	fast := &UpstreamTrace{Start: start, Headers: start.Add(time.Second)}
	if fast.Slow(start.Add(time.Minute)) || !fast.Slow(start.Add(3*time.Minute)) {
		t.Error("slow() ignores the thresholds")
	}
}

func TestSizeString(t *testing.T) {
	for n, want := range map[int]string{512: "512 B", 2048: "2 KiB", 3 << 20: "3.0 MiB"} {
		if got := SizeString(n); got != want {
			t.Errorf("sizeString(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTraceSummaryWarmConnAndRetry(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &UpstreamTrace{Start: start}
	ct, o := u.ClientTrace(), u.Observer()
	ct.GetConn("h:443")
	o.Retry(errors.New("unexpected EOF"))
	ct.GetConn("h:443") // the retry
	o.WarmConn(45 * time.Second)
	end := start.Add(time.Second)
	got := u.Summary(&sse.Translator{}, end, 10)
	want := "conn warm (opened 45s before), 2 upstream calls, retried after: unexpected EOF | total 1s, ended " + end.Format("15:04:05") + " | request 10 B"
	if got != want {
		t.Errorf("summary = %q\nwant      %q", got, want)
	}
	ct.GetConn("h:443")
	if u.Warm {
		t.Error("a new attempt keeps the warm flag of the last one")
	}
}

// A retry names the kind of connection the first try was on: whether the ones
// that fail are the long-idle ones shows in the log.
func TestTraceSummaryNamesTheFirstConnection(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &UpstreamTrace{Start: start}
	ct, o := u.ClientTrace(), u.Observer()
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{Reused: true, IdleTime: 73 * time.Second})
	o.Retry(errors.New("wsasend: aborted"))
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{})
	end := start.Add(time.Second)
	got := u.Summary(&sse.Translator{}, end, 10)
	want := "conn new (dns 0s, connect 0s, tls 0s), 2 upstream calls, retried after: wsasend: aborted (the first try was on a reused, idle 1m13s connection) | total 1s, ended " + end.Format("15:04:05") + " | request 10 B"
	if got != want {
		t.Errorf("summary = %q\nwant      %q", got, want)
	}

	w := &UpstreamTrace{Start: start}
	ct, o = w.ClientTrace(), w.Observer()
	ct.GetConn("h:443")
	o.WarmConn(40 * time.Second)
	ct.GotConn(httptrace.GotConnInfo{})
	if w.First != "warm, opened 40s before" {
		t.Errorf("first = %q", w.First)
	}
}

// The pool's age and every retry reach the log: a connection that dies young
// or after a long quiet spell is a different problem.
func TestTraceReportsPoolAgeAndEveryRetry(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &UpstreamTrace{Start: start}
	type hear struct {
		n    int
		err  string
		conn string
	}
	var heard []hear
	u.OnRetry = func(n int, err error, _ time.Duration, conn, _ string) {
		heard = append(heard, hear{n, err.Error(), conn})
	}
	ct, o := u.ClientTrace(), u.Observer()

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
	got := u.Summary(&sse.Translator{}, start.Add(time.Second), 10)
	if !strings.Contains(got, "conn new (dns 0s, connect 0s, tls 0s, pool 2s)") || !strings.Contains(got, "retried after: TLS handshake timeout") {
		t.Errorf("summary = %q", got)
	}
	// Without connpool there is no pool age and nothing is invented.
	v := &UpstreamTrace{Start: start}
	if s := v.Summary(&sse.Translator{}, start, 0); strings.Contains(s, "pool") {
		t.Errorf("summary without a pool: %q", s)
	}
}

// Only a connection opened for the request is marked in the request line; the
// share of them is what the warm-up should keep near zero.
func TestNewConnText(t *testing.T) {
	u := &UpstreamTrace{Start: time.Unix(1000, 0)}
	ct, o := u.ClientTrace(), u.Observer()
	if got := u.NewConnText(); got != "" {
		t.Errorf("before any attempt = %q", got)
	}
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{})
	if got := u.NewConnText(); got != "conn new (tls 0s)" {
		t.Errorf("new = %q", got)
	}
	o.Pool(21 * time.Minute)
	if got := u.NewConnText(); got != "conn new (tls 0s, pool 21m0s)" {
		t.Errorf("new on a pool = %q", got)
	}
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{Reused: true, IdleTime: time.Second})
	if got := u.NewConnText(); got != "" {
		t.Errorf("reused = %q", got)
	}
	ct.GetConn("h:443")
	o.WarmConn(10 * time.Second)
	ct.GotConn(httptrace.GotConnInfo{})
	if got := u.NewConnText(); got != "" {
		t.Errorf("warm = %q", got)
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
		u := &UpstreamTrace{}
		u.ClientTrace().GetConn("h:443")
		u.Attempt = now.Add(-c.ran)
		got := u.PingHint(c.err, now)
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
	u := &UpstreamTrace{Start: start}
	ct := u.ClientTrace()
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{Reused: true})
	if got := u.Summary(&sse.Translator{}, start.Add(time.Second), 10); strings.Contains(got, "written in") {
		t.Errorf("summary before the request was written = %q", got)
	}
	ct.WroteRequest(httptrace.WroteRequestInfo{})
	u.Got, u.Wrote = start.Add(time.Second), start.Add(4*time.Second+500*time.Millisecond)
	if got := u.Summary(&sse.Translator{}, start.Add(30*time.Second), 5<<20); !strings.Contains(got, "| request 5.0 MiB (written in 3.5s)") {
		t.Errorf("summary = %q", got)
	}
	ct.GetConn("h:443") // a new attempt forgets the last one's times
	if !u.Got.IsZero() || !u.Wrote.IsZero() {
		t.Error("a new attempt keeps the write times of the last one")
	}
}

// A big prompt with nothing read from the cache is marked; small ones and
// cached ones are not.
func TestSummaryMarksACacheMiss(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, c := range []struct {
		name  string
		usage sse.Usage
		want  bool
	}{
		{"big, nothing cached", sse.Usage{Input: 575000, Cached: 0, Output: 10}, true},
		{"big, mostly cached", sse.Usage{Input: 575000, Cached: 470000, Output: 10}, false},
		{"small, nothing cached", sse.Usage{Input: 900, Output: 10}, false},
		{"at the limit", sse.Usage{Input: cacheMissMin}, true},
	} {
		u := &UpstreamTrace{Start: start}
		got := u.Summary(&sse.Translator{Usage: c.usage}, start.Add(time.Second), 10)
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
		u := &UpstreamTrace{Read: end.Add(-c.silent)}
		got := u.SilenceText(end)
		if !strings.HasPrefix(got, c.want) || strings.Contains(got, "health-ping") != c.ping {
			t.Errorf("%s: %q, want prefix %q, health-ping hint %v", c.name, got, c.want, c.ping)
		}
	}
	if got := (&UpstreamTrace{}).SilenceText(end); got != "" {
		t.Errorf("no byte read = %q, want nothing", got)
	}
}

func TestStampedBodyRecordsTheLastRead(t *testing.T) {
	u := &UpstreamTrace{}
	body := u.Stamp(io.NopCloser(strings.NewReader("abc")))
	if !u.Read.IsZero() {
		t.Fatal("read time set before any read")
	}
	if n, _ := body.Read(make([]byte, 2)); n != 2 || u.Read.IsZero() {
		t.Fatalf("after a read: n=%d, read=%v", n, u.Read)
	}
	u.ClientTrace().GetConn("h:443") // a new attempt forgets the last one's reads
	if !u.Read.IsZero() {
		t.Error("a new attempt keeps the read time of the last one")
	}
}

// A prompt read from the cache for less than 90% is marked with the share; the
// edges of the rule are exact, and small prompts are never marked.
func TestCacheMark(t *testing.T) {
	for _, c := range []struct {
		name  string
		usage sse.Usage
		want  string
	}{
		{"nothing cached", sse.Usage{Input: 500000}, "cache miss"},
		{"64 percent", sse.Usage{Input: 732972, Cached: 469888}, "cache partial 64% (469888 of 732972)"},
		{"just under the share", sse.Usage{Input: 100000, Cached: 89999}, "cache partial 89% (89999 of 100000)"},
		{"exactly the share", sse.Usage{Input: 100000, Cached: 90000}, ""},
		{"all cached", sse.Usage{Input: 500000, Cached: 500000}, ""},
		{"small and partial", sse.Usage{Input: cacheMissMin - 1, Cached: 10}, ""},
		{"at the minimum and partial", sse.Usage{Input: cacheMissMin, Cached: 10}, "cache partial 0% (10 of 10000)"},
		{"no usage", sse.Usage{}, ""},
	} {
		if got := cacheMark(c.usage); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSummaryMarksAPartialCache(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &UpstreamTrace{Start: start}
	got := u.Summary(&sse.Translator{Usage: sse.Usage{Input: 575000, Cached: 470000, Output: 10}}, start.Add(time.Second), 10)
	if !strings.Contains(got, "| tokens in 575000 (cached 470000) out 10 (reasoning 0) | cache partial 81% (470000 of 575000)") || strings.Contains(got, "cache miss") {
		t.Errorf("summary = %q", got)
	}
}
