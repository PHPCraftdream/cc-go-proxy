package core

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/tune"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

func TestProfileRemovePurge(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "zai2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := creds.Store{Dir: t.TempDir()}
	if _, err := store.EnsureProfileToken("zai2"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAPIKey("zai", "sk-test"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ProfileRemove(&out, ProfileRmOpts{Name: "zai2", Yes: true, Purge: true}, path, cfg, store, base, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "zai2")); !os.IsNotExist(err) {
		t.Errorf("the profile's Claude dir survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "keep")); err != nil {
		t.Errorf("a sibling directory was touched: %v", err)
	}
	// Provider zai stays (zai still uses it), so its credentials stay too.
	if _, err := os.Stat(filepath.Join(store.Dir, "apikey-zai.ktav")); err != nil {
		t.Errorf("credentials of the kept provider were deleted: %v", err)
	}

	// --purge on the only profile of a provider deletes the credentials too.
	path, cfg = writeTestConfig(t, testConfig)
	store = creds.Store{Dir: t.TempDir()}
	if err := store.SaveAPIKey("zai", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if err := ProfileRemove(&bytes.Buffer{}, ProfileRmOpts{Name: "zai2", Yes: true, Purge: true}, path, cfg, store, t.TempDir(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	// zai is still used by profile zai, so nothing was deleted yet; remove zai itself.
	if err := ProfileRemove(&bytes.Buffer{}, ProfileRmOpts{Name: "zai", Yes: true, Purge: true}, path, loadTestConfig(t), store, t.TempDir(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "apikey-zai.ktav")); !os.IsNotExist(err) {
		t.Errorf("credentials of the removed provider survived: %v", err)
	}
}

func TestProfileRemoveConfirmation(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	var out bytes.Buffer
	if err := ProfileRemove(&out, ProfileRmOpts{Name: "zai2"}, path, cfg, creds.Store{Dir: t.TempDir()}, t.TempDir(), strings.NewReader("n\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `remove profile "zai2"? [y/N]`) || !strings.Contains(out.String(), "aborted") {
		t.Errorf("output:\n%s", out.String())
	}
	if loadTestConfig(t).Profiles["zai2"] == nil {
		t.Error("a declined prompt removed the profile")
	}

	out.Reset()
	if err := ProfileRemove(&out, ProfileRmOpts{Name: "zai2"}, path, cfg, creds.Store{Dir: t.TempDir()}, t.TempDir(), strings.NewReader("y\n")); err != nil {
		t.Fatal(err)
	}
	if loadTestConfig(t).Profiles["zai2"] != nil {
		t.Error("an accepted prompt did not remove the profile")
	}

	// --yes skips the prompt entirely.
	path, cfg = writeTestConfig(t, testConfig)
	out.Reset()
	if err := ProfileRemove(&out, ProfileRmOpts{Name: "zai2", Yes: true}, path, cfg, creds.Store{Dir: t.TempDir()}, t.TempDir(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("--yes still prompted:\n%s", out.String())
	}
}

func TestClaudeProfileDirSafety(t *testing.T) {
	base := t.TempDir()
	p, err := common.ClaudeProfileDir(base, "codex")
	if err != nil || p != filepath.Join(base, "codex") {
		t.Fatalf("claudeProfileDir = %q, %v", p, err)
	}
	for _, bad := range []string{"", "..", "Foo", "a b", "claude", filepath.Join(base, "x")} {
		if _, err := common.ClaudeProfileDir(base, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestProfileCmdUsage(t *testing.T) {
	path, _ := writeTestConfig(t, testConfig)
	for _, args := range [][]string{nil, {"bogus"}, {"add", "x"}, {"rm"}} {
		if err := ProfileCmd(loadTestConfig(t), args); err == nil {
			t.Errorf("profileCmd(%v) succeeded", args)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config vanished: %v", err)
	}
}

func TestProfileAddKeepsCRLF(t *testing.T) {
	crlf := strings.ReplaceAll(testConfig, "\n", "\r\n")
	path, cfg := writeTestConfig(t, crlf)
	if err := ProfileAdd(&bytes.Buffer{}, ProfileAddOpts{Like: "zai"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := loadTestConfig(t)
	if cfg2.Profiles["zai3"] == nil {
		t.Fatal("zai3 missing")
	}
	lines := strings.Split(string(b), "\n")
	crlfCount := 0
	for _, l := range lines[:len(lines)-1] {
		if strings.HasSuffix(l, "\r") {
			crlfCount++
		}
	}
	// Every line, the inserted ones included, must keep the file's CRLF.
	if crlfCount != len(lines)-1 {
		t.Errorf("CRLF endings were not preserved: %d of %d lines", crlfCount, len(lines)-1)
	}
}

// The new provider of --like has the peak hours of the source: they belong to
// the service, not to the login.
func TestProfileAddLikeCarriesPeakHours(t *testing.T) {
	text := strings.Replace(testConfig, "        api_key_env: ZAI_KEY_TEST\n",
		"        api_key_env: ZAI_KEY_TEST\n        peak_tz: Asia/Shanghai\n        peak: [\n            {\n                days: mon-fri\n                from: 14:00\n                to: 18:00\n            }\n        ]\n", 1)
	path, cfg := writeTestConfig(t, text)
	if err := ProfileAdd(&bytes.Buffer{}, ProfileAddOpts{Like: "zai"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	pr := loadTestConfig(t).Providers["zai3"]
	want := []config.PeakWindow{{Days: "mon-fri", From: "14:00", To: "18:00"}}
	if pr == nil || !reflect.DeepEqual(pr.Peak, want) || pr.PeakTZ != "Asia/Shanghai" {
		t.Errorf("provider zai3 = %+v, want the peak hours of zai", pr)
	}
}

// A global proxy, a codex provider p with profiles a and b, and a Claude one.
func providerProxyTestConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	body := "connections.proxy: socks5://me:s3cret@127.0.0.1:1080\n\n" +
		"providers: [\n    {\n        name: p\n        type: codex\n    }\n    {\n        name: w\n        type: claude\n    }\n]\n\n" +
		"profiles: [\n    {\n        name: a\n        provider: p\n    }\n    {\n        name: b\n        provider: p\n    }\n    {\n        name: w\n        provider: w\n    }\n]\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, p)
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	return p
}

func TestConnectionsProviderProxy(t *testing.T) {
	p := providerProxyTestConfig(t)
	var buf bytes.Buffer
	// A proxy for the provider: every profile of it goes through it.
	if err := tune.Connections(&buf, []string{"proxy", "http://corp:3128", "provider", "p"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "name: p\n        type: codex\n        proxy: http://corp:3128\n") {
		t.Fatalf("config:\n%s", data)
	}
	if !strings.Contains(string(data), "connections.proxy: socks5://me:s3cret@127.0.0.1:1080\n") {
		t.Errorf("the global proxy changed:\n%s", data)
	}
	if out := buf.String(); !strings.Contains(out, "provider p: proxy http://corp:3128") || strings.Contains(out, "profile a") {
		t.Errorf("output:\n%s", out)
	}

	// A profile's own proxy beats the provider's; the output lists both.
	buf.Reset()
	if err := tune.Connections(&buf, []string{"proxy", "none", "profile", "a"}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "provider p: proxy http://corp:3128") || !strings.Contains(out, "profile a: proxy none") || strings.Contains(out, "profile b") {
		t.Errorf("output:\n%s", out)
	}

	// JSON: what is in force, whether the object sets it, and the level it comes from.
	buf.Reset()
	if err := tune.Connections(&buf, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Providers map[string]struct {
			Proxy string
			Own   bool
		}
		Profiles map[string]struct {
			Proxy string
			Own   bool
			Level string
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || strings.Contains(buf.String(), "s3cret") {
		t.Fatalf("json %s: %v", buf.String(), err)
	}
	if pr := got.Providers["p"]; pr.Proxy != "http://corp:3128" || !pr.Own {
		t.Errorf("provider p = %+v", pr)
	}
	if w := got.Providers["w"]; w.Own {
		t.Errorf("provider w = %+v", w)
	}
	if a := got.Profiles["a"]; a.Proxy != "none" || !a.Own || a.Level != "profile" {
		t.Errorf("profile a = %+v", a)
	}
	if b := got.Profiles["b"]; b.Proxy != "http://corp:3128" || b.Own || b.Level != "provider" {
		t.Errorf("profile b = %+v", b)
	}

	// none for the provider: direct, the global proxy is not inherited.
	buf.Reset()
	if err := tune.Connections(&buf, []string{"proxy", "none", "provider", "p"}); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(p); !strings.Contains(string(data), "name: p\n        type: codex\n        proxy: none\n") || strings.Contains(string(data), "corp:3128") {
		t.Errorf("config after none:\n%s", data)
	}
	if !strings.Contains(buf.String(), "provider p: proxy none") {
		t.Errorf("output:\n%s", buf.String())
	}

	// inherit removes the override: the global proxy applies again.
	buf.Reset()
	if err := tune.Connections(&buf, []string{"proxy", "inherit", "provider", "p"}); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(p); strings.Contains(string(data), "name: p\n        type: codex\n        proxy:") {
		t.Errorf("config after inherit:\n%s", data)
	}
	if strings.Contains(buf.String(), "provider p") {
		t.Errorf("output after inherit:\n%s", buf.String())
	}
	buf.Reset()
	if err := tune.Connections(&buf, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || got.Providers["p"].Own || got.Profiles["b"].Proxy != "socks5://me:***@127.0.0.1:1080" || got.Profiles["b"].Level != "global" {
		t.Errorf("after inherit: provider p %+v, profile b %+v (%v)", got.Providers["p"], got.Profiles["b"], err)
	}
}

func TestConnectionsProviderProxyErrors(t *testing.T) {
	p := providerProxyTestConfig(t)
	before, _ := os.ReadFile(p)
	for _, args := range [][]string{
		{"proxy", "none", "provider", "nope"},
		{"proxy", "ftp://x:21", "provider", "p"},
		{"proxy", "none", "provider", "w"}, // the real Claude Code does not go through ccgp
		{"proxy", "none", "provider"},
	} {
		if err := tune.Connections(&bytes.Buffer{}, args); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Errorf("config changed by refused edits:\n%s", after)
	}
}

// The new provider of --like has the proxy of the source: it belongs to the
// service, not to the login, and without it the copy would go another way.
func TestProfileAddLikeCarriesProviderProxy(t *testing.T) {
	text := strings.Replace(testConfig, "        api_key_env: ZAI_KEY_TEST\n",
		"        api_key_env: ZAI_KEY_TEST\n        proxy: http://corp:3128\n", 1)
	path, cfg := writeTestConfig(t, text)
	if err := ProfileAdd(&bytes.Buffer{}, ProfileAddOpts{Like: "zai"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	pr := loadTestConfig(t).Providers["zai3"]
	if pr == nil || pr.Proxy != "http://corp:3128" {
		t.Errorf("provider zai3 = %+v, want the proxy of zai", pr)
	}
}

func whichTestConfig() *config.Config {
	return &config.Config{
		ConfigFile: "config.ktav", // file mode
		Providers: map[string]*config.Provider{
			"codex": {Name: "codex", Type: config.ProviderTypeCodex},
		},
		Profiles: map[string]*config.Profile{
			"codex": {
				Name:     "codex",
				Provider: "codex",
				Models: []config.ModelRule{
					{Match: "*haiku*", Model: "mini", ContextWindow: 272000},
					{Match: "claude-opus-*", Model: "big", ContextWindow: 0},
				},
			},
			"empty": {Name: "empty", Provider: "codex"},
		},
	}
}

func TestWhichRouteTiers(t *testing.T) {
	cfg := whichTestConfig()
	for _, tc := range []struct{ model, backend, matchedBy string }{
		{"claude-3-5-haiku-20241022", "mini", "rule *haiku*"},
		{"claude-opus-4-1", "big", "rule claude-opus-*"},
		{"claude-sonnet-4", "gpt-5", "tier setting"},
		{"gpt-4o", "gpt-4o", "passthrough"},
	} {
		ri, err := WhichRoute(cfg, "codex", tc.model)
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		if ri.BackendModel != tc.backend {
			t.Errorf("%s: backend = %q, want %q", tc.model, ri.BackendModel, tc.backend)
		}
		if ri.MatchedBy != tc.matchedBy {
			t.Errorf("%s: matchedBy = %q, want %q", tc.model, ri.MatchedBy, tc.matchedBy)
		}
	}
}

func TestWhichRouteDefaultModel(t *testing.T) {
	cfg := whichTestConfig()
	cfg.Profiles["codex"].DefaultModel = "fallback-model"
	ri, err := WhichRoute(cfg, "codex", "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if ri.BackendModel != "fallback-model" || ri.MatchedBy != "default_model" {
		t.Errorf("got %q / %q, want fallback-model / default_model", ri.BackendModel, ri.MatchedBy)
	}
}

func TestWhichRouteContextWindow(t *testing.T) {
	cfg := whichTestConfig()
	if ri, _ := WhichRoute(cfg, "codex", "claude-3-5-haiku-20241022"); ri.ContextWindow != "272000" {
		t.Errorf("haiku window = %q, want 272000", ri.ContextWindow)
	}
	// A rule without a window falls back to the smallest window of the profile.
	if ri, _ := WhichRoute(cfg, "codex", "claude-opus-4-1"); ri.ContextWindow != "272000" {
		t.Errorf("opus window = %q, want the profile fallback 272000", ri.ContextWindow)
	}
	// No window anywhere in the profile: unknown.
	if ri, err := WhichRoute(cfg, "empty", "gpt-4o"); err != nil || ri.ContextWindow != "unknown" {
		t.Errorf("empty profile window = %q (%v), want unknown", ri.ContextWindow, err)
	}
}

func TestWhichRouteUnknownProfile(t *testing.T) {
	cfg := whichTestConfig()
	_, err := WhichRoute(cfg, "nope", "claude-sonnet-4")
	if err == nil {
		t.Fatal("expected error")
	}
	want := `unknown profile "nope" (configured: codex, empty)`
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

func TestWhichRouteJSONShape(t *testing.T) {
	cfg := whichTestConfig()
	ri, err := WhichRoute(cfg, "codex", "claude-3-5-haiku-20241022")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := common.WriteJSON(&buf, ri); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"profile":       "codex",
		"provider":      "codex",
		"providerType":  "codex",
		"requested":     "claude-3-5-haiku-20241022",
		"backendModel":  "mini",
		"contextWindow": "272000",
		"matchedBy":     "rule *haiku*",
	}
	for k, v := range want {
		if doc[k] != v {
			t.Errorf("%s = %v, want %q", k, doc[k], v)
		}
	}
	for k := range doc {
		if !strings.Contains(strings.Join([]string{"profile", "provider", "providerType", "requested", "backendModel", "contextWindow", "matchedBy"}, " "), k) {
			t.Errorf("unexpected JSON field %q", k)
		}
	}
}

func TestWhichRouteTextOutput(t *testing.T) {
	cfg := whichTestConfig()
	ri, err := WhichRoute(cfg, "empty", "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := renderRouteText(&buf, ri); err != nil {
		t.Fatal(err)
	}
	// Values start in one column, after the longest label ("context window:").
	row := func(label, value string) string {
		return label + strings.Repeat(" ", 16-len(label)) + value + "\n"
	}
	want := row("profile:", "empty") +
		row("provider:", "codex (codex)") +
		row("requested:", "gpt-4o") +
		row("backend model:", "gpt-4o") +
		row("context window:", "unknown") +
		row("matched by:", "passthrough")
	if buf.String() != want {
		t.Errorf("output:\n%q\nwant:\n%q", buf.String(), want)
	}
}

func TestParseWhichArgs(t *testing.T) {
	p, m, j, err := parseWhichArgs([]string{"codex", "--json", "claude-opus-4-1"})
	if err != nil || p != "codex" || m != "claude-opus-4-1" || !j {
		t.Errorf("got %q %q %v %v", p, m, j, err)
	}
	p, m, j, err = parseWhichArgs([]string{"codex", "x"})
	if err != nil || p != "codex" || m != "x" || j {
		t.Errorf("got %q %q %v %v", p, m, j, err)
	}
}

// Env-mode tier overrides apply in env mode only; file mode ignores them.
func TestWhichRouteEnvModeTierOverrides(t *testing.T) {
	env := &config.Config{
		ProviderName: "env", ProviderType: config.ProviderTypeCodexAPI,
		OpusModel: "my-opus", HaikuModel: "my-haiku", SonnetModel: "my-sonnet",
		Profiles: map[string]*config.Profile{"default": {Name: "default"}},
	}
	for model, want := range map[string]string{
		"claude-opus-4-1": "my-opus", "claude-haiku-4": "my-haiku", "claude-sonnet-4": "my-sonnet",
	} {
		ri, err := WhichRoute(env, "default", model)
		if err != nil || ri.BackendModel != want {
			t.Errorf("%s: backend %q (%v), want %q", model, ri.BackendModel, err, want)
		}
	}
	// File mode (the tier fallback above is the converter default, not an override).
	ri, err := WhichRoute(whichTestConfig(), "empty", "claude-opus-4-1")
	if err != nil || ri.BackendModel != "gpt-5" {
		t.Errorf("file mode opus: %q (%v), want gpt-5", ri.BackendModel, err)
	}
}

func TestWhichCmdUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"onlyone"},
		{"a", "b", "c"},
		{"--json"},
	} {
		if err := WhichCmd(args); err == nil || !strings.Contains(err.Error(), "usage: ccgp --which <profile> <model> [--json]") {
			t.Errorf("whichCmd(%v) = %v, want usage error", args, err)
		}
	}
}
