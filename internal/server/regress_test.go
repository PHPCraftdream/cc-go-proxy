package server

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/gofiber/fiber/v2"
)

// Regression tests: each one pins something that went wrong in real use.

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
			logStreamFailure(w, start, "codex", "a -> b", "boom", `{"type":"error"}`, "x-request-id=r1 | conn new")
		}), `^` + hms + `stream failed after \S+ \[codex\] a -> b: boom \| upstream event: \{"type":"error"\} \| x-request-id=r1 \| conn new\n$`},
		{"failure without details", line(func(w io.Writer) { logStreamFailure(w, start, "codex", "a -> b", "boom", "", "") }),
			`^` + hms + `stream failed after \S+ \[codex\] a -> b: boom\n$`},
		{"slow", line(func(w io.Writer) { logSlowStream(w, start, "codex", "a -> b", "conn reused | total 3m") }),
			`^` + hms + `slow stream \S+ \[codex\] a -> b: conn reused \| total 3m\n$`},
		{"canceled", line(func(w io.Writer) { logStreamCanceled(w, start, "codex", "a -> b", "conn new") }),
			`^` + hms + `stream canceled by client after \S+ \[codex\] a -> b: conn new\n$`},
		{"overload retry", line(func(w io.Writer) {
			logUpstreamRetry(w, "codex", "a -> b", 1, 2*time.Second, "overloaded", `{"x":1}`, "cf-ray=1")
		}), `^` + hms + `upstream retry 1/2 in 2s \[codex\] a -> b: overloaded \| upstream event: \{"x":1\} \| cf-ray=1\n$`},
		{"rate limit", line(func(w io.Writer) { logRateRetry(w, "codex", "a -> b", 2*time.Second, "retry-after=2") }),
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
