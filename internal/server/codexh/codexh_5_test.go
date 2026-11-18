package codexh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/wsh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v2"
)

func codexWSSideUser(text string) string { return `{"role":"user","content":"` + text + `"}` }

func codexWSSideAssistant(text string) string { return `{"role":"assistant","content":"` + text + `"}` }

// A background request of the client (a short fixed message after the same
// history) answered on the websocket replaces its chain; once ccgp has seen
// that, requests ending in the message go over HTTP and the conversation's
// chain survives them.
func TestCodexWebsocketBackgroundRequestsGoOverHTTPOnceLearned(t *testing.T) {
	var frames, posts atomic.Int32
	answers := []string{"a1", "s1", "a2", "a3"}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			posts.Add(1)
			okStream(w)
			return
		}
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for _, answer := range answers {
			readCodexWSTestFrame(t, conn)
			id := "side-response-" + answer
			frames.Add(1)
			writeCodexWSTestEvents(t, conn,
				`{"type":"response.created","response":{"id":"`+id+`"}}`,
				`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+answer+`"}]}}`,
				`{"type":"response.completed","response":{"id":"`+id+`"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	post := func(messages string) {
		t.Helper()
		if status, body := h.post(t, codexWSSideBody("side-session", messages)); status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
			t.Fatalf("status %d, body %q", status, body)
		}
	}
	main, a1, side := codexWSSideUser("main question"), codexWSSideAssistant("a1"), codexWSSideUser("background template")

	post(main)
	post(strings.Join([]string{main, a1, side}, ","))
	post(strings.Join([]string{main, a1, codexWSSideUser("real next")}, ","))
	if frames.Load() != 3 || posts.Load() != 0 {
		t.Fatalf("before learning: %d websocket frames and %d HTTP posts, want 3 and 0", frames.Load(), posts.Load())
	}
	if !strings.Contains(h.stream.String(), "requests ending in it go over HTTP from now on") {
		t.Errorf("the learned message was not announced: %q", h.stream.String())
	}

	post(strings.Join([]string{main, a1, codexWSSideUser("real next"), codexWSSideAssistant("a2"), side}, ","))
	if frames.Load() != 3 || posts.Load() != 1 {
		t.Fatalf("background request: %d websocket frames and %d HTTP posts, want 3 and 1", frames.Load(), posts.Load())
	}
	if !strings.Contains(h.access.String(), "transport http (side request)") {
		t.Errorf("access log lacks the side request note: %q", h.access.String())
	}
	// The line names the conversation and agent and the size, so the cost is told per agent.
	if !regexp.MustCompile(`transport http \(side request\)[^\n]*\| conv [0-9a-f]{6}\.[0-9a-f]{4} \| request \d`).MatchString(h.access.String()) {
		t.Errorf("side request line lacks conv and size: %q", h.access.String())
	}
	var stats strings.Builder
	wsh.WriteCodexWebsocketStats(&stats, time.Now())
	if !strings.Contains(stats.String(), "1 background request sent over HTTP") {
		t.Errorf("periodic line = %q", stats.String())
	}

	// The next request keeps the message, with a reply: it was an ordinary turn,
	// so it is forgotten and the request goes on the websocket.
	post(strings.Join([]string{main, a1, codexWSSideUser("real next"), codexWSSideAssistant("a2"), side, codexWSSideAssistant("r"), codexWSSideUser("go on")}, ","))
	if frames.Load() != 4 || posts.Load() != 1 {
		t.Fatalf("after the ordinary turn: %d websocket frames and %d HTTP posts, want 4 and 1", frames.Load(), posts.Load())
	}
	if !regexp.MustCompile(`upstream websocket \[codex\] conv [0-9a-f]{6}\.[0-9a-f]{4}: a user message of \d+ B taken for a background request's stayed in the next request, with its reply \(an ordinary turn\); it is no longer taken for one \(1 request had gone over HTTP for it\)`).MatchString(h.stream.String()) {
		t.Errorf("the forgotten message was not announced: %q", h.stream.String())
	}
	stats.Reset()
	wsh.WriteCodexWebsocketStats(&stats, time.Now())
	if !strings.Contains(stats.String(), "1 message taken for a background request's found to be an ordinary turn's") {
		t.Errorf("periodic line = %q", stats.String())
	}
}

// The subagents of a conversation share its cache key, system prompt and tools;
// each keeps its own websocket, so one does not replace the other's chain.
func TestCodexWebsocketAgentsOfOneConversationHaveTheirOwnConnection(t *testing.T) {
	var mu sync.Mutex
	var conns atomic.Int32
	var previous []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		id := conns.Add(1)
		for exchange := 0; exchange < 2; exchange++ {
			frame := readCodexWSTestFrame(t, conn)
			var payload struct {
				Previous string `json:"previous_response_id"`
			}
			_ = json.Unmarshal(frame, &payload)
			mu.Lock()
			previous = append(previous, payload.Previous)
			mu.Unlock()
			responseID := fmt.Sprintf("agent-%d-%d", id, exchange)
			writeCodexWSTestEvents(t, conn,
				`{"type":"response.created","response":{"id":"`+responseID+`"}}`,
				`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"r"}]}}`,
				`{"type":"response.completed","response":{"id":"`+responseID+`"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	post := func(messages ...string) {
		t.Helper()
		if status, body := h.post(t, codexWSSideBody("agents-session", strings.Join(messages, ","))); status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
			t.Fatalf("status %d, body %q", status, body)
		}
	}
	taskA, taskB, reply := codexWSSideUser("task of agent A"), codexWSSideUser("task of agent B"), codexWSSideAssistant("r")
	post(taskA)
	post(taskB)
	post(taskA, reply, codexWSSideUser("more for A"))
	post(taskB, reply, codexWSSideUser("more for B"))

	mu.Lock()
	got := append([]string(nil), previous...)
	mu.Unlock()
	if want := []string{"", "", "agent-1-0", "agent-2-0"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("previous_response_id of the frames = %q, want %q (full, full, then a delta on each agent's own connection)", got, want)
	}
	if conns.Load() != 2 {
		t.Errorf("handshakes = %d, want one per agent", conns.Load())
	}
	tags := regexp.MustCompile(`conv ([0-9a-f]{6})\.([0-9a-f]{4})`).FindAllStringSubmatch(h.access.String(), -1)
	if len(tags) != 4 || tags[0][1] != tags[1][1] || tags[0][2] == tags[1][2] || tags[0][2] != tags[2][2] || tags[1][2] != tags[3][2] {
		t.Errorf("conversation tags in the access log = %q, want one thread and two agent tags", tags)
	}
}

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

// A reply read to its end (no streaming asked for) is asked again like a
// stream: an overloaded backend before any content is not the client's error.
func TestNonStreamOverloadIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	nonStreamBody(t)
	var calls atomic.Int32
	status, body, _, log := codexCall(t, overloadBackend(&calls, 1), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "\"hi\"") {
		t.Fatalf("status %d, body %q: want the answer of the second try", status, body)
	}
	if calls.Load() != 2 || !strings.Contains(log, "upstream retry 1/2 in 1ms") || !strings.Contains(log, "server_is_overloaded") {
		t.Errorf("calls %d, log %q", calls.Load(), log)
	}
	if strings.Contains(log, "stream failed") {
		t.Errorf("a retried reply is logged as failed: %q", log)
	}
}

// A broken connection before any content is asked again for a reply without
// streaming, too.
func TestNonStreamBrokenReplyIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	nonStreamBody(t)
	var calls atomic.Int32
	status, body, _, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")) // ends early
			return
		}
		overloadBackend(new(atomic.Int32), 0).ServeHTTP(w, r)
	}), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "\"hi\"") || calls.Load() != 2 {
		t.Fatalf("status %d, calls %d, body %q", status, calls.Load(), body)
	}
	if !strings.Contains(log, "upstream retry 1/2") || !strings.Contains(log, "ended before completion") {
		t.Errorf("log %q", log)
	}
}

// When the retries run out the client gets the 502 and the failure is logged
// like a stream's, with the reason and the timing.
func TestNonStreamOverloadRunsOut(t *testing.T) {
	fastRetries(t, 1)
	nonStreamBody(t)
	var calls atomic.Int32
	status, _, access, log := codexCall(t, overloadBackend(&calls, 10), time.Minute)
	if status != fiber.StatusBadGateway || !strings.Contains(access, "server_is_overloaded") || !strings.Contains(access, "| non-stream") {
		t.Fatalf("status %d, access %q: want 502 with the reason and the non-stream note", status, access)
	}
	if calls.Load() != 2 || !strings.Contains(log, "upstream retry 1/1") || !strings.Contains(log, "non-stream reply failed after") {
		t.Errorf("calls %d, log %q", calls.Load(), log)
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

func parseStreamEvents(body string) []streamEvent {
	var events []streamEvent
	for _, eventText := range strings.Split(body, "\n\n") {
		var event streamEvent
		var data string
		for _, line := range strings.Split(eventText, "\n") {
			if strings.HasPrefix(line, "event: ") {
				event.name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event.name != "" && data != "" {
			if err := json.Unmarshal([]byte(data), &event.data); err == nil {
				events = append(events, event)
			}
		}
	}
	return events
}

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

type streamEvent struct {
	name string
	data map[string]interface{}
}

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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

// A continuation that sent a turn state and got none back is normal and says
// nothing; only a request with no state in hand and none returned is noted.
func TestTurnStateNoteIsQuietOnContinuations(t *testing.T) {
	oldStates, oldWatch, oldStats := obs.TurnStates, obs.PromptWatch, obs.PromptStats
	obs.TurnStates, obs.PromptWatch, obs.PromptStats = &obs.TurnStateStore{}, &obs.CacheWatch{}, &obs.CacheStats{}
	t.Cleanup(func() { obs.TurnStates, obs.PromptWatch, obs.PromptStats = oldStates, oldWatch, oldStats })
	oldBody, oldSlowTotal := codexTestBody, obs.SlowTotal
	codexTestBody = ""
	obs.SlowTotal = 0
	t.Cleanup(func() { codexTestBody, obs.SlowTotal = oldBody, oldSlowTotal })

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

func TestTurnStateBucketsAreIsolatedByFullTuple(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	base := fp("ab", "c", "tools", 1)
	reg := s.Begin(base, false, false, now)
	reg.Store("saved", now)

	for _, other := range []codex.Fingerprint{
		fp("a", "bc", "tools", 1),
		fp("ab", "changed-system", "tools", 1),
		fp("ab", "c", "changed-tools", 1),
	} {
		if got := s.Begin(other, true, false, now).State; got != "" {
			t.Errorf("tuple %q/%q/%q read another bucket: %q", other.Key, other.System, other.Tools, got)
		}
	}
	modelOnly := base
	modelOnly.Model = "different-model"
	modelOnly.Reasoning = "different-reasoning"
	if got := s.Begin(modelOnly, true, false, now).State; got != "saved" {
		t.Errorf("model/control fields changed bucket identity: %q", got)
	}
}

func TestTurnThreadIDIsStableAndUnambiguous(t *testing.T) {
	first := fp("ab", "c", "", 1)
	if a, b := obs.TurnThreadID(obs.TurnKey(first)), obs.TurnThreadID(obs.TurnKey(first)); a != b {
		t.Fatalf("same tuple produced different IDs: %q and %q", a, b)
	}
	if a, b := obs.TurnThreadID(obs.TurnKey(first)), obs.TurnThreadID(obs.TurnKey(fp("a", "bc", "", 1))); a == b {
		t.Fatalf("ambiguous tuples produced the same ID: %q", a)
	}
	if got := len(obs.TurnThreadID(obs.TurnKey(first))); got != 64 {
		t.Errorf("SHA256 hex ID length = %d, want 64", got)
	}
}

func TestTurnStateTTLBoundary(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("key", "system", "tools", 1)
	s.Begin(f, false, false, now).Store("saved", now)
	if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL-time.Nanosecond)).State; got != "saved" {
		t.Fatalf("state just before expiry = %q", got)
	}
	if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL)).State; got != "" {
		t.Errorf("state at expiry = %q, want empty", got)
	}
}

func TestTurnStateMissingStateRefreshDoesNotExtendTTL(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("key", "system", "tools", 1)
	s.Begin(f, false, false, now).Store("saved", now)
	for i := 1; i <= 4; i++ {
		at := now.Add(time.Duration(i) * 5 * time.Minute)
		if got := s.Begin(f, true, false, at).State; got != "saved" {
			t.Fatalf("continuation %d state = %q", i, got)
		}
	}
	if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL)).State; got != "" {
		t.Errorf("repeated Begin extended old state to expiry: %q", got)
	}

	fresh := s.Begin(f, true, false, now.Add(obs.TurnStateTTL+time.Minute))
	fresh.Store("refreshed", now.Add(obs.TurnStateTTL+time.Minute))
	if got := s.Begin(f, true, false, now.Add(2*obs.TurnStateTTL)).State; got != "refreshed" {
		t.Errorf("fresh Store did not refresh TTL: %q", got)
	}
}

func TestTurnStateFIFOEvictsOldestWithoutReorderingRepeatedKeys(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	first := fp("key0000", "system", "tools", 1)
	s.Begin(first, false, false, now).Store("old", now)
	for i := 1; i < obs.MaxTurnStateBuckets; i++ {
		s.Begin(fp(fmt.Sprintf("key%04d", i), "system", "tools", 1), false, false, now)
	}
	s.Begin(first, true, false, now)
	s.Begin(fp("new-key", "system", "tools", 1), false, false, now)
	if len(s.Buckets) != obs.MaxTurnStateBuckets {
		t.Fatalf("bucket count = %d, want %d", len(s.Buckets), obs.MaxTurnStateBuckets)
	}
	if _, exists := s.Buckets[obs.TurnKey(first)]; exists {
		t.Error("repeated first key was reordered instead of evicted")
	}
	if got := s.Begin(fp("key0001", "system", "tools", 1), true, false, now).ThreadID; got != obs.TurnThreadID(obs.TurnKey(fp("key0001", "system", "tools", 1))) {
		t.Errorf("newest key thread ID changed: %q", got)
	}
}

func TestTurnStateNewTurnClearsAndRejectsStaleCompletion(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("key", "system", "tools", 1)
	s.Begin(f, false, false, now).Store("before", now)
	old := s.Begin(f, true, false, now.Add(time.Second))
	cleared := s.Begin(f, false, true, now.Add(2*time.Second))
	if !cleared.NewTurn {
		t.Fatal("new turn was not reported")
	}
	old.Store("stale reply", now.Add(3*time.Second))
	if got := s.Begin(f, true, false, now.Add(4*time.Second)).State; got != "" {
		t.Fatalf("stale completion restored cleared state: %q", got)
	}
}

func TestTurnStateStaleCompletionCannotRestoreEvictedOrExpiredBucket(t *testing.T) {
	t.Run("eviction", func(t *testing.T) {
		s := &obs.TurnStateStore{}
		now := time.Unix(1000, 0)
		f := fp("evicted", "system", "tools", 1)
		stale := s.Begin(f, false, false, now)
		for i := 0; i < obs.MaxTurnStateBuckets; i++ {
			s.Begin(fp(fmt.Sprintf("other%04d", i), "system", "tools", 1), false, false, now)
		}
		s.Begin(f, false, false, now)
		stale.Store("late", now)
		if got := s.Begin(f, true, false, now).State; got != "" {
			t.Errorf("evicted registration restored state: %q", got)
		}
	})
	t.Run("expiry", func(t *testing.T) {
		s := &obs.TurnStateStore{}
		now := time.Unix(1000, 0)
		f := fp("expired", "system", "tools", 1)
		stale := s.Begin(f, false, false, now)
		if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL)).State; got != "" {
			t.Fatalf("state at expiry = %q", got)
		}
		stale.Store("late", now.Add(obs.TurnStateTTL))
		if got := s.Begin(f, true, false, now.Add(obs.TurnStateTTL+time.Nanosecond)).State; got != "" {
			t.Errorf("expired registration restored state: %q", got)
		}
	})
}

func TestTurnStateWithoutKeyIsNotStored(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	f := fp("", "system", "tools", 1)
	reg := s.Begin(f, false, false, now)
	if reg.ThreadID != obs.TurnThreadID(obs.TurnKey(f)) {
		t.Errorf("thread ID = %q", reg.ThreadID)
	}
	reg.Store("private state", now)
	if len(s.Buckets) != 0 {
		t.Errorf("stored %d buckets without a key", len(s.Buckets))
	}
	if got := s.Begin(f, true, false, now).State; got != "" {
		t.Errorf("replayed state without a key: %q", got)
	}
}

func TestTurnStateStoreConcurrentUse(t *testing.T) {
	s := &obs.TurnStateStore{}
	now := time.Unix(1000, 0)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				f := fp(fmt.Sprintf("key%d", (g*7+i)%80), fmt.Sprintf("system%d", i%3), "tools", 1)
				reg := s.Begin(f, i%2 == 0, i%17 == 0, now.Add(time.Duration(i)*time.Second))
				reg.Store(fmt.Sprintf("state-%d", i), now.Add(time.Duration(i)*time.Second))
			}
		}()
	}
	wg.Wait()
	if len(s.Buckets) > obs.MaxTurnStateBuckets {
		t.Errorf("bucket count = %d, exceeds %d", len(s.Buckets), obs.MaxTurnStateBuckets)
	}
}

func fp(key, system, tools string, items int) codex.Fingerprint {
	return codex.Fingerprint{Key: key, System: system, Tools: tools, NTools: 3, Items: items}
}
