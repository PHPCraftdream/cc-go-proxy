package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

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
		{"codex: stream read error: http2: client connection lost", false, causeLost, true},
		{"codex: stream read error: http2: client connection lost", true, causePing, true},
		{"net/http: timeout awaiting response headers", false, causeNoReply, true},
		{"read tcp 127.0.0.1:1->8.6.112.0:443: wsarecv: An existing connection was forcibly closed by the remote host.", false, causeReset, true},
		{"stream read error: unexpected EOF", false, causeReset, true},
		{"stream ended before completion", false, causeOther, true},
		{"context canceled", false, "", false},
		{"codex: not logged in", false, "", false},
	} {
		got, ok := linkCause(tc.msg, tc.ping)
		if got != tc.cause || ok != tc.wantOK {
			t.Errorf("linkCause(%q, %v) = %q, %v; want %q, %v", tc.msg, tc.ping, got, ok, tc.cause, tc.wantOK)
		}
	}
}

func TestLinkStatsTake(t *testing.T) {
	s := &linkStats{}
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
	if got := s.Take(); got != "1 replies; no broken calls" {
		t.Errorf("quiet window = %q", got)
	}
}

func TestWriteLinkStatsPrintsOnlyWhenSomethingHappened(t *testing.T) {
	old := linkHealth
	linkHealth = &linkStats{}
	t.Cleanup(func() { linkHealth = old })
	var b bytes.Buffer
	now := time.Date(2026, 11, 9, 18, 0, 0, 0, time.UTC)
	writeLinkStats(&b, now)
	if b.Len() != 0 {
		t.Fatalf("an idle window printed %q", b.String())
	}
	noteBreak("context canceled", false, false) // the client left: not the link's doing
	writeLinkStats(&b, now)
	if b.Len() != 0 {
		t.Fatalf("a canceled call counted as a break: %q", b.String())
	}
	noteBreak("http2: client connection lost", false, true)
	writeLinkStats(&b, now)
	if want := "[18:00:00] upstream link [codex]: 0 replies; 1 broken calls (1 sent again by ccgp, 0 reached the client): 1 connection lost\n"; b.String() != want {
		t.Errorf("link line = %q\nwant        %q", b.String(), want)
	}
}

// A reply that ends well, and one that breaks mid-stream, both reach the stats
// the log line is made of.
func TestCodexRepliesFeedTheLinkStats(t *testing.T) {
	old := linkHealth
	linkHealth = &linkStats{}
	t.Cleanup(func() { linkHealth = old })

	status, _, _, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { turnStateOK(w, "") }), time.Minute)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := linkHealth.Take(); !strings.Contains(got, "1 replies") || !strings.Contains(got, "no broken calls") {
		t.Errorf("link stats after a good reply = %q", got)
	}

	fastRetries(t, 0)
	codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
	}), time.Minute)
	if got := linkHealth.Take(); !strings.Contains(got, "1 broken calls (0 sent again by ccgp, 1 reached the client): 1 other") {
		t.Errorf("link stats after a cut stream = %q", got)
	}

	codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		<-r.Context().Done()
	}), 100*time.Millisecond)
	if got := linkHealth.Take(); !strings.Contains(got, "1 no reply in time") {
		t.Errorf("link stats after a silent backend = %q", got)
	}
}

// A call connpool sent again is a break the client never saw: counted as sent
// again, and still logged.
func TestReconnectIsCountedAsASentAgainBreak(t *testing.T) {
	old, oldLog := linkHealth, logOut
	linkHealth = &linkStats{}
	var log bytes.Buffer
	logOut = &log
	t.Cleanup(func() { linkHealth, logOut = old, oldLog })

	u := newUpstreamTrace()
	u.onRetry = u.reconnectHook("codex", "claude-opus-5-5 -> gpt-6.1-sol")
	u.observer().Retry(errors.New(`connect: TLS handshake timeout`))
	if !strings.Contains(log.String(), "upstream reconnect 1 after") {
		t.Errorf("reconnect not logged: %q", log.String())
	}
	if got := linkHealth.Take(); !strings.Contains(got, "1 broken calls (1 sent again by ccgp, 0 reached the client): 1 connect timeout") {
		t.Errorf("link stats = %q", got)
	}
}

func TestPhaseText(t *testing.T) {
	start := time.Unix(1000, 0)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	// A slow link: the body takes most of the time.
	u := &upstreamTrace{start: start, calls: 1, attempt: at(0), got: at(300 * time.Millisecond), wrote: at(22 * time.Second), headers: at(30 * time.Second)}
	got := phaseText(u.phasesLocked(at(32*time.Second), at(40*time.Second)))
	want := "time connect 300ms, upload 21.7s, headers wait 8s, first token 2s, stream 8s (mostly upload on the link, 54%)"
	if got != want {
		t.Errorf("slow link: %q\nwant %q", got, want)
	}

	// A slow model: fast upload, long wait for the first token and the stream.
	u = &upstreamTrace{start: start, calls: 1, attempt: at(0), got: at(10 * time.Millisecond), wrote: at(3 * time.Second), headers: at(7 * time.Second)}
	got = phaseText(u.phasesLocked(at(34*time.Second), at(2*time.Minute+45*time.Second)))
	if !strings.Contains(got, "(mostly stream at the backend, 79%)") {
		t.Errorf("slow model: %q", got)
	}

	// Time before the last try (an earlier failed attempt and its backoff) counts as link.
	u = &upstreamTrace{start: start, calls: 2, attempt: at(30 * time.Second), got: at(31 * time.Second), wrote: at(32 * time.Second), headers: at(33 * time.Second)}
	got = phaseText(u.phasesLocked(time.Time{}, at(34*time.Second)))
	if !strings.Contains(got, "before the last try 30s") || !strings.Contains(got, "(mostly before the last try on the link, 88%)") {
		t.Errorf("earlier tries: %q", got)
	}

	// The reply of an earlier try is not this try's headers.
	u = &upstreamTrace{start: start, calls: 2, attempt: at(20 * time.Second), got: at(21 * time.Second), wrote: at(22 * time.Second), headers: at(5 * time.Second)}
	got = phaseText(u.phasesLocked(time.Time{}, at(30*time.Second)))
	if !strings.Contains(got, "no reply 8s") || strings.Contains(got, "headers wait") {
		t.Errorf("stale headers: %q", got)
	}

	// Stamps that cannot belong to one call (a stretch longer than the call) are dropped.
	u = &upstreamTrace{start: start, calls: 1, attempt: at(24 * time.Hour), got: at(25 * time.Hour)}
	if got := phaseText(u.phasesLocked(time.Time{}, at(time.Second))); got != "" {
		t.Errorf("inconsistent stamps: %q", got)
	}

	// Nothing known, nothing printed.
	if got := phaseText((&upstreamTrace{}).phasesLocked(time.Time{}, time.Time{})); got != "" {
		t.Errorf("no stamps: %q", got)
	}
	if got := phaseText([]phase{{"connect", "link", time.Millisecond}}); got != "" {
		t.Errorf("only sub-minimum stretches: %q", got)
	}
}

// The slow-stream line carries the split.
func TestSlowStreamLineHasTheTimeSplit(t *testing.T) {
	oldSlow := slowTotal
	slowTotal = 0
	t.Cleanup(func() { slowTotal = oldSlow })
	_, _, _, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		turnStateOK(w, "")
	}), time.Minute)
	if !strings.Contains(stream, "slow stream ") || !strings.Contains(stream, "| time ") || !strings.Contains(stream, "(mostly ") {
		t.Errorf("slow line lacks the time split: %q", stream)
	}
}

// A continuation that sent a turn state and got none back is normal and says
// nothing; only a request with no state in hand and none returned is noted.
func TestTurnStateNoteIsQuietOnContinuations(t *testing.T) {
	oldStates, oldWatch, oldStats := turnStates, promptWatch, promptStats
	turnStates, promptWatch, promptStats = &turnStateStore{}, &cacheWatch{}, &cacheStats{}
	t.Cleanup(func() { turnStates, promptWatch, promptStats = oldStates, oldWatch, oldStats })
	oldBody, oldSlowTotal := codexTestBody, slowTotal
	codexTestBody = ""
	slowTotal = 0
	t.Cleanup(func() { codexTestBody, slowTotal = oldBody, oldSlowTotal })

	// A continuation with nothing in hand and nothing returned.
	_, _, _, stream := callTurnState(t, turnStateContinuationBody("quiet"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { turnStateOK(w, "") }))
	if !strings.Contains(stream, "no turn state returned") || strings.Contains(stream, "turn state sent") {
		t.Errorf("no state at all: %q", stream)
	}

	// The turn's first reply gives a token ...
	_, _, _, stream = callTurnState(t, turnStateUserBody("quiet", `""`, "null"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { turnStateOK(w, turnStateA) }))
	if !strings.Contains(stream, "turn state returned") || strings.Contains(stream, "no turn state returned") {
		t.Errorf("first reply: %q", stream)
	}
	// ... a later step sends it, gets none back, and that is not worth a warning.
	_, _, _, stream = callTurnState(t, turnStateContinuationBody("quiet"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { turnStateOK(w, "") }))
	if !strings.Contains(stream, "turn state sent") || strings.Contains(stream, "no turn state returned") || strings.Contains(stream, "turn state returned") {
		t.Errorf("continuation that sent the state: %q", stream)
	}
}
