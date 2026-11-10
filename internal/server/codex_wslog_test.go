package server

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

func TestSafeWSReasonKeepsNumbersAndToolNamesOnly(t *testing.T) {
	for reason, want := range map[string]string{
		"lowered age limit":                    "lowered age limit",
		"history diverged at item 412 of 1079": "history diverged at item 412 of 1079",
		"history diverged at item 0":           "history diverged at item 0",
		"history diverged at item x of y":      "history diverged",
		"tool call changed (arguments, Read)":  "tool call changed (arguments, Read)",
		"tool call changed (arguments, Edit, was 312 B, now 340 B, first 120 B and last 40 B equal)": "tool call changed (arguments, Edit, was 312 B, now 340 B, first 120 B and last 40 B equal)",
		"tool call changed (arguments, Edit, was x B, now 340 B, first 120 B and last 40 B equal)":   "full request",
		"tool call changed (arguments, Edit, was 312 B, now 340 B, first 1 B and last 40 B equal) x": "full request",
		"tool call changed (id, mcp__srv__do-it.v2:x)":                                               "tool call changed (id, mcp__srv__do-it.v2:x)",
		"tool call changed (name, ?)":                    "tool call changed (name, ?)",
		"tool call changed (secret, Read)":               "full request",
		"tool call changed (arguments, rm -rf /)":        "full request",
		"tool call changed (arguments, Bash\nforged)":    "full request",
		"something the backend said, with a token abc1":  "full request",
		"assistant message missing (found user message)": "assistant message missing (found user message)",
		"tool call missing (found reasoning)":            "tool call missing (found reasoning)",
		"new item not sendable (assistant message)":      "new item not sendable (assistant message)",
		"output item not compared (web_search_call)":     "output item not compared (web_search_call)",
		"output item not compared (Secret Text)":         "full request",
		"output unreadable":                              "output unreadable",
		"output tool call without id":                    "output tool call without id",
	} {
		if got := safeWSReason(reason); got != want {
			t.Errorf("safeWSReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestWSReasonTextNamesTheLoweredAge(t *testing.T) {
	info := codex.WSInfo{Reason: "lowered age limit", Limit: 20 * time.Minute}
	if got, want := wsReasonText(info), "replaced at the lowered age of 20m0s"; got != want {
		t.Errorf("wsReasonText = %q, want %q", got, want)
	}
	if got := wsReasonText(codex.WSInfo{Reason: "lowered age limit"}); got != "lowered age limit" {
		t.Errorf("without a known age: %q", got)
	}
	if got := wsReasonText(codex.WSInfo{Reason: "connection broken"}); got != "connection broken" {
		t.Errorf("other reasons are unchanged: %q", got)
	}
}

func TestLogWSLimitLine(t *testing.T) {
	old := logOut
	var buf bytes.Buffer
	logOut = &buf
	t.Cleanup(func() { logOut = old })
	logWSLimit(codex.WSLimit{Age: 20 * time.Minute, Was: 25 * time.Minute, Broke: 12*time.Minute + 5*time.Second, Status: 1001})
	logWSLimit(codex.WSLimit{Age: 15 * time.Minute, Was: 20 * time.Minute, Broke: 11 * time.Minute, Status: -1})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	for i, tail := range []string{
		"upstream websocket [codex]: the backend ended connections before the planned 25m0s (the last at 12m5s, close status 1001); replacing them after 20m0s from now on",
		"upstream websocket [codex]: the backend ended connections before the planned 20m0s (the last at 11m0s, no close frame); replacing them after 15m0s from now on",
	} {
		if !strings.HasSuffix(lines[i], tail) || !strings.HasPrefix(lines[i], "[") {
			t.Errorf("line %d = %q, want it to end with %q", i, lines[i], tail)
		}
	}
}

func TestWSStatsTextCountsBreaksAndEarlyReplacements(t *testing.T) {
	plain := wsStatsText(codex.WSStats{Delta: 12, Full: 3, Busy: 1, Dials: 2, SentBytes: 41 << 10}, 0)
	if want := "12 delta, 3 full, 1 busy, 0 HTTP fallbacks, 2 dials; sent 41 KiB"; plain != want {
		t.Errorf("plain = %q, want %q", plain, want)
	}
	full := wsStatsText(codex.WSStats{Delta: 1, Full: 2, Fallbacks: 1, Dials: 1, SentBytes: 2048, Lost: 1, Early: 2}, 1)
	for _, want := range []string{"2 full (1 where a delta could not continue),", "1 HTTP fallback,", "; 1 connection ended by the backend or the network", "; 2 connections replaced at the lowered age"} {
		if !strings.Contains(full, want) {
			t.Errorf("%q lacks %q", full, want)
		}
	}
}

func TestLinkCauseNamesAWebsocketEndedByTheBackend(t *testing.T) {
	cause, ok := linkCause("stream read error: codex websocket: connection failed", false)
	if !ok || cause != causeWSEnded {
		t.Fatalf("cause = %q, %v", cause, ok)
	}
	// The keepalive's own close is still a lost connection.
	if cause, _ := linkCause("codex websocket: client connection lost", false); cause != causeLost {
		t.Fatalf("keepalive close cause = %q", cause)
	}
	var s linkStats
	s.Break(causeWSEnded, true)
	if text := s.Take(); !strings.Contains(text, "1 broken call (1 sent again by ccgp, 0 reached the client): 1 "+causeWSEnded) {
		t.Fatalf("link line = %q", text)
	}
}

func TestWSDivergenceTextSaysWhatChangedNotTheContent(t *testing.T) {
	edited := codex.WSDivergence{At: 1236, Had: 1237, Has: 1240,
		Was:      codex.ItemShape{Kind: "user message", Size: 3000, Parts: 2},
		Now:      codex.ItemShape{Kind: "user message", Size: 3412, Parts: 3},
		SameHead: 2990, SameTail: 4, Reply: true, ReplyAt: 1237, Since: 41 * time.Second}
	want := "history diverged at item 1236 of 1237 (this request has 1240): was user message 3 KiB in 2 parts, " +
		"now user message 3 KiB in 3 parts (+412 B); first 3 KiB and last 4 B equal, between them 6 B -> 418 B; " +
		"the reply chained on is at item 1237; the chained reply ended 41s ago | prompt key abcd1234"
	if got := wsDivergenceText(edited, "prompt key abcd1234"); got != want {
		t.Errorf("edited: got %q, want %q", got, want)
	}

	ended := codex.WSDivergence{At: 5, Had: 6, Has: 5, Was: codex.ItemShape{Kind: "function_call_output", Size: 10}, SameHead: -1, SameTail: -1, ReplyAt: -1}
	want = "history diverged at item 5 of 6 (this request has 5): was function_call_output 10 B, now the request ends there; the chain has no reply"
	if got := wsDivergenceText(ended, ""); got != want {
		t.Errorf("ended: got %q, want %q", got, want)
	}

	replaced := codex.WSDivergence{At: 0, Had: 3, Has: 3, Was: codex.ItemShape{Kind: "user message", Size: 2048, Parts: 1},
		Now: codex.ItemShape{Kind: "user message", Size: 1024, Parts: 1}, SameHead: -1, SameTail: -1, Reply: true, ReplyAt: -1}
	want = "history diverged at item 0 of 3 (this request has 3): was user message 2 KiB in 1 part, now user message 1 KiB in 1 part (-1 KiB); " +
		"the reply chained on is not in this request"
	if got := wsDivergenceText(replaced, ""); got != want {
		t.Errorf("replaced: got %q, want %q", got, want)
	}
}

func TestWebsocketLineNamesTheConversationAndLogsTheDivergence(t *testing.T) {
	old := logOut
	var buf bytes.Buffer
	logOut = &buf
	t.Cleanup(func() { logOut = old })

	if got := wsConvTag("3fa2c1d4e5"); got != "3fa2c1" {
		t.Errorf("wsConvTag = %q", got)
	}
	trace := newUpstreamTrace()
	trace.wire = &codex.Wire{}
	trace.conv = "3fa2c1"
	trace.prompt = "prompt key abcd1234"
	trace.noteWebsocket(codex.WSInfo{Mode: "full", Reused: true, ConnAge: time.Second, Reason: "history diverged at item 0 of 1",
		Divergence: &codex.WSDivergence{At: 0, Had: 1, Has: 2, Was: codex.ItemShape{Kind: "user message", Size: 50, Parts: 1},
			Now: codex.ItemShape{Kind: "user message", Size: 50, Parts: 1}, SameHead: 40, SameTail: 4, ReplyAt: -1}}, 100)
	if trace.wsConn != "conn reused (age 1s, conv 3fa2c1)" {
		t.Errorf("conn text = %q", trace.wsConn)
	}
	line := buf.String()
	if !strings.Contains(line, "upstream websocket [codex] conv 3fa2c1: history diverged at item 0 of 1 (this request has 2)") ||
		!strings.HasSuffix(line, "| prompt key abcd1234\n") {
		t.Errorf("divergence line = %q", line)
	}

	buf.Reset()
	plain := newUpstreamTrace()
	plain.wire = &codex.Wire{}
	plain.noteWebsocket(codex.WSInfo{Mode: "delta", Reused: true, ConnAge: time.Second}, 100)
	if plain.wsConn != "conn reused (age 1s)" || buf.Len() != 0 {
		t.Errorf("without a conversation: conn %q, log %q", plain.wsConn, buf.String())
	}
}

func TestLogWSLostSaysWhetherTheBreakCountsTowardTheLimit(t *testing.T) {
	old := logOut
	var buf bytes.Buffer
	logOut = &buf
	t.Cleanup(func() { logOut = old })
	logWSLost(codex.WSLost{Age: 12*time.Minute + 5*time.Second, Status: 1001, Planned: 25 * time.Minute, Counts: true})
	logWSLost(codex.WSLost{Age: 2 * time.Minute, Status: -1, Planned: 25 * time.Minute})
	logWSLost(codex.WSLost{Age: 25 * time.Minute, Status: 1006, Planned: 25 * time.Minute})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	for i, tail := range []string{
		"a connection was ended by the backend or the network at 12m5s of age (close status 1001; planned 25m0s): before the planned age, counts toward lowering it",
		"a connection was ended by the backend or the network at 2m0s of age (no close frame; planned 25m0s): too young to say anything about a limit",
		"a connection was ended by the backend or the network at 25m0s of age (close status 1006; planned 25m0s): at or after the planned age",
	} {
		if !strings.HasSuffix(lines[i], tail) {
			t.Errorf("line %d = %q, want it to end with %q", i, lines[i], tail)
		}
	}
}

func TestWSAgentTagIsShortAndOptional(t *testing.T) {
	if got := wsAgentTag("1a2b3c4d"); got != ".1a2b" {
		t.Errorf("wsAgentTag = %q", got)
	}
	if wsAgentTag("") != "" || wsAgentTag("abc") != "" {
		t.Error("a missing or short agent key must give no tag")
	}
}
