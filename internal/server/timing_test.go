package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
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
