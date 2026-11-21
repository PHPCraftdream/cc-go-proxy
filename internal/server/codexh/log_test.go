package codexh

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/gofiber/fiber/v2"
)

func TestDropShownMessage(t *testing.T) {
	for _, tt := range []struct {
		name, detail, msg, want string
		dropped                 bool
	}{
		{"error event",
			`{"type":"error","error":{"code":"cyber_policy","message":"flagged"}}`, "cyber_policy: flagged",
			`{"type":"error","error":{"code":"cyber_policy","message":"(shown before)"}}`, true},
		{"response.failed, spaces",
			`{"type":"response.failed","response":{"error":{"message" : "boom ’x\""}}}`, "server_error: boom ’x\"",
			`{"type":"response.failed","response":{"error":{"message" : "(shown before)"}}}`, true},
		{"another message is kept",
			`{"error":{"message":"other"}}`, "boom", `{"error":{"message":"other"}}`, false},
		{"a value named message is kept",
			`{"type":"message","error":{"message":"boom"}}`, "boom", `{"type":"message","error":{"message":"(shown before)"}}`, true},
		{"empty message is kept",
			`{"error":{"message":""}}`, "boom", `{"error":{"message":""}}`, false},
		{"not a string",
			`{"message":null,"error":{"message":"boom"}}`, "boom", `{"message":null,"error":{"message":"(shown before)"}}`, true},
		{"broken json", `{"message":"bo`, "boom", `{"message":"bo`, false},
		{"found inside the reason is another text", `{"error":{"message":"a"}}`, "bad", `{"error":{"message":"a"}}`, false},
		{"the end of a word is not the message", `{"error":{"message":"boom"}}`, "kaboom", `{"error":{"message":"boom"}}`, false},
		{"no detail", "", "boom", "", false},
	} {
		got, dropped := shared.DropShownMessage(tt.detail, tt.msg)
		if got != tt.want || dropped != tt.dropped {
			t.Errorf("%s: got %q, %v; want %q, %v", tt.name, got, dropped, tt.want, tt.dropped)
		}
	}
}

// The live cyber_policy refusal: its long message is written once, whole.
func TestStreamFailureWritesARepeatedMessageOnce(t *testing.T) {
	text := "This content was flagged for possible cybersecurity risk. " + strings.Repeat("If this seems wrong, try rephrasing your request. ", 8) + "END"
	ev := `{"type":"error","error":{"type":"invalid_request_error","code":"cyber_policy","message":"` + text + `","param":null}}`
	var b bytes.Buffer
	shared.LogStreamFailure(&b, time.Now(), "stream", "codex", "a -> b", "cyber_policy: "+text, ev, "cf-ray=1")
	got := b.String()
	if n := strings.Count(got, "This content was flagged"); n != 1 {
		t.Errorf("message written %d times: %q", n, got)
	}
	want := ": cyber_policy: " + text + ` | upstream event: {"type":"error","error":{"type":"invalid_request_error","code":"cyber_policy","message":"(shown before)","param":null}} | cf-ray=1` + "\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("line %q\nwant suffix %q", got, want)
	}
}

func TestUpstreamRetryDropsTheShownMessage(t *testing.T) {
	var b bytes.Buffer
	rt.LogUpstreamRetry(&b, "codex", "a -> b", 1, 2, 2*time.Second, "server_is_overloaded: overloaded",
		`{"type":"error","error":{"code":"server_is_overloaded","message":"overloaded"}}`, "cf-ray=1")
	want := `server_is_overloaded: overloaded | upstream event: {"type":"error","error":{"code":"server_is_overloaded","message":"(shown before)"}} | cf-ray=1` + "\n"
	if got := b.String(); !strings.HasSuffix(got, want) {
		t.Errorf("line %q\nwant suffix %q", got, want)
	}
}

// Without a repeat the reason keeps its usual cap.
func TestEventTextCapsAReasonNotRepeated(t *testing.T) {
	long := strings.Repeat("x", shared.MaxLogErr+50)
	if got := shared.EventText(long, `{"error":{"message":"other"}}`); !strings.HasPrefix(got, strings.Repeat("x", shared.MaxLogErr)+"… | upstream event: ") {
		t.Errorf("got %q", got)
	}
}

func codexCall(t *testing.T, backend http.Handler, headerTimeout time.Duration) (status int, body, access, stream string) {
	t.Helper()
	return codexCallWith(t, backend, &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: headerTimeout}}, nil)
}

func TestStalledBackendIs502WithReasonInLog(t *testing.T) {
	status, _, access, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the client hanging up; without
		// it the handler, and so httptest.Server.Close, would block forever.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}), 100*time.Millisecond)
	if status != fiber.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if !strings.Contains(access, " 502 - ") || !strings.Contains(access, "timeout awaiting response headers") {
		t.Errorf("access log = %q", access)
	}
	// Which connection it waited on tells a dead one from a bad route.
	if !strings.Contains(access, " | conn new (dns ") {
		t.Errorf("access log lacks the connection: %q", access)
	}
}

func TestUpstreamErrorMessageIsLogged(t *testing.T) {
	status, _, access, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"usage limit reached"}}`))
	}), time.Minute)
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if !strings.Contains(access, " 429 - ") || !strings.Contains(access, ": codex: usage limit reached") {
		t.Errorf("access log = %q", access)
	}
}

// A refusal by the edge (a 403 page) names the ids and the verdict of the edge
// and shows what the page says, so a challenge can be told from a block.
func TestEdgeRefusalLogsIdsAndPageText(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, access, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cf-Ray", "abc123-PIT")
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><body><script>var a=1;</script><h1>Enable JavaScript</h1></body></html>`))
	}), time.Minute)
	if status != fiber.StatusForbidden || !strings.Contains(body, "html error page: Enable JavaScript") {
		t.Fatalf("status %d, body %q", status, body)
	}
	for _, want := range []string{" 403 - ", "html error page: Enable JavaScript | cf-ray=abc123-PIT cf-mitigated=challenge | conn "} {
		if !strings.Contains(access, want) {
			t.Errorf("access log lacks %q: %q", want, access)
		}
	}
	if strings.Contains(access, "var a") {
		t.Errorf("a script reached the log: %q", access)
	}
	// A page is the edge's, not the backend's: asked again, twice, then given up.
	if calls.Load() != 3 {
		t.Errorf("backend calls = %d, want 3 (the request and two retries)", calls.Load())
	}
	for _, want := range []string{"upstream retry 1/2", "upstream retry 2/2", "HTTP 403: html error page: Enable JavaScript", "cf-ray=abc123-PIT cf-mitigated=challenge | conn new"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream log lacks %q: %q", want, stream)
		}
	}
}

// A gateway error is still asked again, and the failed line says which connection it was on.
func TestGatewayErrorLineNamesTheConnection(t *testing.T) {
	fastRetries(t, 1)
	status, _, access, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "bad gateway")
	}), time.Minute)
	if status != fiber.StatusBadGateway {
		t.Fatalf("status %d, want 502", status)
	}
	if !strings.Contains(stream, "upstream retry 1/1") || !strings.Contains(access, " 502 - ") || !strings.Contains(access, "| conn ") {
		t.Errorf("stream %q, access %q", stream, access)
	}
}

func TestBrokenStreamIsLogged(t *testing.T) {
	fastRetries(t, 2)
	status, body, _, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
	}), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "event: error") {
		t.Fatalf("status %d, body %q: want 200 with an error event", status, body)
	}
	if !strings.Contains(stream, "stream failed after") || !strings.Contains(stream, "ended before completion") ||
		!strings.Contains(stream, "[codex]") {
		t.Errorf("stream log = %q", stream)
	}
	if strings.Contains(stream, "not retried") {
		t.Errorf("no content was sent, yet the log says it was: %q", stream)
	}
}

// The header timeout must not cut a reply that has started: streams run for minutes.
func TestSlowStreamPastHeaderTimeoutSurvives(t *testing.T) {
	status, body, access, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond) // well past the 100 ms header timeout
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"))
	}), 100*time.Millisecond)
	if status != fiber.StatusOK || strings.Contains(body, "event: error") || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q: want a complete stream", status, body)
	}
	if stream != "" || strings.Contains(access, ": ") {
		t.Errorf("unexpected failure log: access %q, stream %q", access, stream)
	}
}

func TestLogStreamFailureLine(t *testing.T) {
	var b bytes.Buffer
	shared.LogStreamFailure(&b, time.Now().Add(-2*time.Second), "stream", "codex", "claude-opus-5-5 -> gpt-6.1-sol", "stream read error:\n unexpected EOF", "", "")
	got := b.String()
	for _, want := range []string{"stream failed after 2", "[codex] claude-opus-5-5 -> gpt-6.1-sol: stream read error: unexpected EOF\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q lacks %q", got, want)
		}
	}
}

// An error event from the backend is logged raw (but for the message the line
// already shows), with the request ids. (Not
// an overload: that one is retried, see TestOverloadedBackendIsAskedAgain.)
func TestUpstreamErrorEventIsLoggedRaw(t *testing.T) {
	ev := `{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too long"},"sequence_number":3}`
	_, body, _, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req_123")
		w.Header().Set("Cf-Ray", "ray-ams")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: " + ev + "\n\n"))
	}), time.Minute)
	if !strings.Contains(body, "codex: context_length_exceeded: too long") {
		t.Errorf("client error lacks the upstream message: %q", body)
	}
	logged := strings.Replace(ev, `"too long"`, shared.ShownMessage, 1)
	for _, want := range []string{"context_length_exceeded: too long | upstream event: " + logged, "| x-request-id=req_123 cf-ray=ray-ams | conn new", "| request "} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream log %q lacks %q", stream, want)
		}
	}
}

func TestLogStreamCanceledLine(t *testing.T) {
	var b bytes.Buffer
	rt.LogStreamCanceled(&b, time.Now().Add(-3*time.Second), "stream", "codex", "a -> b", "conn reused (idle 1s)")
	if got := b.String(); !strings.Contains(got, "stream canceled by client after 3") || !strings.HasSuffix(got, "[codex] a -> b: conn reused (idle 1s)\n") {
		t.Errorf("line %q", got)
	}
}

// The ids of the backend come last on every stream line, the canceled one too.
func TestCanceledLineEndsWithTheIds(t *testing.T) {
	var buf bytes.Buffer
	old := shared.LogOut
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := &sse.Translator{Failed: "client left"}
	up := &http.Response{Header: http.Header{"Cf-Ray": {"ray-1"}}}
	trace := obs.NewUpstreamTrace()
	trace.Prompt = "key abcd1234, no system prompt, no tools, 1 input items"
	logUpstreamEnd(ctx, tr, trace, up, "codex", "a -> b", 10)
	got := buf.String()
	if !strings.Contains(got, "stream canceled by client") || !strings.HasSuffix(got, "| prompt key abcd1234, no system prompt, no tools, 1 input items | cf-ray=ray-1\n") {
		t.Errorf("line %q", got)
	}
}

// A finished reply tells the watch what it was billed, and the next request of
// the key reads it, even when the reply was not slow enough to be logged.
func TestUpstreamEndFeedsTheCacheWatch(t *testing.T) {
	obs.PromptWatch = &obs.CacheWatch{}
	t.Cleanup(func() { obs.PromptWatch = &obs.CacheWatch{} })
	var buf bytes.Buffer
	old := shared.LogOut
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })
	f := codex.Fingerprint{Key: "abcd1234", System: "s", Tools: "t", NTools: 1, Items: 4}
	registration := obs.PromptWatch.See(f, []uint64{1, 2, 3, 4})
	trace := obs.NewUpstreamTrace()
	trace.Fp = f
	trace.Prev = registration.Prev
	trace.PromptGeneration = registration.Generation
	trace.Prompt = obs.PromptText(f, registration, time.Now())
	tr := &sse.Translator{Usage: sse.Usage{Input: 640000, Cached: 600000, Output: 5}}
	logUpstreamEnd(context.Background(), tr, trace, &http.Response{Header: http.Header{}}, "codex", "a -> b", 10)
	if buf.Len() != 0 {
		t.Errorf("a fast reply was logged: %q", buf.String())
	}
	next := codex.Fingerprint{Key: "abcd1234", System: "s", Tools: "t", NTools: 1, Items: 6}
	registration = obs.PromptWatch.See(next, []uint64{1, 2, 3, 4, 5, 6})
	if got := obs.PromptText(next, registration, time.Now()); !strings.Contains(got, "history extends the last request by 2 items; the last request had 640000 tokens in") {
		t.Errorf("line %q", got)
	}
}

// The log line says what went on the wire: the compressed size next to the
// request's, and how long the writing took.
func TestSummaryShowsTheCompressedSize(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &obs.UpstreamTrace{Start: start, Wire: &codex.Wire{Raw: 6 << 20, Sent: 1 << 20, Encoding: "zstd"}}
	if got := u.Summary(&sse.Translator{}, start.Add(time.Second), 6<<20); !strings.Contains(got, "| request 6.0 MiB (zstd 1.0 MiB)") {
		t.Errorf("summary = %q", got)
	}
	ct := u.ClientTrace()
	ct.GetConn("h:443")
	ct.GotConn(httptrace.GotConnInfo{})
	u.Got, u.Wrote = start.Add(time.Second), start.Add(3*time.Second)
	if got := u.Summary(&sse.Translator{}, start.Add(5*time.Second), 6<<20); !strings.Contains(got, "| request 6.0 MiB (zstd 1.0 MiB, written in 2s)") {
		t.Errorf("summary = %q", got)
	}
	u.Wire = &codex.Wire{Raw: 100, Sent: 100}
	if got := u.Summary(&sse.Translator{}, start.Add(5*time.Second), 100); !strings.Contains(got, "| request 100 B (written in 2s)") {
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
	u := &obs.UpstreamTrace{}
	if got := u.SentText(now); got != "" {
		t.Errorf("before any attempt = %q", got)
	}
	u.Attempt = now.Add(-20 * time.Second)
	if got := u.SentText(now); got != "no connection after 20s" {
		t.Errorf("no connection = %q", got)
	}
	u.Got = now.Add(-18 * time.Second)
	if got := u.SentText(now); got != "not written out, 18s on the connection" {
		t.Errorf("still going out = %q", got)
	}
	u.Wrote = now.Add(-10 * time.Second)
	u.Wire = &codex.Wire{Raw: 5 << 20, Sent: 2 << 20, Encoding: "zstd"}
	want := "request 5.0 MiB (zstd 2.0 MiB): written in 8s, no reply for 10s"
	if got := u.SentText(now); got != want {
		t.Errorf("written = %q\nwant      %q", got, want)
	}
	u.Wire = &codex.Wire{Raw: 100, Sent: 100}
	if got := u.SentText(now); !strings.HasPrefix(got, "request 100 B: written in") {
		t.Errorf("plain body = %q", got)
	}
}

func TestReconnectLineShowsHowFarTheRequestGot(t *testing.T) {
	var b strings.Builder
	obs.LogReconnect(&b, "codex", "a -> b", 1, 50*time.Second, "http2: client connection lost", "warm, opened 5s before", "request 4.8 MiB (zstd 2.2 MiB): written in 5.4s, no reply for 44.6s")
	if got := b.String(); !strings.HasSuffix(got, "connection | request 4.8 MiB (zstd 2.2 MiB): written in 5.4s, no reply for 44.6s\n") {
		t.Errorf("line = %q", got)
	}
}

// A stream that dies after content says how long it had been silent.
func TestFailedStreamLogsHowLongItWasSilent(t *testing.T) {
	fastRetries(t, 1)
	_, _, _, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"half\"}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
	}), time.Minute)
	for _, want := range []string{"stream failed after", "not retried: content already sent (text)", " | silent for "} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q: %q", want, log)
		}
	}
}

// The line is stamped with the start of the call, so it says when the stream
// ended, and "after" counts from the same start as "total".
func TestFailedStreamLineNamesItsEnd(t *testing.T) {
	fastRetries(t, 1)
	_, _, _, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"half\"}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
	}), time.Minute)
	m := regexp.MustCompile(`stream failed after (\S+) .* total (\S+), ended \d\d:\d\d:\d\d`).FindStringSubmatch(log)
	if m == nil {
		t.Fatalf("no failed line with total and ended: %q", log)
	}
	after, err1 := time.ParseDuration(m[1])
	total, err2 := time.ParseDuration(m[2])
	if err1 != nil || err2 != nil || after < total || after-total > 100*time.Millisecond {
		t.Errorf("after %q and total %q are not the same span", m[1], m[2])
	}
}

// A request tells the next one of its key which data centre answered; the next
// failed or slow line then says whether the edge changed.
func TestEdgeFlowsFromOneRequestToTheNext(t *testing.T) {
	obs.PromptWatch = &obs.CacheWatch{}
	obs.PromptStats = &obs.CacheStats{}
	t.Cleanup(func() { obs.PromptWatch, obs.PromptStats = &obs.CacheWatch{}, &obs.CacheStats{} })
	var buf bytes.Buffer
	old := shared.LogOut
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })

	f1 := codex.Fingerprint{Key: "abcd1234", System: "s", Tools: "t", NTools: 1, Items: 4}
	t1 := obs.NewUpstreamTrace()
	t1.Fp = f1
	reg1 := obs.PromptWatch.See(f1, []uint64{1, 2, 3, 4})
	t1.Prev = reg1.Prev
	t1.PromptGeneration = reg1.Generation
	t1.Prompt = obs.PromptText(f1, reg1, time.Now())
	logUpstreamEnd(context.Background(), &sse.Translator{Usage: sse.Usage{Input: 640000, Cached: 600000}}, t1,
		&http.Response{Header: http.Header{"Cf-Ray": {"r1-LAX"}}}, "codex", "a -> b", 10)
	if buf.Len() != 0 {
		t.Fatalf("a fast reply was logged: %q", buf.String())
	}

	f2 := codex.Fingerprint{Key: "abcd1234", System: "s", Tools: "t", NTools: 1, Items: 6}
	t2 := obs.NewUpstreamTrace()
	t2.Fp = f2
	reg2 := obs.PromptWatch.See(f2, []uint64{1, 2, 3, 4, 5, 6})
	t2.Prev = reg2.Prev
	t2.PromptGeneration = reg2.Generation
	t2.Prompt = obs.PromptText(f2, reg2, time.Now())
	tr := &sse.Translator{Failed: "boom", Usage: sse.Usage{Input: 650000, Cached: 400000}}
	logUpstreamEnd(context.Background(), tr, t2, &http.Response{Header: http.Header{"Cf-Ray": {"r2-PIT"}}}, "codex", "a -> b", 10)
	got := buf.String()
	if !strings.Contains(got, "; edge LAX -> PIT") || !strings.Contains(got, "cache partial 61% (400000 of 650000)") {
		t.Errorf("line %q", got)
	}
	// Both replies went into the counters.
	line, by := obs.PromptStats.Take()
	if !strings.HasPrefix(line, "2 big prompts, 1 read from the cache in part") {
		t.Errorf("counters: %q", line)
	}
	// The second reply has known gap/edge groups and both replies are not sent with turn state.
	if by != "started <5m before: 1 prompts, 61% cached; other edge: 1 prompts, 61% cached; turn state not sent: 2 prompts, 77% cached" {
		t.Errorf("groups: %q", by)
	}
	if !strings.Contains(got, "; the last request started ") || !strings.Contains(got, " ago, ended ") {
		t.Errorf("no timing of the last request: %q", got)
	}
}

// A reply that ends well, and one that breaks mid-stream, both reach the stats
// the log line is made of.
func TestCodexRepliesFeedTheLinkStats(t *testing.T) {
	old := obs.LinkHealth
	obs.LinkHealth = &obs.LinkStats{}
	t.Cleanup(func() { obs.LinkHealth = old })

	status, _, _, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { turnStateOK(w, "") }), time.Minute)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := obs.LinkHealth.Take(); !strings.Contains(got, "1 reply") || !strings.Contains(got, "no broken calls") {
		t.Errorf("link stats after a good reply = %q", got)
	}

	fastRetries(t, 0)
	codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
	}), time.Minute)
	if got := obs.LinkHealth.Take(); !strings.Contains(got, "1 broken call (0 sent again by ccgp, 1 reached the client): 1 other") {
		t.Errorf("link stats after a cut stream = %q", got)
	}

	codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		<-r.Context().Done()
	}), 100*time.Millisecond)
	if got := obs.LinkHealth.Take(); !strings.Contains(got, "1 no reply in time") {
		t.Errorf("link stats after a silent backend = %q", got)
	}
}

// The slow-stream line carries the split.
func TestSlowStreamLineHasTheTimeSplit(t *testing.T) {
	oldSlow := obs.SlowTotal
	obs.SlowTotal = 0
	t.Cleanup(func() { obs.SlowTotal = oldSlow })
	_, _, _, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		turnStateOK(w, "")
	}), time.Minute)
	if !strings.Contains(stream, "slow stream ") || !strings.Contains(stream, "| time ") || !strings.Contains(stream, "(mostly ") {
		t.Errorf("slow line lacks the time split: %q", stream)
	}
}

func TestSlowStreamIsLoggedWithTiming(t *testing.T) {
	old := obs.SlowHeaders
	// 0, not 1ns: on Windows the clock moves in ticks, so a local reply can take 0.
	obs.SlowHeaders = 0
	t.Cleanup(func() { obs.SlowHeaders = old })

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
		"slow stream ", "[codex] claude-opus-5-5 -> ", ": conn new (dns ", "| headers ", ", first token ", ", total ", ", ended ",
		"| request ", "| tokens in 100 (cached 40) out 20 (reasoning 5)", "| x-request-id=req_9\n",
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("slow line %q lacks %q", stream, want)
		}
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

// The end lines of a reply without streaming say so.
func TestUpstreamEndNamesANonStreamReply(t *testing.T) {
	var buf bytes.Buffer
	old := shared.LogOut
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })
	for _, nonStream := range []bool{false, true} {
		buf.Reset()
		trace := obs.NewUpstreamTrace()
		trace.NonStream = nonStream
		trace.Start = time.Now().Add(-obs.SlowTotal - time.Second)
		logUpstreamEnd(context.Background(), &sse.Translator{}, trace, &http.Response{Header: http.Header{}}, "codex", "a -> b", 10)
		want := "] slow stream "
		if nonStream {
			want = "] slow non-stream reply "
		}
		if !strings.Contains(buf.String(), want) {
			t.Errorf("nonStream %v: line %q, want %q", nonStream, buf.String(), want)
		}
	}
}
