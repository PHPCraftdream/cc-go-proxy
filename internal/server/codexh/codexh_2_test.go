package codexh

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/wsh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/PHPCraftdream/cc-go-proxy/pkg/models"
	"github.com/gofiber/fiber/v2"
)

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

func overloadBackend(calls *atomic.Int32, overloadedCalls int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		created := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"
		if calls.Add(1) <= overloadedCalls {
			_, _ = w.Write([]byte(created + "data: {\"type\":\"error\",\"error\":{\"type\":\"service_unavailable_error\",\"code\":\"server_is_overloaded\",\"message\":\"overloaded\"}}\n\n"))
			return
		}
		_, _ = w.Write([]byte(created +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"hi\"}\n\n" +
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n"))
	})
}

func fastRetries(t *testing.T, n int) {
	old, oldBroken := OverloadBackoff, BrokenBackoff
	OverloadBackoff = make([]time.Duration, n)
	BrokenBackoff = make([]time.Duration, n)
	for i := 0; i < n; i++ {
		OverloadBackoff[i], BrokenBackoff[i] = time.Millisecond, time.Millisecond
	}
	t.Cleanup(func() { OverloadBackoff, BrokenBackoff = old, oldBroken })
}

func TestOverloadedBackendIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, _, stream := codexCall(t, overloadBackend(&calls, 1), time.Minute)
	if status != fiber.StatusOK || strings.Contains(body, "event: error") || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q: want a complete stream", status, body)
	}
	if strings.Count(body, "event: message_start") != 1 || !strings.Contains(body, "event: ping") {
		t.Errorf("body %q: want one message_start and a ping", body)
	}
	if calls.Load() != 2 || !strings.Contains(stream, "upstream retry 1/2 in 1ms") || !strings.Contains(stream, "server_is_overloaded") {
		t.Errorf("calls %d, log %q", calls.Load(), stream)
	}
	if strings.Contains(stream, "stream failed") {
		t.Errorf("a retried stream is logged as failed: %q", stream)
	}
}

func TestOverloadRetriesRunOut(t *testing.T) {
	fastRetries(t, 1)
	var calls atomic.Int32
	_, body, _, stream := codexCall(t, overloadBackend(&calls, 10), time.Minute)
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("body %q: want the overload error after the retries", body)
	}
	if calls.Load() != 2 || !strings.Contains(stream, "upstream retry 1/1") || !strings.Contains(stream, "stream failed") {
		t.Errorf("calls %d, log %q", calls.Load(), stream)
	}
}

func TestLogStreamCanceledLine(t *testing.T) {
	var b bytes.Buffer
	rt.LogStreamCanceled(&b, time.Now().Add(-3*time.Second), "stream", "codex", "a -> b", "conn reused (idle 1s)")
	if got := b.String(); !strings.Contains(got, "stream canceled by client after 3") || !strings.HasSuffix(got, "[codex] a -> b: conn reused (idle 1s)\n") {
		t.Errorf("line %q", got)
	}
}

// connectProxy is an HTTP CONNECT proxy that counts its tunnels.
func connectProxy(t *testing.T) (proxyURL string, tunnels *atomic.Int32) {
	t.Helper()
	tunnels = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		tunnels.Add(1)
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		_, _ = io.Copy(up, c)
		_ = up.Close()
	}))
	t.Cleanup(srv.Close)
	return srv.URL, tunnels
}

// A profile uses the global proxy unless it has a proxy of its own, or none.
func TestCodexRequestsFollowTheProfileProxy(t *testing.T) {
	var calls atomic.Int32
	backend := overloadBackend(&calls, 0) // always a complete answer
	global, viaGlobal := connectProxy(t)
	own, viaOwn := connectProxy(t)

	run := func(profile string) {
		t.Helper()
		router := connpool.NewRouter(0, 1, 1, nil)
		status, body, _, _ := codexCallWith(t, backend, &http.Client{Transport: router}, func(cfg *config.Config) {
			cfg.Profile = profile
			cfg.ConnectionsProxy = global
			cfg.Profiles = map[string]*config.Profile{
				"inherits": {Name: "inherits", Provider: "codex"},
				"direct":   {Name: "direct", Provider: "codex", Proxy: config.ProxyNone},
				"own":      {Name: "own", Provider: "codex", Proxy: own},
			}
		})
		if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
			t.Fatalf("%s: status %d, body %q", profile, status, body)
		}
	}
	run("inherits")
	if g, o := viaGlobal.Load(), viaOwn.Load(); g != 1 || o != 0 {
		t.Fatalf("inherits: tunnels global %d, own %d; want 1 and 0", g, o)
	}
	run("direct")
	if g, o := viaGlobal.Load(), viaOwn.Load(); g != 1 || o != 0 {
		t.Fatalf("direct: tunnels global %d, own %d; want no new tunnel", g, o)
	}
	run("own")
	if g, o := viaGlobal.Load(), viaOwn.Load(); g != 1 || o != 1 {
		t.Fatalf("own: tunnels global %d, own %d; want 1 and 1", g, o)
	}
}

func TestOpenAIRequestsFollowTheProfileProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer backend.Close()
	global, viaGlobal := connectProxy(t)
	oldT := rt.OpenAITransport
	t.Cleanup(func() { rt.OpenAITransport = oldT })

	call := func(profile string, proxy string) {
		t.Helper()
		rt.OpenAITransport = connpool.NewRouter(0, 1, 1, nil)
		cfg := &config.Config{
			Profile: profile, OpenAIBaseURL: backend.URL, ConnectionsProxy: global,
			Profiles: map[string]*config.Profile{profile: {Name: profile, Proxy: proxy}},
		}
		if _, err := rt.CallOpenAIInternal(&models.OpenAIRequest{Model: "m"}, cfg); err != nil {
			t.Fatalf("%s: %v", profile, err)
		}
	}
	call("inherits", "")
	if g := viaGlobal.Load(); g != 1 {
		t.Fatalf("inherits: tunnels = %d, want 1", g)
	}
	call("direct", config.ProxyNone)
	if g := viaGlobal.Load(); g != 1 {
		t.Fatalf("direct: tunnels = %d, want no new one", g)
	}
}

func TestUpstreamCtxCarriesTheProfileChoice(t *testing.T) {
	global, _ := proxydial.Parse("socks5://127.0.0.1:1080")
	cfg := &config.Config{
		Profile: "p", ConnectionsProxy: global.String(),
		Profiles: map[string]*config.Profile{"p": {Name: "p", Proxy: config.ProxyNone}},
	}
	if c := proxydial.ChoiceFor(rt.UpstreamCtx(context.Background(), cfg)); !c.Direct {
		t.Errorf("profile none: %+v", c)
	}
	cfg.Profiles["p"].Proxy = ""
	if c := proxydial.ChoiceFor(rt.UpstreamCtx(context.Background(), cfg)); c.URL == nil || c.URL.Host != "127.0.0.1:1080" {
		t.Errorf("inheriting profile: %+v", c)
	}
}

// lastCodexReply is the header of the reply codexCallWith got, for tests that read it.
var lastCodexReply http.Header

// lastCodexClose is whether that reply closed its connection (the client drops the header).
var lastCodexClose bool

func rateLimitBackend(calls *atomic.Int32, retryAfter string, limited int32) http.Handler {
	ok := overloadBackend(calls, 0)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := calls.Load(); n < limited {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.Header().Set("X-RateLimit-Remaining-Requests", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"Rate limit exceeded"}}`)
			return
		}
		ok.ServeHTTP(w, r)
	})
}

func TestShortRateLimitIsWaitedOut(t *testing.T) {
	var calls atomic.Int32
	status, body, _, stream := codexCall(t, rateLimitBackend(&calls, "0", 1), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q: want the answer of the second call", status, body)
	}
	if calls.Load() != 2 {
		t.Errorf("backend calls = %d, want 2", calls.Load())
	}
	for _, want := range []string{"upstream rate limit: retrying in 0s", "retry-after=0", "x-ratelimit-remaining-requests=0"} {
		if !strings.Contains(stream, want) {
			t.Errorf("log %q lacks %q", stream, want)
		}
	}
}

func TestLongRateLimitGoesToTheClient(t *testing.T) {
	var calls atomic.Int32
	status, body, access, _ := codexCall(t, rateLimitBackend(&calls, "120", 100), time.Minute)
	if status != fiber.StatusTooManyRequests || !strings.Contains(body, "Rate limit exceeded") {
		t.Fatalf("status %d, body %q: want the 429", status, body)
	}
	if calls.Load() != 1 {
		t.Errorf("backend calls = %d: a long Retry-After must not be waited out", calls.Load())
	}
	if got := lastCodexReply.Get("Retry-After"); got != "120" {
		t.Errorf("Retry-After for the client = %q, want 120", got)
	}
	if !strings.Contains(access, "retry-after=120") || !strings.Contains(access, "x-ratelimit-remaining-requests=0") {
		t.Errorf("access log %q lacks the limits", access)
	}
	if strings.Contains(body, "x-ratelimit") {
		t.Errorf("the headers leaked into the client's message: %q", body)
	}
}

func TestRateLimitWithoutRetryAfterIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	status, _, _, _ := codexCall(t, rateLimitBackend(&calls, "", 100), time.Minute)
	if status != fiber.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("status %d after %d calls, want a plain 429 after one", status, calls.Load())
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		v    string
		want time.Duration
		ok   bool
	}{
		{"3", 3 * time.Second, true},
		{"0", 0, true},
		{"1.5", 1500 * time.Millisecond, true},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"-1", 0, false},
		{"soon", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.v != "" {
			h.Set("Retry-After", tc.v)
		}
		got, ok := shared.RetryAfter(h, now)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Retry-After %q = %s, %v; want %s, %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
	h := http.Header{"Retry-After": {"2"}, "X-Ratelimit-Remaining": {"0"}, "X-Codex-Primary-Reset-After-Seconds": {"31"}, "Content-Type": {"x"}}
	if got := shared.RateHeaders(h); got != "retry-after=2 x-codex-primary-reset-after-seconds=31 x-ratelimit-remaining=0" {
		t.Errorf("rateHeaders = %q", got)
	}
}

func TestNotRetriedNote(t *testing.T) {
	if got := notRetriedNote("thinking, text"); got != "not retried: content already sent (thinking, text)" {
		t.Errorf("with content = %q", got)
	}
	if got := notRetriedNote(""); got != "" {
		t.Errorf("without content = %q", got)
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

// A registration is active only for the lifetime of its request, including
// paths that fail before a response can be translated.
func TestCodexPromptRegistrationFinishesOnEveryExit(t *testing.T) {
	oldWatch := obs.PromptWatch
	t.Cleanup(func() { obs.PromptWatch = oldWatch })
	keyedBody := func(stream bool) string {
		return `{"model":"claude-opus-5-5","max_tokens":10,"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `,"metadata":{"user_id":"shared-subagent-session"},"messages":[{"role":"user","content":"hi"}]}`
	}
	assertFinished := func(t *testing.T) {
		t.Helper()
		active, entries, unended := obs.PromptWatch.Pending()
		if active != 0 {
			t.Errorf("active prompt keys remain: %d", active)
		}
		if entries != 1 {
			t.Errorf("watched entries = %d, want one", entries)
		}
		for _, key := range unended {
			t.Errorf("current watched request %q has no end time", key)
		}
	}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { okStream(w) })

	t.Run("transport error", func(t *testing.T) {
		obs.PromptWatch = &obs.CacheWatch{}
		codexTestBody = keyedBody(true)
		t.Cleanup(func() { codexTestBody = "" })
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("injected transport failure")
		})}
		status, _, _, _ := codexCallWith(t, backend, client, nil)
		if status != fiber.StatusBadGateway {
			t.Fatalf("status = %d, want 502", status)
		}
		assertFinished(t)
	})

	t.Run("non-2xx response", func(t *testing.T) {
		obs.PromptWatch = &obs.CacheWatch{}
		codexTestBody = keyedBody(true)
		t.Cleanup(func() { codexTestBody = "" })
		status, _, _, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
		}), time.Minute)
		if status != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
		assertFinished(t)
	})

	t.Run("successful non-stream", func(t *testing.T) {
		obs.PromptWatch = &obs.CacheWatch{}
		codexTestBody = keyedBody(false)
		t.Cleanup(func() { codexTestBody = "" })
		status, _, _, _ := codexCall(t, backend, time.Minute)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		assertFinished(t)
	})

	t.Run("successful stream", func(t *testing.T) {
		obs.PromptWatch = &obs.CacheWatch{}
		codexTestBody = keyedBody(true)
		t.Cleanup(func() { codexTestBody = "" })
		status, _, _, _ := codexCall(t, backend, time.Minute)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		assertFinished(t)
	})
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

func TestCodexRetryReleasesPreparedAtFirstContentBlock(t *testing.T) {
	oldBackoff := OverloadBackoff
	OverloadBackoff = []time.Duration{0}
	t.Cleanup(func() { OverloadBackoff = oldBackoff })

	for _, mode := range []string{"stream", "collector"} {
		for _, kind := range []string{"thinking", "text", "tool_use"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				prepared := &codex.Prepared{}
				holder := &wsh.CodexRetry{Prepared: prepared}
				var calls int
				holder.Call = func(got *codex.Prepared) (*http.Response, error) {
					calls++
					if got != prepared {
						t.Fatalf("retry got prepared %p, want original %p", got, prepared)
					}
					if holder.Prepared == nil {
						t.Fatal("prepared request was released before content started")
					}
					body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"
					if calls == 1 {
						body += "data: {\"type\":\"error\",\"error\":{\"type\":\"service_unavailable_error\",\"code\":\"server_is_overloaded\",\"message\":\"overloaded\"}}\n\n"
					} else {
						switch kind {
						case "thinking":
							body += "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"i\",\"type\":\"reasoning\"}}\n\n"
						case "text":
							body += "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"i\",\"type\":\"message\"}}\n\n"
						case "tool_use":
							body += "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"i\",\"type\":\"function_call\",\"call_id\":\"c\",\"name\":\"tool\"}}\n\n"
						}
						body += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}

				var col *sse.Collector
				var emitted []string
				dest := sse.Emit(func(event string, _ any) {
					if event == "content_block_start" && holder.Prepared != nil {
						t.Errorf("prepared request was not released before emitting content_block_start")
					}
					emitted = append(emitted, event)
				})
				if mode == "collector" {
					col = &sse.Collector{}
					dest = sse.Emit(func(event string, data any) {
						if event == "content_block_start" && holder.Prepared != nil {
							t.Errorf("prepared request was not released before emitting content_block_start")
						}
						col.Emit(event, data)
					})
				}
				tr := &sse.Translator{Model: "model", Emit: holder.Emit(dest)}
				final := runUpstream(context.Background(), tr,
					mustPreparedResponse(t, holder),
					func() (*http.Response, error) { return holder.CallPrepared() }, "profile", "model")
				defer final.Body.Close()

				if calls != 2 {
					t.Fatalf("calls = %d, want overload then retry", calls)
				}
				if holder.Prepared != nil {
					t.Fatal("prepared request remains after first content_block_start")
				}
				if mode == "stream" {
					if !containsString(emitted, "content_block_start") || !containsString(emitted, "message_stop") {
						t.Fatalf("stream events = %v", emitted)
					}
				} else {
					result, err := col.Result()
					if err != nil || len(result["content"].([]any)) != 1 {
						t.Fatalf("collector result = %#v, err %v", result, err)
					}
				}
			})
		}
	}
}

func mustPreparedResponse(t *testing.T, holder *wsh.CodexRetry) *http.Response {
	t.Helper()
	resp, err := holder.CallPrepared()
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestCodexPreparedRequestIsSerializedOnceForAttempts(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	path := filepath.Join(filepath.Dir(file), "codex.go")
	fset := token.NewFileSet()
	fileAST, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var handler *ast.FuncDecl
	for _, decl := range fileAST.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "HandleCodexMessages" {
			handler = fn
			break
		}
	}
	if handler == nil {
		t.Fatal("HandleCodexMessages not found")
	}
	prepares, retryCalls, againCalls, preparedCalls := 0, 0, 0, 0
	capturedReq := false
	ast.Inspect(handler.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "PrepareRequest":
					prepares++
				case "CallPrepared":
					// codex.CallPrepared is the direct call, retry.CallPrepared an attempt.
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "retry" {
						retryCalls++
					} else {
						preparedCalls++
					}
				case "CallAgain":
					againCalls++
				}
			}
		case *ast.FuncLit:
			// No closure of the handler may capture the converted request.
			ast.Inspect(node.Body, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok && id.Name == "req" {
					capturedReq = true
				}
				return true
			})
		}
		return true
	})
	// retry.CallPrepared: the first call, the short rate-limit retry and the edge-error loop;
	// the again closure goes through CallAgain (websocket or HTTP).
	if prepares != 1 || retryCalls != 3 || againCalls != 1 || preparedCalls != 1 || capturedReq {
		t.Fatalf("handler prepares=%d retry calls=%d again calls=%d direct prepared calls=%d closure captures req=%v; want 1, 3, 1, 1, false", prepares, retryCalls, againCalls, preparedCalls, capturedReq)
	}

	closures, closureCalls := 0, 0
	ast.Inspect(fileAST, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok || lit.Type.Params == nil || len(lit.Type.Params.List) != 1 || len(lit.Type.Params.List[0].Names) != 1 || lit.Type.Params.List[0].Names[0].Name != "p" {
			return true
		}
		found := false
		ast.Inspect(lit.Body, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CallPrepared" {
					found = len(call.Args) == 4
					if found {
						arg, ok := call.Args[3].(*ast.Ident)
						found = ok && arg.Name == "p"
					}
					closureCalls++
				}
			}
			if ident, ok := inner.(*ast.Ident); ok && ident.Name == "req" {
				found = false
			}
			return true
		})
		if found {
			closures++
		}
		return false
	})
	if closures != 1 || closureCalls != 1 {
		t.Fatalf("prepared call closure matches=%d, CallPrepared calls=%d; want one explicit prepared argument and no req capture", closures, closureCalls)
	}
}

func TestCodexPreparedRequestReusedAcrossEdgeRateAndOverloadRetries(t *testing.T) {
	fastRetries(t, 1)
	oldStates, oldWatch, oldStats := obs.TurnStates, obs.PromptWatch, obs.PromptStats
	obs.TurnStates, obs.PromptWatch, obs.PromptStats = &obs.TurnStateStore{}, &obs.CacheWatch{}, &obs.CacheStats{}
	t.Cleanup(func() { obs.TurnStates, obs.PromptWatch, obs.PromptStats = oldStates, oldWatch, oldStats })
	oldBody, oldSlowTotal := codexTestBody, obs.SlowTotal
	codexTestBody = ""
	obs.SlowTotal = 0
	t.Cleanup(func() { codexTestBody, obs.SlowTotal = oldBody, oldSlowTotal })

	const userID = "prepared-request-session"
	const state = "Prepared-State-opaque/Keep-MiXeD"
	seedBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Codex-Turn-State", state)
		turnStateOK(w, "")
	})
	status, _, _, _ := callTurnState(t, turnStateUserBody(userID, `""`, "null"), seedBackend)
	if status != fiber.StatusOK {
		t.Fatalf("seed status = %d", status)
	}

	var calls atomic.Int32
	var bodies [][]byte
	var headers []http.Header
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		headers = append(headers, r.Header.Clone())
		switch calls.Add(1) {
		case 1:
			w.Header().Set("X-Codex-Turn-State", state)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `gateway`)
		case 2:
			w.Header().Set("X-Codex-Turn-State", state)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"r"}}`+"\n\n"+
				`data: {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"overloaded"}}`+"\n\n")
		default:
			w.Header().Set("X-Codex-Turn-State", state)
			okStream(w)
		}
	})

	continuation := turnStateContinuationWith(userID, `""`, "null")
	status, body, _, logs := callTurnState(t, continuation, backend)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, response %q", status, body)
	}
	if calls.Load() != 3 || len(bodies) != 3 {
		t.Fatalf("upstream calls = %d, captured bodies = %d, want three", calls.Load(), len(bodies))
	}
	for i := 1; i < len(bodies); i++ {
		if !bytes.Equal(bodies[0], bodies[i]) {
			t.Errorf("attempt %d body differs from first body", i+1)
		}
	}
	for i, h := range headers {
		if h.Get("session-id") == "" || h.Get("x-codex-routing-hint") == "" {
			t.Errorf("attempt %d missing session/routing headers: %v", i+1, h)
		}
		for key, want := range map[string]string{
			"session-id":           headers[0].Get("session-id"),
			"x-codex-routing-hint": headers[0].Get("x-codex-routing-hint"),
			"thread-id":            headers[0].Get("thread-id"),
			"x-client-request-id":  headers[0].Get("x-client-request-id"),
			"x-codex-turn-state":   state,
		} {
			if got := h.Get(key); got != want {
				t.Errorf("attempt %d %s = %q, want %q", i+1, key, got, want)
			}
		}
	}
	if !strings.Contains(logs, "upstream retry 1/1") || !strings.Contains(logs, "server_is_overloaded") {
		t.Errorf("retry log = %q", logs)
	}

	var rateCalls atomic.Int32
	var rateBodies [][]byte
	var rateHeaders []http.Header
	rateBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rateBodies = append(rateBodies, body)
		rateHeaders = append(rateHeaders, r.Header.Clone())
		w.Header().Set("X-Codex-Turn-State", state)
		if rateCalls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		okStream(w)
	})
	status, body, _, rateLogs := callTurnState(t, continuation, rateBackend)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") || rateCalls.Load() != 2 {
		t.Fatalf("429 retry status %d, calls %d, response %q", status, rateCalls.Load(), body)
	}
	if len(rateBodies) != 2 || len(rateHeaders) != 2 {
		t.Fatalf("429 retry captured bodies=%d headers=%d; want two", len(rateBodies), len(rateHeaders))
	}
	if !bytes.Equal(rateBodies[0], rateBodies[1]) {
		t.Error("429 retry request body differs")
	}
	if rateHeaders[0].Get("session-id") == "" || rateHeaders[0].Get("x-codex-routing-hint") == "" || rateHeaders[0].Get("x-codex-turn-state") != state {
		t.Errorf("429 initial routing headers = %v", rateHeaders[0])
	}
	for key, want := range map[string]string{
		"session-id":           rateHeaders[0].Get("session-id"),
		"x-codex-routing-hint": rateHeaders[0].Get("x-codex-routing-hint"),
		"thread-id":            rateHeaders[0].Get("thread-id"),
		"x-client-request-id":  rateHeaders[0].Get("x-client-request-id"),
		"x-codex-turn-state":   rateHeaders[0].Get("x-codex-turn-state"),
	} {
		if first, second := rateHeaders[0].Get(key), rateHeaders[1].Get(key); first != want || second != want {
			t.Errorf("429 retry %s headers = %q, %q; want %q", key, first, second, want)
		}
	}
	if !strings.Contains(rateLogs, "upstream rate limit: retrying") {
		t.Errorf("429 retry log = %q", rateLogs)
	}
}

func TestCodexTurnKind(t *testing.T) {
	user := map[string]any{"type": "message", "role": "user"}
	assistant := map[string]any{"type": "message", "role": "assistant"}
	call := map[string]any{"type": "function_call"}
	output := map[string]any{"type": "function_call_output"}
	reasoning := map[string]any{"type": "reasoning"}

	for _, tc := range []struct {
		name         string
		input        []any
		continuation bool
		newTurn      bool
	}{
		{"empty", nil, false, false},
		{"first user prompt", []any{user}, false, true},
		{"new prompt after an answer", []any{user, assistant, user}, false, true},
		{"new prompt after a tool loop", []any{user, call, output, assistant, user}, false, true},
		{"tool output last", []any{user, call, output}, true, false},
		{"tool output then reminder text (one converted user message)", []any{user, call, output, user}, true, false},
		{"several outputs then text", []any{user, call, call, output, output, user}, true, false},
		{"ends with an assistant call", []any{user, call}, false, false},
		{"reasoning before the user prompt", []any{user, reasoning, assistant, user}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			continuation, newTurn := codexTurnKind(&codex.Request{Input: tc.input})
			if continuation != tc.continuation || newTurn != tc.newTurn {
				t.Errorf("codexTurnKind = (continuation %v, new turn %v), want (%v, %v)", continuation, newTurn, tc.continuation, tc.newTurn)
			}
		})
	}
	if c, n := codexTurnKind(nil); c || n {
		t.Errorf("nil request = (%v, %v), want (false, false)", c, n)
	}
}
