package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/gofiber/fiber/v2"
)

func logLine(t *testing.T, handler fiber.Handler) string {
	t.Helper()
	var buf bytes.Buffer
	app := fiber.New()
	app.Use(rt.AccessLog(&buf))
	app.Post("/call/:token/v1/messages", handler)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/call/secret/v1/messages", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return buf.String()
}

func TestAccessLogShowsErrorReason(t *testing.T) {
	got := logLine(t, func(c *fiber.Ctx) error {
		return shared.AnthropicError(c, fiber.StatusBadGateway, "api_error", "Post \"https://x/responses\":\n  net/http: timeout awaiting response headers")
	})
	for _, want := range []string{
		" 502 - ", "POST /call/***/v1/messages",
		`: Post "https://x/responses": net/http: timeout awaiting response headers` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("token leaked: %q", got)
	}
}

func TestAccessLogHasNoReasonOnSuccess(t *testing.T) {
	got := logLine(t, func(c *fiber.Ctx) error {
		c.Locals(shared.LocalsErr, "left over")
		return c.SendStatus(fiber.StatusOK)
	})
	if !strings.Contains(got, " 200 - ") || strings.Contains(got, ": ") {
		t.Errorf("log = %q", got)
	}
}

// The model lookup and the health check are repeated all day: a success says
// nothing and gets no line, a failure does.
func TestAccessLogSkipsSuccessfulQuietRoutes(t *testing.T) {
	status := fiber.StatusOK
	var buf bytes.Buffer
	app := fiber.New()
	app.Use(rt.AccessLog(&buf))
	h := func(c *fiber.Ctx) error { return c.SendStatus(status) }
	app.Get("/health", h)
	app.Get("/call/:token/model", h)
	app.Post("/call/:token/model", h)
	app.Get("/call/:token/other", h)

	hit := func(method, path string) {
		t.Helper()
		resp, err := app.Test(httptest.NewRequest(method, path, nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	hit(fiber.MethodGet, "/health")
	hit(fiber.MethodGet, "/call/secret/model")
	if buf.Len() != 0 {
		t.Fatalf("successful quiet routes were logged: %q", buf.String())
	}
	hit(fiber.MethodGet, "/call/secret/other")
	hit(fiber.MethodPost, "/call/secret/model")
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Fatalf("log = %q, want a line for another route and for a POST", buf.String())
	}
	buf.Reset()
	status = fiber.StatusUnauthorized
	hit(fiber.MethodGet, "/call/secret/model")
	hit(fiber.MethodGet, "/health")
	if got := buf.String(); !strings.Contains(got, " 401 - ") || strings.Count(got, "\n") != 2 || strings.Contains(got, "secret") {
		t.Errorf("failed quiet routes: log = %q", got)
	}
}

func TestOneLine(t *testing.T) {
	if got := shared.OneLine("a \n\t b", 10); got != "a b" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("я", 400)
	got := shared.OneLine(long, 300)
	if !strings.HasSuffix(got, "…") || len(got) > 300+len("…") {
		t.Errorf("len %d", len(got))
	}
}

func TestHealthPayloadJSONFields(t *testing.T) {
	f := &rt.InFlight{}
	b, err := json.Marshal(healthPayload(f, "/some/ccgp", 12345))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"status", "service", "version", "pid", "exe", "exe_mod", "active"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing field %q in %s", k, b)
		}
	}
	if got["exe"] != "/some/ccgp" || got["exe_mod"] != float64(12345) || got["active"] != float64(0) {
		t.Errorf("unexpected values: %s", b)
	}
}

func TestExeInfoReportsTestBinary(t *testing.T) {
	path, mod := exeInfo()
	if path == "" {
		t.Fatal("exe path is empty")
	}
	if mod == 0 {
		t.Error("mod time of the test binary should be known")
	}
}

func TestInFlightMiddlewareCountsCallRequests(t *testing.T) {
	f := &rt.InFlight{}
	app := fiber.New()
	app.Use(f.Middleware())
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(healthPayload(f, "", 0))
	})
	release := make(chan struct{})
	app.Post(shared.CallPrefix+":token/v1/messages", func(c *fiber.Ctx) error {
		<-release
		return c.SendString("ok")
	})

	done := make(chan error, 1)
	go func() {
		_, err := app.Test(httptest.NewRequest("POST", "/call/tok/v1/messages", nil), -1)
		done <- err
	}()

	waitForCount(t, f, 1)

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h["active"] != float64(1) {
		t.Errorf("active = %v, want 1 while request in flight", h["active"])
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := f.Count(); n != 0 {
		t.Errorf("active = %d after completion, want 0", n)
	}
}

// TestInFlightSurvivesHandlerReturnThroughStreamWriter is the streaming case:
// the handler returns immediately after installing a body stream writer, and
// the request must stay counted until the writer finishes.
func TestInFlightSurvivesHandlerReturnThroughStreamWriter(t *testing.T) {
	f := &rt.InFlight{}
	app := fiber.New()
	app.Use(f.Middleware())
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.JSON(healthPayload(f, "", 0))
	})
	writerStarted := make(chan struct{})
	release := make(chan struct{})
	app.Post(shared.CallPrefix+":token/v1/messages", func(c *fiber.Ctx) error {
		rel := rt.DeferActive(c)
		c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
			defer rel()
			close(writerStarted)
			<-release
			_, _ = w.WriteString("done")
		})
		return nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := app.Test(httptest.NewRequest("POST", "/call/tok/v1/messages", nil), -1)
		done <- err
	}()

	// The handler has returned (counter would have dropped without the
	// takeover); the writer is running and must keep the request counted.
	select {
	case <-writerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("stream writer never started")
	}
	waitForCount(t, f, 1)

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h["active"] != float64(1) {
		t.Errorf("active = %v, want 1 while stream writer runs", h["active"])
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream request never finished")
	}
	waitForCount(t, f, 0)
}

// TestInFlightReleasesOnHandlerReturnAndNeverBelowZero: a plain handler
// releases on return, and repeated releases cannot push the counter negative.
func TestInFlightReleasesOnHandlerReturnAndNeverBelowZero(t *testing.T) {
	f := &rt.InFlight{}
	app := fiber.New()
	app.Use(f.Middleware())
	app.Post(shared.CallPrefix+":token/v1/messages", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})
	if _, err := app.Test(httptest.NewRequest("POST", "/call/tok/v1/messages", nil)); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, f, 0)

	// A taken-over release can be called twice and the middleware releases
	// again after the handler returns: the guard must keep the count at 0.
	app.Post(shared.CallPrefix+":token/twice/v1/messages", func(c *fiber.Ctx) error {
		rel := rt.DeferActive(c)
		rel()
		rel()
		return c.SendString("ok")
	})
	if _, err := app.Test(httptest.NewRequest("POST", "/call/tok/twice/v1/messages", nil)); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, f, 0)
}

func TestInFlightMiddlewareIgnoresHealth(t *testing.T) {
	f := &rt.InFlight{}
	app := fiber.New()
	app.Use(f.Middleware())
	started := make(chan struct{})
	block := make(chan struct{})
	app.Get("/health", func(c *fiber.Ctx) error {
		if n := f.Count(); n != 0 {
			t.Errorf("/health counted as in flight while open: %d", n)
		}
		close(started)
		<-block
		return c.SendString("ok")
	})

	done := make(chan error, 1)
	go func() {
		_, err := app.Test(httptest.NewRequest("GET", "/health", nil), -1)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("health handler never started")
	}
	waitForCount(t, f, 0)

	close(block)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("health request never finished")
	}
}

// waitForCount polls until the counter reaches want; a failing request would
// never reach it, so the wait must be bounded.
func waitForCount(t *testing.T, f *rt.InFlight, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.Count() != want {
		if time.Now().After(deadline) {
			t.Fatalf("in-flight count = %d, want %d", f.Count(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const authTestToken = "0123456789abcdef0123456789abcdef"

func newAuthApp(build shared.ProfileBuilder) *fiber.App {
	app := fiber.New()
	idx := creds.StaticIndex("work", authTestToken)
	app.Post(shared.CallPrefix+":token/v1/messages", shared.RequireProfile(idx, build), func(c *fiber.Ctx) error {
		return c.SendString(shared.RequestConfig(c).Profile)
	})
	return app
}

func okBuild(profile string) (*config.Config, error) {
	return &config.Config{Profile: profile}, nil
}

func TestRequireProfile(t *testing.T) {
	tests := []struct {
		name string
		path string
		want int
	}{
		{"valid token", "/call/" + authTestToken + "/v1/messages", fiber.StatusOK},
		{"wrong token", "/call/" + authTestToken[:31] + "x/v1/messages", fiber.StatusNotFound},
		{"short token", "/call/abc/v1/messages", fiber.StatusNotFound},
		{"token prefix", "/call/" + authTestToken + "extra/v1/messages", fiber.StatusNotFound},
		{"no prefix", "/v1/messages", fiber.StatusNotFound},
		{"empty token", "/call//v1/messages", fiber.StatusNotFound},
	}
	app := newAuthApp(okBuild)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := app.Test(httptest.NewRequest("POST", tt.path, nil))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestRequireProfileBuildError(t *testing.T) {
	app := newAuthApp(func(string) (*config.Config, error) { return nil, errors.New("not logged in") })
	resp, err := app.Test(httptest.NewRequest("POST", "/call/"+authTestToken+"/v1/messages", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestMaskToken(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/call/secret/v1/messages", "/call/***/v1/messages"},
		{"/call/secret", "/call/***"},
		{"/call/secret/", "/call/***/"},
		{"/health", "/health"},
		{"/v1/messages", "/v1/messages"},
	}
	for _, tt := range tests {
		if got := shared.MaskToken(tt.in); got != tt.want {
			t.Errorf("maskToken(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// postBody sends a body of size bytes and returns the status, or the error
// app.Test reports when the server refuses the body before any route runs.
func postBody(t *testing.T, app *fiber.App, size int) (int, error) {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/call/x/v1/messages", bytes.NewReader(make([]byte, size)))
	resp, err := app.Test(req, -1)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

func TestBodyLimitAllowsLongConversations(t *testing.T) {
	app := fiber.New(appConfig((&config.Config{}).MaxBodyBytes()))
	// Past fiber's 4 MiB default; no route is registered, so a request that gets
	// through the body check ends in 404.
	if got, err := postBody(t, app, 5<<20); err != nil || got != fiber.StatusNotFound {
		t.Fatalf("5 MiB body: status %d, err %v; want %d", got, err, fiber.StatusNotFound)
	}
}

func TestBodyOverLimitIsRejected(t *testing.T) {
	cfg := &config.Config{MaxBodyMB: 1}
	app := fiber.New(appConfig(cfg.MaxBodyBytes()))
	// fasthttp refuses the body before any route runs: app.Test then reports the
	// error instead of a response (a real client gets 413).
	got, err := postBody(t, app, 2<<20)
	rejected := got == fiber.StatusRequestEntityTooLarge || (err != nil && strings.Contains(err.Error(), "exceeds the given limit"))
	if !rejected {
		t.Fatalf("2 MiB body over a 1 MiB limit: status %d, err %v; want a rejection", got, err)
	}
}

// The error handler logs refused bodies and headers (token masked) and nothing else.
func TestErrorHandlerLogsRejections(t *testing.T) {
	old := shared.LogOut
	t.Cleanup(func() { shared.LogOut = old })

	run := func(code int) string {
		var buf syncBuf
		shared.LogOut = &buf
		app := fiber.New(appConfig(1 << 20))
		app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error {
			return fiber.NewError(code, "refused")
		})
		resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/call/secret/v1/messages", strings.NewReader("abcd")), -1)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != code {
			t.Fatalf("status = %d, want %d", resp.StatusCode, code)
		}
		return buf.String()
	}

	got := run(fiber.StatusRequestEntityTooLarge)
	for _, want := range []string{"413 - rejected POST /call/***/v1/messages", "content-length 4", "raise listen.max_body_mb"} {
		if !strings.Contains(got, want) {
			t.Errorf("413 log %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("token leaked: %q", got)
	}
	if got := run(fiber.StatusRequestHeaderFieldsTooLarge); !strings.Contains(got, "431 - rejected") || strings.Contains(got, "max_body_mb") {
		t.Errorf("431 log = %q", got)
	}
	if got := run(fiber.StatusBadRequest); got != "" {
		t.Errorf("400 logged: %q", got)
	}
}

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
