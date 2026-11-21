package server

import (
	"bytes"
	"fmt"
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
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/gofiber/fiber/v2"
)

// tcpPair is a connected pair of loopback TCP connections: server and client side.
func tcpPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return server, client
}

func TestWatchClientGoneTellsAClosedClient(t *testing.T) {
	server, client := tcpPair(t)
	gone := make(chan struct{}, 1)
	stop := rt.WatchClientGone(server, func() { gone <- struct{}{} })
	defer stop()
	_ = client.Close()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the closed client was not told")
	}
}

func TestWatchClientGoneStopsQuietlyAndLeavesTheConnectionUsable(t *testing.T) {
	server, client := tcpPair(t)
	var calls atomic.Int32
	stop := rt.WatchClientGone(server, func() { calls.Add(1) })
	time.Sleep(2 * rt.ClientWatchTick) // a few idle rounds
	started := time.Now()
	stop()
	stop() // twice is harmless
	if took := time.Since(started); took > 2*rt.ClientWatchTick {
		t.Errorf("stop took %s", took)
	}
	if calls.Load() != 0 {
		t.Fatal("a client that stayed was taken for gone")
	}
	// No deadline is left behind: the connection still reads and writes.
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(server, b[:]); err != nil || b[0] != 'x' {
		t.Fatalf("read after the watch: %q, %v", b[:], err)
	}
	_ = client.Close()
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Error("the watch went on after stop")
	}
}

func TestWatchClientGoneIgnoresAConnectionThatIsNotTCP(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	var calls atomic.Int32
	stop := rt.WatchClientGone(a, func() { calls.Add(1) })
	_ = b.Close()
	time.Sleep(50 * time.Millisecond)
	stop()
	if calls.Load() != 0 {
		t.Error("an in-memory connection was watched")
	}
}

// A request without streaming sends nothing until its reply is complete; when
// its client goes away meanwhile, the upstream call is canceled and the log says
// so, whether the backend's reply had started (headers and a first event in) or
// not (the client left while ccgp waited for the headers: once only a race of
// the first case on a slow runner, which left no canceled line).
func TestNonStreamReplyIsCanceledWhenTheClientGoesAway(t *testing.T) {
	t.Run("reply started", func(t *testing.T) { nonStreamClientLeaves(t, true) })
	t.Run("before the reply", func(t *testing.T) { nonStreamClientLeaves(t, false) })
}

func nonStreamClientLeaves(t *testing.T, started bool) {
	received := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if started {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
			w.(http.Flusher).Flush()
		}
		received <- struct{}{}
		select {
		case <-r.Context().Done():
			canceled <- struct{}{}
		case <-time.After(20 * time.Second):
		}
	}))
	defer backend.Close()

	oldClient, oldLog := codex.HTTPClient, shared.LogOut
	t.Cleanup(func() { codex.HTTPClient, shared.LogOut = oldClient, oldLog })
	codex.HTTPClient = &http.Client{}
	var sl, al syncBuf
	shared.LogOut = &sl
	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sess := &oauth.Session{Store: store, Client: &oauth.Client{}}
	cfg := &config.Config{
		Profile: "codex", ProviderName: "codex", CodexTransport: "http",
		Providers: map[string]*config.Provider{"codex": {Name: "codex", Type: "codex", BaseURL: backend.URL}},
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(rt.AccessLog(&al))
	app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error { return codexh.HandleCodexMessages(c, cfg, sess) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.ShutdownWithTimeout(5 * time.Second) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	_, _ = fmt.Fprintf(conn, "POST /call/secret/v1/messages HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("the backend got no request")
	}
	_ = conn.Close()
	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream call went on after the client left")
	}
	// The access line is printed when the handler returns, after its own lines.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(al.String(), "POST") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(al.String(), "| non-stream") {
		t.Errorf("access line lacks the non-stream note: %q", al.String())
	}
	if !strings.Contains(sl.String(), "non-stream reply canceled by client after") {
		t.Errorf("log lacks the canceled line: %q", sl.String())
	}
	if !started && !strings.Contains(sl.String(), "before the reply started") {
		t.Errorf("log does not say the reply had not started: %q", sl.String())
	}
}

// A client that stays gets its reply on a connection closed after it.
func TestNonStreamReplyClosesTheConnection(t *testing.T) {
	nonStreamBody(t)
	status, body, access, _ := codexCall(t, overloadBackend(new(atomic.Int32), 0), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, `"hi"`) {
		t.Fatalf("status %d, body %q", status, body)
	}
	if !strings.Contains(access, "| non-stream") {
		t.Errorf("access line lacks the non-stream note: %q", access)
	}
	if !lastCodexClose {
		t.Error("the reply did not close its connection")
	}
}

// Data from the client is not a close: the watch ends without a word, and a
// close after it is not taken for the client going away.
func TestWatchClientGoneEndsOnData(t *testing.T) {
	server, client := tcpPair(t)
	var calls atomic.Int32
	stop := rt.WatchClientGone(server, func() { calls.Add(1) })
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * rt.ClientWatchTick)
	_ = client.Close()
	time.Sleep(2 * rt.ClientWatchTick)
	started := time.Now()
	stop()
	if took := time.Since(started); took > rt.ClientWatchTick {
		t.Errorf("stop after the watch ended took %s", took)
	}
	if calls.Load() != 0 {
		t.Error("a client that sent data was taken for gone")
	}
}

// lastCodexClose is whether that reply closed its connection (the client drops the header).
var lastCodexClose bool

func nonStreamBody(t *testing.T) {
	t.Helper()
	codexTestBody = `{"model":"claude-opus-5-5","max_tokens":10,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	t.Cleanup(func() { codexTestBody = "" })
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

// codexCall runs one streaming /v1/messages request through handleCodexMessages
// against backend and returns the status, body, access log and stream log.
// codexTestBody replaces the request the helpers send (default: a streaming one).
var codexTestBody string

func codexCall(t *testing.T, backend http.Handler, headerTimeout time.Duration) (status int, body, access, stream string) {
	t.Helper()
	return codexCallWith(t, backend, &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: headerTimeout}}, nil)
}

// codexCallWith is codexCall with the upstream client and a change to the profile's config given.
func codexCallWith(t *testing.T, backend http.Handler, client *http.Client, tweak func(*config.Config)) (status int, body, access, stream string) {
	t.Helper()
	srv := httptest.NewServer(backend)
	defer srv.Close()

	oldClient, oldLog := codex.HTTPClient, shared.LogOut
	t.Cleanup(func() { codex.HTTPClient, shared.LogOut = oldClient, oldLog })
	codex.HTTPClient = client
	var sl syncBuf
	shared.LogOut = &sl

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
	app.Use(rt.AccessLog(&al))
	app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error { return codexh.HandleCodexMessages(c, cfg, sess) })
	reqBody := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	if codexTestBody != "" {
		reqBody = codexTestBody
	}
	req := httptest.NewRequest(fiber.MethodPost, "/call/secret/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	lastCodexReply, lastCodexClose = resp.Header.Clone(), resp.Close
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), al.String(), sl.String()
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

func fastRetries(t *testing.T, n int) {
	old, oldBroken := codexh.OverloadBackoff, codexh.BrokenBackoff
	codexh.OverloadBackoff = make([]time.Duration, n)
	codexh.BrokenBackoff = make([]time.Duration, n)
	for i := 0; i < n; i++ {
		codexh.OverloadBackoff[i], codexh.BrokenBackoff[i] = time.Millisecond, time.Millisecond
	}
	t.Cleanup(func() { codexh.OverloadBackoff, codexh.BrokenBackoff = old, oldBroken })
}

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

// lastCodexReply is the header of the reply codexCallWith got, for tests that read it.
var lastCodexReply http.Header

// The levels replace each other whole: the provider's proxy beats the global
// one, the profile's own beats the provider's, and none ends the search.
func TestCodexRequestsFollowTheProviderProxy(t *testing.T) {
	var calls atomic.Int32
	backend := overloadBackend(&calls, 0) // always a complete answer
	global, viaGlobal := connectProxy(t)
	prov, viaProv := connectProxy(t)
	own, viaOwn := connectProxy(t)

	run := func(profile, providerProxy string) {
		t.Helper()
		router := connpool.NewRouter(0, 1, 1, nil)
		status, body, _, _ := codexCallWith(t, backend, &http.Client{Transport: router}, func(cfg *config.Config) {
			cfg.Profile = profile
			cfg.ConnectionsProxy = global
			cfg.Providers["codex"].Proxy = providerProxy
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
	tunnels := func() [3]int32 { return [3]int32{viaGlobal.Load(), viaProv.Load(), viaOwn.Load()} }

	run("inherits", prov)
	if got := tunnels(); got != [3]int32{0, 1, 0} {
		t.Fatalf("inherits, provider proxy: tunnels global/provider/own = %v, want 0/1/0", got)
	}
	run("own", prov)
	if got := tunnels(); got != [3]int32{0, 1, 1} {
		t.Fatalf("own beats the provider's: tunnels = %v, want 0/1/1", got)
	}
	run("direct", prov)
	if got := tunnels(); got != [3]int32{0, 1, 1} {
		t.Fatalf("none of the profile: tunnels = %v, want no new tunnel", got)
	}
	run("inherits", config.ProxyNone)
	if got := tunnels(); got != [3]int32{0, 1, 1} {
		t.Fatalf("none of the provider beats the global proxy: tunnels = %v, want no new tunnel", got)
	}
	run("inherits", "")
	if got := tunnels(); got != [3]int32{1, 1, 1} {
		t.Fatalf("no provider proxy: tunnels = %v, want the global one used (1/1/1)", got)
	}
}

// The backend failed after it had sent content: the client already holds part
// of the answer, so ccgp must not start the request over.
func TestOverloadAfterContentIsNotRetried(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"half an answ\"}\n\n" +
			"data: {\"type\":\"error\",\"error\":{\"type\":\"service_unavailable_error\",\"code\":\"server_is_overloaded\",\"message\":\"overloaded\"}}\n\n"))
	})
	status, body, _, stream := codexCall(t, backend, time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "half an answ") || !strings.Contains(body, "event: error") {
		t.Fatalf("status %d, body %q: want the partial answer and the error", status, body)
	}
	if calls.Load() != 1 {
		t.Errorf("backend calls = %d: a request with content already sent was asked again", calls.Load())
	}
	if strings.Contains(stream, "upstream retry") || !strings.Contains(stream, "stream failed") {
		t.Errorf("log %q", stream)
	}
	if !strings.Contains(stream, "| not retried: content already sent") {
		t.Errorf("the log does not say why the request was not asked again: %q", stream)
	}
}

// brokenBackend cuts the connection in the middle of the stream on its first
// brokenCalls calls (after some text when withText), then answers in full.
func brokenBackend(calls *atomic.Int32, brokenCalls int32, withText bool) http.Handler {
	full := overloadBackend(new(atomic.Int32), 0)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > brokenCalls {
			full.ServeHTTP(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		out := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"
		if withText {
			out += "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"half an answ\"}\n\n"
		}
		_, _ = w.Write([]byte(out))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // the connection dies mid-stream
	})
}

// A connection that breaks before the client got any content costs the client
// only a pause: the request is sent again over a new connection.
func TestBrokenConnectionBeforeContentIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, _, stream := codexCall(t, brokenBackend(&calls, 1, false), time.Minute)
	if status != fiber.StatusOK || strings.Contains(body, "event: error") || !strings.Contains(body, "message_stop") || !strings.Contains(body, "\"hi\"") {
		t.Fatalf("status %d, body %q: want a complete stream", status, body)
	}
	if strings.Count(body, "event: message_start") != 1 {
		t.Errorf("body %q: want one message_start", body)
	}
	if calls.Load() != 2 || !strings.Contains(stream, "upstream retry 1/2 in 1ms") || !strings.Contains(stream, "stream read error") {
		t.Errorf("calls %d, log %q", calls.Load(), stream)
	}
	if strings.Contains(stream, "stream failed") {
		t.Errorf("a retried stream is logged as failed: %q", stream)
	}
}

// Once the client holds part of the answer the request is not started over.
func TestBrokenConnectionAfterContentIsNotAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	_, body, _, stream := codexCall(t, brokenBackend(&calls, 10, true), time.Minute)
	if !strings.Contains(body, "half an answ") || !strings.Contains(body, "event: error") {
		t.Fatalf("body %q: want the partial answer and the error", body)
	}
	if calls.Load() != 1 || strings.Contains(stream, "upstream retry") || !strings.Contains(stream, "stream failed") {
		t.Errorf("calls %d, log %q", calls.Load(), stream)
	}
}

// A backend that keeps cutting the connection gets its two retries, then the
// client gets the error.
func TestBrokenConnectionRetriesRunOut(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	_, body, _, stream := codexCall(t, brokenBackend(&calls, 10, false), time.Minute)
	if !strings.Contains(body, "event: error") {
		t.Fatalf("body %q: want the error after the retries", body)
	}
	if calls.Load() != 3 || !strings.Contains(stream, "upstream retry 2/2") || !strings.Contains(stream, "stream failed") {
		t.Errorf("calls %d, log %q", calls.Load(), stream)
	}
}
