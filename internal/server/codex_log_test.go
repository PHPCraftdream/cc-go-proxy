package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/PHPCraftdream/cc-go-proxy/pkg/models"
	"github.com/gofiber/fiber/v2"
)

// syncBuf is a log sink the stream-writer goroutine and the test share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// codexCall runs one streaming /v1/messages request through handleCodexMessages
// against backend and returns the status, body, access log and stream log.
func codexCall(t *testing.T, backend http.Handler, headerTimeout time.Duration) (status int, body, access, stream string) {
	t.Helper()
	return codexCallWith(t, backend, &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: headerTimeout}}, nil)
}

// codexCallWith is codexCall with the upstream client and a change to the profile's config given.
func codexCallWith(t *testing.T, backend http.Handler, client *http.Client, tweak func(*config.Config)) (status int, body, access, stream string) {
	t.Helper()
	srv := httptest.NewServer(backend)
	defer srv.Close()

	oldClient, oldLog := codex.HTTPClient, logOut
	t.Cleanup(func() { codex.HTTPClient, logOut = oldClient, oldLog })
	codex.HTTPClient = client
	var sl syncBuf
	logOut = &sl

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sess := &oauth.Session{Store: store, Client: &oauth.Client{}}
	cfg := &config.Config{
		Profile:      "codex",
		ProviderName: "codex",
		Providers:    map[string]*config.Provider{"codex": {Name: "codex", Type: "codex", BaseURL: srv.URL}},
	}
	if tweak != nil {
		tweak(cfg)
	}

	var al syncBuf
	app := fiber.New()
	app.Use(accessLog(&al))
	app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error { return handleCodexMessages(c, cfg, sess) })
	req := httptest.NewRequest(fiber.MethodPost, "/call/secret/v1/messages", strings.NewReader(
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	lastCodexReply = resp.Header.Clone()
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), al.String(), sl.String()
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
	logStreamFailure(&b, time.Now().Add(-2*time.Second), "codex", "claude-opus-5-5 -> gpt-6.1-sol", "stream read error:\n unexpected EOF", "", "")
	got := b.String()
	for _, want := range []string{"stream failed after 2", "[codex] claude-opus-5-5 -> gpt-6.1-sol: stream read error: unexpected EOF\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q lacks %q", got, want)
		}
	}
}

// An error event from the backend is logged raw, with the request ids. (Not
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
	for _, want := range []string{"context_length_exceeded: too long | upstream event: " + ev, "| x-request-id=req_123 cf-ray=ray-ams | conn new", "| request "} {
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
	old, oldBroken := overloadBackoff, brokenBackoff
	overloadBackoff = make([]time.Duration, n)
	brokenBackoff = make([]time.Duration, n)
	for i := 0; i < n; i++ {
		overloadBackoff[i], brokenBackoff[i] = time.Millisecond, time.Millisecond
	}
	t.Cleanup(func() { overloadBackoff, brokenBackoff = old, oldBroken })
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
	logStreamCanceled(&b, time.Now().Add(-3*time.Second), "codex", "a -> b", "conn reused (idle 1s)")
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
	oldT := openAITransport
	t.Cleanup(func() { openAITransport = oldT })

	call := func(profile string, proxy string) {
		t.Helper()
		openAITransport = connpool.NewRouter(0, 1, 1, nil)
		cfg := &config.Config{
			Profile: profile, OpenAIBaseURL: backend.URL, ConnectionsProxy: global,
			Profiles: map[string]*config.Profile{profile: {Name: profile, Proxy: proxy}},
		}
		if _, err := callOpenAIInternal(&models.OpenAIRequest{Model: "m"}, cfg); err != nil {
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
	if c := proxydial.ChoiceFor(upstreamCtx(context.Background(), cfg)); !c.Direct {
		t.Errorf("profile none: %+v", c)
	}
	cfg.Profiles["p"].Proxy = ""
	if c := proxydial.ChoiceFor(upstreamCtx(context.Background(), cfg)); c.URL == nil || c.URL.Host != "127.0.0.1:1080" {
		t.Errorf("inheriting profile: %+v", c)
	}
}

// lastCodexReply is the header of the reply codexCallWith got, for tests that read it.
var lastCodexReply http.Header

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
		got, ok := retryAfter(h, now)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Retry-After %q = %s, %v; want %s, %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
	h := http.Header{"Retry-After": {"2"}, "X-Ratelimit-Remaining": {"0"}, "X-Codex-Primary-Reset-After-Seconds": {"31"}, "Content-Type": {"x"}}
	if got := rateHeaders(h); got != "retry-after=2 x-codex-primary-reset-after-seconds=31 x-ratelimit-remaining=0" {
		t.Errorf("rateHeaders = %q", got)
	}
}

func TestNotRetriedNote(t *testing.T) {
	if got := notRetriedNote(true); got != "not retried: content already sent" {
		t.Errorf("with content = %q", got)
	}
	if got := notRetriedNote(false); got != "" {
		t.Errorf("without content = %q", got)
	}
}
