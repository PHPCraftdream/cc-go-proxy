package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/chat"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
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

func TestApplyConnectionLimits(t *testing.T) {
	t.Cleanup(func() { applyConnectionLimits(&config.Config{}) })
	if a, n := applyConnectionLimits(&config.Config{ConnectionsActive: 3, ConnectionsTotal: 4}); a != 3 || n != 4 {
		t.Fatalf("applied %d/%d", a, n)
	}
	for name, rt := range map[string]http.RoundTripper{
		"codex": codex.HTTPClient.Transport, "anthropic": chat.UpstreamClient.Transport, "openai": rt.OpenAITransport,
	} {
		tr, ok := rt.(*connpool.Router)
		if !ok {
			t.Errorf("%s: transport %T, want *connpool.Router", name, rt)
			continue
		}
		if a, n := tr.Limits(); a != 3 || n != 4 {
			t.Errorf("%s: limits %d/%d, want 3/4", name, a, n)
		}
	}
}

func TestWriteUpstreamStats(t *testing.T) {
	ts := upstreamTransports()
	if len(ts) != 3 {
		t.Fatalf("transports = %d, want codex, anthropic and openai", len(ts))
	}
	for _, r := range ts {
		r.Stats() // other tests in the package count too: start from zero
	}
	var b strings.Builder
	writeUpstreamStats(&b, time.Unix(0, 0), ts)
	if b.Len() != 0 {
		t.Errorf("quiet transports logged %q", b.String())
	}

	// A client that retried a dropped connection gets a line.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = c.Close()
		}
	}))
	defer srv.Close()
	router := connpool.NewRouter(0, 1, 1, nil)
	resp, err := (&http.Client{Transport: router}).Post(srv.URL, "text/plain", strings.NewReader("x"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a dropped connection produced an answer")
	}
	b.Reset()
	writeUpstreamStats(&b, time.Unix(0, 0), map[string]*connpool.Router{"codex": router})
	if got := b.String(); !strings.Contains(got, "upstream stats [codex]: ") || !strings.Contains(got, "0 after connect failures, 2 after broken connections") {
		t.Errorf("stats line = %q", got)
	}
}

// The profile and connection settings are read again when config.ktav changes:
// a profile's proxy, the limits and the global proxy all follow the file, a
// broken file leaves the previous settings in force.
func TestConfigReloadAppliesProxyAndLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ktav")
	t.Setenv(config.ConfigFileEnv, path)
	t.Cleanup(func() {
		proxydial.SetDefault(nil)
		applyConnectionLimits(&config.Config{})
	})
	text := func(top, profileProxy string) string {
		p := ""
		if profileProxy != "" {
			p = "        proxy: " + profileProxy + "\n"
		}
		return top + "providers: [\n    {\n        name: p\n        type: codex\n    }\n]\n\n" +
			"profiles: [\n    {\n        name: a\n        provider: p\n" + p + "    }\n    {\n        name: b\n        provider: p\n    }\n]\n"
	}
	t0 := time.Now().Add(-time.Hour)
	write := func(body string, n int) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := t0.Add(time.Duration(n) * 3 * time.Second) // a new modification time each time
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	write(text("", ""), 0)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	src := newCfgSource(cfg)
	if src.get() != cfg {
		t.Fatal("an unchanged file produced a new config")
	}

	write(text("connections.active: 3\nconnections.total: 4\nconnections.proxy: http://corp:3128\n\n", "none"), 1)
	got := src.get()
	if got == cfg {
		t.Fatal("the changed file was not read")
	}
	if a, n := got.ConnectionLimits(); a != 3 || n != 4 {
		t.Errorf("limits = %d/%d, want 3/4", a, n)
	}
	for name, r := range upstreamTransports() {
		if a, n := r.Limits(); a != 3 || n != 4 {
			t.Errorf("%s router limits = %d/%d after the reload, want 3/4", name, a, n)
		}
	}
	if c := got.ProxyChoice("a"); !c.Direct {
		t.Errorf("profile a (proxy none) = %+v, want direct", c)
	}
	if c := got.ProxyChoice("b"); c.URL == nil || c.URL.Host != "corp:3128" {
		t.Errorf("profile b (inherits) = %+v, want the global proxy", c)
	}
	if d := proxydial.Default(); d == nil || d.Host != "corp:3128" {
		t.Errorf("default proxy = %v, want the global one", d)
	}

	write("providers: [\n", 2) // broken: the previous settings stay
	if again := src.get(); again != got {
		t.Error("a broken file replaced the working config")
	}
	if a, _ := got.ConnectionLimits(); a != 3 {
		t.Errorf("limits changed to %d", a)
	}

	write(text("connections.active: 5\n\n", "socks5://other:1080"), 3)
	fixed := src.get()
	if fixed == got {
		t.Fatal("the repaired file was not read")
	}
	if a, n := fixed.ConnectionLimits(); a != 5 || n != 10 {
		t.Errorf("limits = %d/%d, want 5/10 (total back at its default)", a, n)
	}
	if c := fixed.ProxyChoice("a"); c.URL == nil || c.URL.Host != "other:1080" {
		t.Errorf("profile a = %+v, want its own proxy", c)
	}
	if c := fixed.ProxyChoice("b"); c.URL != nil || c.Direct {
		t.Errorf("profile b = %+v, want the environment's way (no global proxy any more)", c)
	}
	if proxydial.Default() != nil {
		t.Error("the removed global proxy is still the default")
	}
}

// The monitoring reads these lines; their shape is a contract.
func TestLogLineShapes(t *testing.T) {
	start := time.Now().Add(-1500 * time.Millisecond)
	line := func(f func(w io.Writer)) string {
		var b bytes.Buffer
		f(&b)
		return b.String()
	}
	hms := `\[\d\d:\d\d:\d\d\] `
	tests := []struct {
		name, got, want string
	}{
		{"failure", line(func(w io.Writer) {
			shared.LogStreamFailure(w, start, "stream", "codex", "a -> b", "boom", `{"type":"error"}`, "x-request-id=r1 | conn new")
		}), `^` + hms + `stream failed after \S+ \[codex\] a -> b: boom \| upstream event: \{"type":"error"\} \| x-request-id=r1 \| conn new\n$`},
		{"failure without details", line(func(w io.Writer) { shared.LogStreamFailure(w, start, "stream", "codex", "a -> b", "boom", "", "") }),
			`^` + hms + `stream failed after \S+ \[codex\] a -> b: boom\n$`},
		{"slow", line(func(w io.Writer) { rt.LogSlowStream(w, start, "stream", "codex", "a -> b", "conn reused | total 3m") }),
			`^` + hms + `slow stream \S+ \[codex\] a -> b: conn reused \| total 3m\n$`},
		{"canceled", line(func(w io.Writer) { rt.LogStreamCanceled(w, start, "stream", "codex", "a -> b", "conn new") }),
			`^` + hms + `stream canceled by client after \S+ \[codex\] a -> b: conn new\n$`},
		{"overload retry", line(func(w io.Writer) {
			rt.LogUpstreamRetry(w, "codex", "a -> b", 1, 2, 2*time.Second, "overloaded", `{"x":1}`, "cf-ray=1")
		}), `^` + hms + `upstream retry 1/2 in 2s \[codex\] a -> b: overloaded \| upstream event: \{"x":1\} \| cf-ray=1\n$`},
		{"reconnect", line(func(w io.Writer) {
			obs.LogReconnect(w, "codex", "a -> b", 2, 1500*time.Millisecond, "wsasend: aborted", "reused, idle 52s, pool 8m12s", "")
		}), `^` + hms + `upstream reconnect 2 after 1\.5s \[codex\] a -> b: wsasend: aborted \| the failed try was on a reused, idle 52s, pool 8m12s connection\n$`},
		{"rate limit", line(func(w io.Writer) { shared.LogRateRetry(w, "codex", "a -> b", 2*time.Second, "retry-after=2") }),
			`^` + hms + `upstream rate limit: retrying in 2s \[codex\] a -> b: retry-after=2\n$`},
	}
	for _, tt := range tests {
		if !regexp.MustCompile(tt.want).MatchString(tt.got) {
			t.Errorf("%s: line %q does not match %s", tt.name, tt.got, tt.want)
		}
	}
}

// A proxy password must never reach the log or the console.
func TestProxyTextHidesThePassword(t *testing.T) {
	cfg := &config.Config{ConnectionsProxy: "socks5://me:s3cret@proxy.example:1080"}
	if got := proxyText(cfg); got != "socks5://me:***@proxy.example:1080" || strings.Contains(got, "s3cret") {
		t.Errorf("proxyText = %q", got)
	}
	if got := proxyText(&config.Config{}); got != "none" {
		t.Errorf("no proxy: %q", got)
	}
}

// The route must be mounted by the same function Start uses, behind auth.
func TestRegisterCallRoutesMountsModelRoute(t *testing.T) {
	app := fiber.New()
	registerCallRoutes(app, shared.RequireProfile(creds.StaticIndex("work", authTestToken),
		func(string) (*config.Config, error) {
			return &config.Config{Profile: "work", DefaultModel: "mapped"}, nil
		}),
		shared.NewSessions(creds.Store{Dir: t.TempDir()}))

	status, body := routeGet(t, app, "/call/"+authTestToken+"/model?requested=claude-opus-5-5")
	if status != fiber.StatusOK || !strings.Contains(body, `"backendModel":"mapped"`) {
		t.Fatalf("mounted route: status %d body %s", status, body)
	}
	if status, _ := routeGet(t, app, "/call/wrong/model?requested=claude-opus-5-5"); status != fiber.StatusNotFound {
		t.Fatalf("unknown token: status %d, want 404", status)
	}
	if status, body := routeGet(t, app, "/call/"+authTestToken+"/"); status != fiber.StatusOK || !strings.Contains(body, `"profile":"work"`) {
		t.Fatalf("info route: status %d body %s", status, body)
	}
}

func routeGet(t *testing.T, app *fiber.App, path string) (int, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}
