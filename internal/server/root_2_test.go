package server

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/chat"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/gofiber/fiber/v2"
)

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

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The header values that go upstream must not alias fasthttp's request buffer:
// the HTTP/2 transport keeps them in its HPACK table, and strings that change
// under it panic the server ("id (N) <= evictCount (M)", in a goroutine of
// net/http that cannot be recovered). A value read from the request must still
// be what it was after later requests have been parsed.
func TestForwardedHeaderStringsDoNotAliasTheRequestBuffer(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			var kept []string // what the transport was given, as it was given
			old := chat.UpstreamClient
			chat.UpstreamClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				kept = append(kept, r.Header.Get("anthropic-beta"), r.Header.Get("anthropic-version"), r.Header.Get("Accept"))
				return &http.Response{
					StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{}`)), Request: r,
				}, nil
			})}
			t.Cleanup(func() { chat.UpstreamClient = old })

			app := compatApp(compatConfig("http://upstream.invalid/api", nil))
			call := func(beta, version, accept string) {
				t.Helper()
				req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"m","messages":[]}`))
				req.Header.Set("anthropic-beta", beta)
				req.Header.Set("anthropic-version", version)
				req.Header.Set("Accept", accept)
				resp, err := app.Test(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			// Values of the same lengths, so that a buffer reused in place is overwritten.
			call("beta-aaaaaaaaaaaaaaaaaa", "2023-06-01", "text/event-stream")
			call("beta-bbbbbbbbbbbbbbbbbb", "2024-01-01", "application/json")
			call("beta-cccccccccccccccccc", "2025-02-02", "text/plain-xxxxxx")

			want := []string{
				"beta-aaaaaaaaaaaaaaaaaa", "2023-06-01", "text/event-stream",
				"beta-bbbbbbbbbbbbbbbbbb", "2024-01-01", "application/json",
				"beta-cccccccccccccccccc", "2025-02-02", "text/plain-xxxxxx",
			}
			if len(kept) != len(want) {
				t.Fatalf("the transport saw %d values, want %d: %q", len(kept), len(want), kept)
			}
			for i := range want {
				if kept[i] != want[i] {
					t.Errorf("value %d changed after the request: %q, want %q", i, kept[i], want[i])
				}
			}
		})
	}
}

func TestOwnStringIsACopy(t *testing.T) {
	input := string([]byte("abc"))
	output := shared.OwnString(input)
	if output != input {
		t.Fatalf("ownString = %q, want %q", output, input)
	}
	if unsafe.StringData(input) == unsafe.StringData(output) {
		t.Error("ownString returned the input string storage")
	}
}

// fiberStringMethods return strings that alias the request buffer.
var fiberStringMethods = map[string]bool{
	"Get": true, "Params": true, "Query": true, "Cookies": true, "FormValue": true,
	"Hostname": true, "IP": true, "Path": true, "OriginalURL": true, "BaseURL": true,
}

// A string read from the fiber context may be kept (in a variable, a header, a
// map) only as a copy: every such call whose result is assigned must be wrapped
// in ownString. Using one in place (a comparison, an argument) is fine: it is
// gone when the handler returns. The check reads the source of this package and its subpackages.
func TestFiberStringsAreCopiedBeforeTheyAreKept(t *testing.T) {
	var paths []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range paths {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		unsafeCall := func(e ast.Expr) (string, bool) {
			call, ok := e.(*ast.CallExpr)
			if !ok {
				return "", false
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !fiberStringMethods[sel.Sel.Name] {
				return "", false
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "c" {
				return "", false
			}
			return sel.Sel.Name, true
		}
		report := func(rhs []ast.Expr) {
			for _, r := range rhs {
				if m, bad := unsafeCall(r); bad {
					t.Errorf("%s: c.%s(...) is assigned without ownString: the string aliases the request buffer",
						fset.Position(r.Pos()), m)
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				report(n.Rhs)
			case *ast.ValueSpec:
				report(n.Values)
			case *ast.KeyValueExpr:
				report([]ast.Expr{n.Value})
			case *ast.CallExpr:
				// A string put into a header or a map-like setter.
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Set" || sel.Sel.Name == "Add") && len(n.Args) == 2 {
					report(n.Args[1:])
				}
			}
			return true
		})
	}
	if checked < 5 {
		t.Fatalf("only %d source files read: the check does not see the package", checked)
	}
}

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
// codexTestBody replaces the request the helpers send (default: a streaming one).
var codexTestBody string

// lastCodexReply is the header of the reply codexCallWith got, for tests that read it.
var lastCodexReply http.Header

func compatApp(cfg *config.Config) *fiber.App {
	app := fiber.New()
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return chat.HandleAnthropicCompat(c, cfg) })
	app.Post("/v1/messages/count_tokens", func(c *fiber.Ctx) error { return chat.HandleAnthropicCountTokens(c, cfg) })
	return app
}

func compatConfig(upstreamURL string, efforts map[string]string) *config.Config {
	return &config.Config{
		Profile: "zai", ProviderName: "zai", ProviderType: config.ProviderTypeAnthropicCompat,
		OpenAIAPIKey: "zai-key",
		Providers: map[string]*config.Provider{
			"zai": {Name: "zai", Type: config.ProviderTypeAnthropicCompat, BaseURL: upstreamURL, EffortMap: efforts},
		},
		ModelRules: []config.ModelRule{
			{Match: "*fable*", Model: "glm-5.3", ContextWindow: 1000000},
			{Match: "*", Model: "glm-5.3-flash", ContextWindow: 1000000},
		},
	}
}

const sampleRequest = `{
	"model": "claude-sonnet-5-5",
	"max_tokens": 128000,
	"stream": true,
	"temperature": 0.25,
	"system": [{"type":"text","text":"You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5. <b>keep</b>"}],
	"messages": [
		{"role":"system","content":[{"type":"text","text":"ctx You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5."}]},
		{"role":"user","content":"hello & goodbye"}
	],
	"thinking": {"type":"adaptive","display":"omitted"},
	"output_config": {"effort": "xhigh"},
	"tools": [{"name":"Read","input_schema":{"type":"object"}}]
}`

func peakConfig(upstreamURL, tz string, windows ...config.PeakWindow) *config.Config {
	cfg := compatConfig(upstreamURL, nil)
	cfg.Providers["zai"].Peak = windows
	cfg.Providers["zai"].PeakTZ = tz
	return cfg
}

// 2026-11-03 is a Tuesday.
func shanghai(day, hour, min int) time.Time {
	return time.Date(2026, 11, day, hour, min, 0, 0, time.FixedZone("+08:00", 8*3600))
}

func TestPeakRefusalNamesWindowAndResumeTime(t *testing.T) {
	cfg := peakConfig("http://x", "+08:00", config.PeakWindow{Days: "mon-fri", From: "14:00", To: "18:00"})
	pauses := peak.NewPauses(t.TempDir())
	msg := peakRefusal(cfg, pauses, shanghai(3, 15, 42))
	for _, want := range []string{
		"Peak hours of provider zai: mon-fri 14:00-18:00 (+08:00 time).",
		"Requests resume at 18:00 +08:00 (",
		" your time, in 2h 18m)",
		"ccgp --peak zai pause",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	for name, now := range map[string]time.Time{
		"before": shanghai(3, 13, 59), "at the end": shanghai(3, 18, 0), "weekend": shanghai(7, 15, 0),
	} {
		if m := peakRefusal(cfg, pauses, now); m != "" {
			t.Errorf("%s: refused: %s", name, m)
		}
	}
}

func TestPeakRefusalFollowsThePause(t *testing.T) {
	cfg := peakConfig("http://x", "+08:00", config.PeakWindow{From: "14:00", To: "18:00"})
	pauses := peak.NewPauses(t.TempDir())
	now := shanghai(3, 15, 0)
	if err := pauses.Set("zai", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if m := peakRefusal(cfg, pauses, now); m != "" {
		t.Errorf("refused during a pause: %s", m)
	}
	if m := peakRefusal(cfg, pauses, now.Add(90*time.Minute)); m == "" {
		t.Error("the pause did not end")
	}
	if err := pauses.Set("zai", time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	if m := peakRefusal(cfg, pauses, now); m == "" {
		t.Error("resume did not end the pause")
	}
}
