package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/chat"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/gofiber/fiber/v2"
)

func TestPeakRefusalNeedsPeakHours(t *testing.T) {
	pauses := peak.NewPauses(t.TempDir())
	now := shanghai(3, 15, 0)
	if m := peakRefusal(peakConfig("http://x", ""), pauses, now); m != "" {
		t.Errorf("a provider without peak hours was refused: %s", m)
	}
	cfg := peakConfig("http://x", "", config.PeakWindow{From: "00:00", To: "24:00"})
	if m := peakRefusal(cfg, pauses, now); !strings.Contains(m, "cover all time") {
		t.Errorf("a window over all time: %q", m)
	}
	cfg.ProviderName = "unknown"
	if m := peakRefusal(cfg, pauses, now); m != "" {
		t.Errorf("an unknown provider was refused: %s", m)
	}
	if m := peakRefusal(&config.Config{}, pauses, now); m != "" {
		t.Errorf("an empty config was refused: %s", m)
	}
}

func TestPeakBanner(t *testing.T) {
	cfg := peakConfig("http://x", "Asia/Shanghai",
		config.PeakWindow{Days: "mon-fri", From: "14:00", To: "18:00"}, config.PeakWindow{From: "20:00", To: "23:00"})
	cfg.Providers["codex"] = &config.Provider{Name: "codex", Type: config.ProviderTypeCodex}
	got := peakBanner(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "peak hours of zai: mon-fri 14:00-18:00, 20:00-23:00 (Asia/Shanghai)") {
		t.Errorf("banner = %q", got)
	}
}

// The route answers a message in peak hours with an HTTP error before the
// provider is asked; token counting goes on; a pause lets messages through.
func TestPeakRouteRefusesMessages(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	defer up.Close()
	old := chat.UpstreamClient
	chat.UpstreamClient = &http.Client{Transport: connpool.NewRouter(0, 1, 1, nil)}
	t.Cleanup(func() { chat.UpstreamClient = old })

	cfg := peakConfig(up.URL, "UTC", config.PeakWindow{From: "00:00", To: "24:00"})
	ss := shared.NewSessions(creds.Store{Dir: t.TempDir()})
	app := fiber.New()
	registerCallRoutes(app, shared.RequireProfile(creds.StaticIndex("zai", authTestToken),
		func(string) (*config.Config, error) { return cfg, nil }), ss)

	post := func(path string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(fiber.MethodPost, "/call/"+authTestToken+path, strings.NewReader(sampleRequest))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	status, body := post("/v1/messages")
	var reply struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	if status != fiber.StatusBadRequest || reply.Type != "error" || reply.Error.Type != "invalid_request_error" ||
		!strings.Contains(reply.Error.Message, "Peak hours of provider zai") {
		t.Errorf("message in peak hours: %d %s", status, body)
	}
	if hits.Load() != 0 {
		t.Errorf("the provider was asked %d times during peak hours", hits.Load())
	}

	if status, _ := post("/v1/messages/count_tokens"); status != fiber.StatusOK || hits.Load() != 1 {
		t.Errorf("token counting: status %d, provider asked %d times, want 200 and 1", status, hits.Load())
	}

	if err := ss.Pauses().Set("zai", time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	if status, body := post("/v1/messages"); status != fiber.StatusOK || hits.Load() != 2 {
		t.Errorf("message during a pause: %d %s, provider asked %d times, want 200 and 2", status, body, hits.Load())
	}
}

// Every provider type goes through the same gate, before its own handler.
func TestPeakRouteRefusesEveryProviderType(t *testing.T) {
	for _, typ := range []string{config.ProviderTypeCodex, config.ProviderTypeCodexAPI, config.ProviderTypeAnthropicCompat} {
		cfg := &config.Config{
			Profile: "p", ProviderName: "p", ProviderType: typ,
			Providers: map[string]*config.Provider{"p": {
				Name: "p", Type: typ, BaseURL: "http://127.0.0.1:1",
				Peak: []config.PeakWindow{{From: "00:00", To: "24:00"}}, PeakTZ: "UTC",
			}},
		}
		app := fiber.New()
		registerCallRoutes(app, shared.RequireProfile(creds.StaticIndex("p", authTestToken),
			func(string) (*config.Config, error) { return cfg, nil }), shared.NewSessions(creds.Store{Dir: t.TempDir()}))
		req := httptest.NewRequest(fiber.MethodPost, "/call/"+authTestToken+"/v1/messages", strings.NewReader(sampleRequest))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != fiber.StatusBadRequest || !strings.Contains(string(b), "Peak hours of provider p") {
			t.Errorf("%s: status %d, body %s", typ, resp.StatusCode, b)
		}
	}
}

// The request line of a refused request says why.
func TestPeakRefusalIsInTheRequestLog(t *testing.T) {
	cfg := peakConfig("http://127.0.0.1:1", "UTC", config.PeakWindow{From: "00:00", To: "24:00"})
	var al syncBuf
	app := fiber.New()
	app.Use(rt.AccessLog(&al))
	registerCallRoutes(app, shared.RequireProfile(creds.StaticIndex("zai", authTestToken),
		func(string) (*config.Config, error) { return cfg, nil }), shared.NewSessions(creds.Store{Dir: t.TempDir()}))
	req := httptest.NewRequest(fiber.MethodPost, "/call/"+authTestToken+"/v1/messages", strings.NewReader(sampleRequest))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	line := al.String()
	for _, want := range []string{" 400 - ", "POST /call/***/v1/messages [zai]: Peak hours of provider zai"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, authTestToken) {
		t.Errorf("the token is in the log:\n%s", line)
	}
}

func TestPeakBannerNamesTheMachinesZone(t *testing.T) {
	cfg := peakConfig("http://x", "", config.PeakWindow{From: "14:00", To: "18:00"})
	got := peakBanner(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "peak hours of zai: 14:00-18:00 (local time)") {
		t.Errorf("banner = %q", got)
	}
	if got := peakBanner(compatConfig("http://x", nil)); len(got) != 0 {
		t.Errorf("a provider without peak hours is in the banner: %q", got)
	}
}

// A window that cannot be read (the loader rejects it, but a Config can be made
// by hand) lets the request through rather than stopping all work.
func TestPeakRefusalIgnoresAnUnreadableSchedule(t *testing.T) {
	cfg := peakConfig("http://x", "", config.PeakWindow{Days: "funday", From: "14:00", To: "18:00"})
	if m := peakRefusal(cfg, peak.NewPauses(t.TempDir()), shanghai(3, 15, 0)); m != "" {
		t.Errorf("refused by a schedule that cannot be read: %s", m)
	}
}

// The refusal follows config.ktav as it changes, without a restart: peak hours
// that appear are enforced on the next request, and ones that go are not.
func TestPeakHoursFollowTheConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ktav")
	t.Setenv(config.ConfigFileEnv, path)
	t.Cleanup(func() {
		proxydial.SetDefault(nil)
		applyConnectionLimits(&config.Config{})
	})
	text := func(peak bool) string {
		p := ""
		if peak {
			p = "        peak_tz: UTC\n        peak: [\n            {\n                from: 00:00\n                to: 24:00\n            }\n        ]\n"
		}
		return "providers: [\n    {\n        name: p\n        type: codex\n" + p + "    }\n]\n\n" +
			"profiles: [\n    {\n        name: a\n        provider: p\n    }\n]\n"
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
	refused := func(src *cfgSource) string {
		t.Helper()
		rc, err := src.get().ForProfile("a", "")
		if err != nil {
			t.Fatal(err)
		}
		return peakRefusal(rc, peak.NewPauses(t.TempDir()), time.Now())
	}

	write(text(false), 0)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	src := newCfgSource(cfg)
	if m := refused(src); m != "" {
		t.Fatalf("refused before any peak hours: %s", m)
	}
	write(text(true), 1)
	if m := refused(src); !strings.Contains(m, "Peak hours of provider p") {
		t.Errorf("peak hours added to the file are not enforced: %q", m)
	}
	write(text(false), 2)
	if m := refused(src); m != "" {
		t.Errorf("peak hours removed from the file are still enforced: %s", m)
	}
}

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
