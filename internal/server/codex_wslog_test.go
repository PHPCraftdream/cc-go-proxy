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
		"lowered age limit":                             "lowered age limit",
		"history diverged at item 412 of 1079":          "history diverged at item 412 of 1079",
		"history diverged at item 0":                    "history diverged at item 0",
		"history diverged at item x of y":               "history diverged",
		"tool call changed (arguments, Read)":           "tool call changed (arguments, Read)",
		"tool call changed (id, mcp__srv__do-it.v2:x)":  "tool call changed (id, mcp__srv__do-it.v2:x)",
		"tool call changed (name, ?)":                   "tool call changed (name, ?)",
		"tool call changed (secret, Read)":              "full request",
		"tool call changed (arguments, rm -rf /)":       "full request",
		"tool call changed (arguments, Bash\nforged)":   "full request",
		"something the backend said, with a token abc1": "full request",
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
	if want := "12 delta, 3 full, 1 busy, 0 fallbacks, 2 dials; sent 41 KiB"; plain != want {
		t.Errorf("plain = %q, want %q", plain, want)
	}
	full := wsStatsText(codex.WSStats{Delta: 1, Full: 1, Dials: 1, SentBytes: 2048, Lost: 1, Early: 2}, 1)
	for _, want := range []string{"1 fallback,", "; 1 connection ended by the backend or the network", "; 2 connections replaced at the lowered age"} {
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
