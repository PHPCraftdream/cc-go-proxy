package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/gofiber/fiber/v2"
)

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
	pauses := config.NewPeakPauses(t.TempDir())
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
	pauses := config.NewPeakPauses(t.TempDir())
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

func TestPeakRefusalNeedsPeakHours(t *testing.T) {
	pauses := config.NewPeakPauses(t.TempDir())
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
	old := upstreamClient
	upstreamClient = &http.Client{Transport: connpool.NewRouter(0, 1, 1, nil)}
	t.Cleanup(func() { upstreamClient = old })

	cfg := peakConfig(up.URL, "UTC", config.PeakWindow{From: "00:00", To: "24:00"})
	ss := &sessions{store: creds.Store{Dir: t.TempDir()}}
	app := fiber.New()
	registerCallRoutes(app, requireProfile(creds.StaticIndex("zai", authTestToken),
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

	if err := ss.pauses().Set("zai", time.Now().Add(time.Hour), time.Now()); err != nil {
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
		registerCallRoutes(app, requireProfile(creds.StaticIndex("p", authTestToken),
			func(string) (*config.Config, error) { return cfg, nil }), &sessions{store: creds.Store{Dir: t.TempDir()}})
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
	app.Use(accessLog(&al))
	registerCallRoutes(app, requireProfile(creds.StaticIndex("zai", authTestToken),
		func(string) (*config.Config, error) { return cfg, nil }), &sessions{store: creds.Store{Dir: t.TempDir()}})
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
	if m := peakRefusal(cfg, config.NewPeakPauses(t.TempDir()), shanghai(3, 15, 0)); m != "" {
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
		return peakRefusal(rc, config.NewPeakPauses(t.TempDir()), time.Now())
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
