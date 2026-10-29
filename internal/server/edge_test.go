package server

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/gofiber/fiber/v2"
)

func okStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range []string{
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":1}}}`,
	} {
		_, _ = w.Write([]byte("data: " + e + "\n\n"))
	}
}

// A gateway error before any content is asked again; the client sees the
// answer of the second try.
func TestGatewayErrorIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, _, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("upstream connect error or disconnect/reset before headers. reset reason: connection termination"))
			return
		}
		okStream(w)
	}), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q", status, body)
	}
	if calls.Load() != 2 {
		t.Errorf("backend calls = %d, want 2", calls.Load())
	}
	for _, want := range []string{"upstream retry 1/2 in 1ms [codex] ", "HTTP 503: upstream connect error or disconnect/reset"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q: %q", want, log)
		}
	}
}

// After the retries the gateway's error goes to the client as it is, and the
// log shows the title of an HTML error page, not its markup.
func TestGatewayErrorGivesUpAfterTheRetries(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, _, access, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`<html><head><style>body{font-family:Arial}</style><title>chatgpt.com | 504: Gateway time-out</title></head><body></body></html>`))
	}), time.Minute)
	if status != fiber.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", status)
	}
	if calls.Load() != 3 {
		t.Errorf("backend calls = %d, want 3 (the request and two retries)", calls.Load())
	}
	if !strings.Contains(access, " 504 - ") || !strings.Contains(access, "html error page: chatgpt.com | 504: Gateway time-out") || strings.Contains(access, "font-family") {
		t.Errorf("access log = %q", access)
	}
	if !strings.Contains(log, "upstream retry 1/2") || !strings.Contains(log, "upstream retry 2/2") {
		t.Errorf("log = %q", log)
	}
}

// Only gateway errors are asked again: a 500 is the backend's own answer.
func TestOtherServerErrorsAreNotAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, _, _, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}), time.Minute)
	if status != fiber.StatusInternalServerError || calls.Load() != 1 || strings.Contains(log, "upstream retry") {
		t.Errorf("status %d after %d calls, log %q", status, calls.Load(), log)
	}
}

func TestEdgeError(t *testing.T) {
	for status, want := range map[int]bool{200: false, 429: false, 500: false, 501: false, 502: true, 503: true, 504: true} {
		if got := edgeError(status); got != want {
			t.Errorf("edgeError(%d) = %v, want %v", status, got, want)
		}
	}
}

// The log line says what went on the wire: the compressed size next to the
// request's, and how long the writing took.
func TestSummaryShowsTheCompressedSize(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &upstreamTrace{start: start, wire: &codex.Wire{Raw: 6 << 20, Sent: 1 << 20, Encoding: "zstd"}}
	if got := u.summary(&codex.Translator{}, start.Add(time.Second), 6<<20); !strings.Contains(got, "| request 6.0 MiB (zstd 1.0 MiB)") {
		t.Errorf("summary = %q", got)
	}
	ct := u.clientTrace()
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{})
	u.got, u.wrote = start.Add(time.Second), start.Add(3*time.Second)
	if got := u.summary(&codex.Translator{}, start.Add(5*time.Second), 6<<20); !strings.Contains(got, "| request 6.0 MiB (zstd 1.0 MiB, written in 2s)") {
		t.Errorf("summary = %q", got)
	}
	u.wire = &codex.Wire{Raw: 100, Sent: 100}
	if got := u.summary(&codex.Translator{}, start.Add(5*time.Second), 100); !strings.Contains(got, "| request 100 B (written in 2s)") {
		t.Errorf("plain summary = %q", got)
	}
}

// A call that fails before any reply says how far the request got, so a stuck
// upload can be told from a backend that took the request and said nothing.
func TestSilentBackendIsLoggedWithTheWriteTime(t *testing.T) {
	status, _, access, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(400 * time.Millisecond)
	}), 100*time.Millisecond)
	if status != fiber.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	for _, want := range []string{"timeout awaiting response headers", " | request ", ": written in ", ", no reply for "} {
		if !strings.Contains(access, want) {
			t.Errorf("access log lacks %q: %q", want, access)
		}
	}
}

func TestSentText(t *testing.T) {
	now := time.Unix(1000, 0)
	u := &upstreamTrace{}
	if got := u.sentText(now); got != "" {
		t.Errorf("before any attempt = %q", got)
	}
	u.attempt = now.Add(-20 * time.Second)
	if got := u.sentText(now); got != "no connection after 20s" {
		t.Errorf("no connection = %q", got)
	}
	u.got = now.Add(-18 * time.Second)
	if got := u.sentText(now); got != "not written out, 18s on the connection" {
		t.Errorf("still going out = %q", got)
	}
	u.wrote = now.Add(-10 * time.Second)
	u.wire = &codex.Wire{Raw: 5 << 20, Sent: 2 << 20, Encoding: "zstd"}
	want := "request 5.0 MiB (zstd 2.0 MiB): written in 8s, no reply for 10s"
	if got := u.sentText(now); got != want {
		t.Errorf("written = %q\nwant      %q", got, want)
	}
	u.wire = &codex.Wire{Raw: 100, Sent: 100}
	if got := u.sentText(now); !strings.HasPrefix(got, "request 100 B: written in") {
		t.Errorf("plain body = %q", got)
	}
}

func TestReconnectLineShowsHowFarTheRequestGot(t *testing.T) {
	var b strings.Builder
	logReconnect(&b, "codex", "a -> b", 1, 50*time.Second, "http2: client connection lost", "warm, opened 5s before", "request 4.8 MiB (zstd 2.2 MiB): written in 5.4s, no reply for 44.6s")
	if got := b.String(); !strings.HasSuffix(got, "connection | request 4.8 MiB (zstd 2.2 MiB): written in 5.4s, no reply for 44.6s\n") {
		t.Errorf("line = %q", got)
	}
}
