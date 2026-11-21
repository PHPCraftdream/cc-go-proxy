package obs

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
)

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
