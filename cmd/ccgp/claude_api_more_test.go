package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// Profiles, sorted: api, codex, zai. Providers, sorted: api, codex, zai.
const apiUIConfig = `listen.port: 60093

providers: [
    {
        name: codex
        type: codex
    }
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
    }
    {
        name: api
        type: claude_api
    }
]

profiles: [
    {
        name: codex
        provider: codex
    }
    {
        name: zai
        provider: zai
    }
    {
        name: api
        provider: api
    }
]
`

// ---- the interface ----

func TestTUIClaudeAPIProfileScreen(t *testing.T) {
	h := newTUI(t, apiUIConfig)
	h.open("profiles")
	h.has("api", "claude_api", "no API key (ccgp --login api)")
	h.open("profile:api")
	h.has("Profile api", "provider  api (claude_api)", "server    "+config.OfficialAnthropicURL, "the real Claude Code: nothing goes through ccgp",
		"base_url", "log in: save the API key", "remove the profile")
	h.lacks("proxy", "models", "default_model", "context_window", "compact_percent", "move to another provider")

	// The server is set from here, and emptying the field brings the official one back.
	h.open("base_url")
	h.replace("https://gw.example")
	h.press("enter")
	if got := h.config().Providers["api"].BaseURL; got != "https://gw.example" {
		t.Fatalf("base_url = %q\n%s", got, h.text())
	}
	h.has("server    https://gw.example")
	h.open("base_url")
	h.press("ctrl+u", "enter")
	if got := h.config().Providers["api"].BaseURL; got != "" {
		t.Errorf("base_url after emptying = %q\n%s", got, h.text())
	}
	h.has("server    " + config.OfficialAnthropicURL)

	// A server the loader refuses is shown under the field and nothing is written.
	before := h.text()
	h.open("base_url")
	h.replace("gw.example")
	h.press("enter")
	if in := h.input(); !strings.Contains(in.err, "http(s) URL") {
		t.Errorf("no refusal under the field: %q", in.err)
	}
	if h.text() != before {
		t.Errorf("a refused server was written:\n%s", h.text())
	}
}

func TestTUIClaudeAPILoginRunsTheCommand(t *testing.T) {
	h := newTUI(t, apiUIConfig)
	h.open("profiles", "profile:api", "login")
	want := []ran{{args: []string{"--login", "api"}, terminal: true}}
	if !reflect.DeepEqual(h.ran, want) {
		t.Errorf("the interface started %+v, want %+v", h.ran, want)
	}
}

func TestTUIClaudeAPIProviderScreen(t *testing.T) {
	h := newTUI(t, apiUIConfig)
	h.open("providers", "provider:api")
	h.has("type         claude_api", "base_url     "+config.OfficialAnthropicURL+" (the default)", "api_key_env  -",
		"log in", "log out")
	h.lacks("proxy", "peak hours", "log in with a device code")

	// A provider of a backend keeps all its items next to it.
	h = newTUI(t, apiUIConfig)
	h.open("providers", "provider:zai")
	h.has("proxy", "peak hours")
}

// labels are the first words of the items of the menu on top.
func labels(h *tui, id string) []string {
	h.t.Helper()
	m, ok := h.a.top().(*menu)
	if !ok {
		h.t.Fatalf("the top screen is not a menu:\n%s", h.view())
	}
	var out []string
	for _, it := range m.items(h.a) {
		if it.id == id {
			out = append(out, strings.Fields(it.label)[0])
		}
	}
	return out
}

func TestTUIClaudeAPIIsNotABackendToMoveToOrMap(t *testing.T) {
	h := newTUI(t, apiUIConfig)
	h.open("profiles", "profile:zai", "move")
	if got := labels(h, "provider"); !reflect.DeepEqual(got, []string{"codex"}) {
		t.Errorf("a profile can be moved to %v, want only codex", got)
	}

	h = newTUI(t, apiUIConfig)
	h.open("models")
	if got := labels(h, "profile"); !reflect.DeepEqual(got, []string{"codex", "zai"}) {
		t.Errorf("models of profiles %v, want codex and zai: a claude_api profile has no mapping", got)
	}
}

func TestTUIAddClaudeAPIProfile(t *testing.T) {
	h := newTUI(t, apiUIConfig)
	h.open("profiles", "add", "add_type", "kind_claude_api")
	h.has("base URL", "official server")
	h.press("enter") // the official server
	h.has("profile name")
	replaceAndEnter(h, "neo") // no model step
	cfg := h.config()
	if pr := cfg.Providers["neo"]; pr == nil || pr.Type != config.ProviderTypeClaudeAPI || pr.BaseURL != "" {
		t.Fatalf("provider = %+v\n%s", pr, h.text())
	}
	if pf := cfg.Profiles["neo"]; pf == nil || pf.Provider != "neo" || len(pf.Models) != 0 {
		t.Errorf("profile = %+v", pf)
	}
	for _, want := range []string{`created Claude API profile "neo"`, "ccgp --login neo"} {
		if !strings.Contains(h.a.msg, want) {
			t.Errorf("the message lacks %q:\n%s", want, h.a.msg)
		}
	}

	// Another server.
	h = newTUI(t, apiUIConfig)
	h.open("providers", "add", "kind_claude_api")
	h.typ("https://gw.example")
	h.press("enter")
	replaceAndEnter(h, "gw")
	if pr := h.config().Providers["gw"]; pr == nil || pr.BaseURL != "https://gw.example" {
		t.Errorf("provider gw = %+v\n%s", pr, h.text())
	}

	// A server that is not a URL: the name step shows why, the config is as it was.
	h = newTUI(t, apiUIConfig)
	before := h.text()
	h.open("profiles", "add", "add_type", "kind_claude_api")
	h.typ("gw.example")
	h.press("enter")
	replaceAndEnter(h, "bad")
	if in := h.input(); !strings.Contains(in.err, "http(s) URL") {
		t.Errorf("no refusal: %q", in.err)
	}
	if h.text() != before {
		t.Errorf("a refused profile was written:\n%s", h.text())
	}
}

// ---- the key ----

// withStdin replaces os.Stdin with a file holding text, for the key prompt.
func withStdin(t *testing.T, text string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = saved; _ = f.Close() })
}

func TestLaunchClaudeAPIAsksForTheKeyOnce(t *testing.T) {
	ownSetup(t)
	out := fakeClaude(t)
	addAPI(t, "api", "")
	withStdin(t, "  sk-ant-typed \n")

	got := launchedEnv(t, out, 3, launch("api", nil))
	if !strings.Contains(got, "ANTHROPIC_API_KEY=sk-ant-typed") {
		t.Errorf("the typed key did not reach claude:\n%s", got)
	}
	store, err := credsStore()
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := store.APIKey("api"); k != "sk-ant-typed" {
		t.Errorf("saved key = %q", k)
	}

	// The next start does not ask again.
	withStdin(t, "")
	_ = os.Remove(out)
	if got = launchedEnv(t, out, 3, launch("api", nil)); !strings.Contains(got, "ANTHROPIC_API_KEY=sk-ant-typed") {
		t.Errorf("the saved key was not used:\n%s", got)
	}
}

func TestLaunchClaudeAPIWithoutAKeyDoesNotStart(t *testing.T) {
	ownSetup(t)
	out := fakeClaude(t)
	addAPI(t, "api", "")
	withStdin(t, "") // nothing typed
	if err := launch("api", nil); err == nil {
		t.Fatal("launch without a key succeeded")
	}
	withStdin(t, "\n") // an empty line
	if err := launch("api", nil); err == nil || !strings.Contains(err.Error(), "empty API key") {
		t.Errorf("an empty key: err = %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("claude was started without a key")
	}
}

// A local server does not excuse a claude_api profile from the key, as it does a codex_api one.
func TestClaudeAPIOnALocalServerStillNeedsAKey(t *testing.T) {
	path, _ := writeTestConfig(t, testConfig)
	cfg := addAPI(t, "loc", "http://localhost:8080")
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Type: config.ProviderTypeCodexAPI, Name: "ollama", BaseURL: "http://localhost:11434/v1"},
		path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	cfg = loadTestConfig(t)
	store := creds.Store{Dir: t.TempDir()}
	if got := loginState(store, cfg.Providers["loc"]); got != "no API key (ccgp --login loc)" {
		t.Errorf("claude_api on localhost: %q", got)
	}
	if got := loginState(store, cfg.Providers["ollama"]); got != "no key needed (local)" {
		t.Errorf("codex_api on localhost: %q", got)
	}
	isolate(t)
	t.Setenv(config.ConfigFileEnv, path)
	withStdin(t, "")
	if err := login(cfg, "loc", false, false); err == nil {
		t.Error("login of a claude_api provider on localhost did not ask for a key")
	}
	if err := login(cfg, "ollama", false, false); err != nil {
		t.Errorf("login of a codex_api provider on localhost: %v", err)
	}
}

func TestLogoutOfAClaudeAPIKey(t *testing.T) {
	store := creds.Store{Dir: t.TempDir()}
	cfg := &config.Config{
		ConfigFile: filepath.Join(t.TempDir(), "config.ktav"),
		Providers: map[string]*config.Provider{
			"api": {Name: "api", Type: config.ProviderTypeClaudeAPI},
			"gw":  {Name: "gw", Type: config.ProviderTypeClaudeAPI},
		},
	}
	for _, p := range []string{"api", "gw"} {
		if err := store.SaveAPIKey(p, "sk-ant-x"); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runLogout(t, store, cfg, "api", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if store.HasProviderCreds("api") || !store.HasProviderCreds("gw") {
		t.Error("logout removed the wrong key")
	}
	if !strings.Contains(out, "logged out of api") || !strings.Contains(out, "ccgp --login api") {
		t.Errorf("output = %q", out)
	}
}

// ---- doctor and status ----

func TestDoctorLeavesClaudeProfilesToClaudeCode(t *testing.T) {
	env := fakeDoctorEnv()
	probes := 0
	env.probe = func(string, proxydial.Choice) (int, error) { probes++; return 200, nil }
	cfg := &config.Config{Providers: map[string]*config.Provider{
		"a": {Name: "a", Type: config.ProviderTypeClaudeAPI, BaseURL: "https://gw.example"},
		"b": {Name: "b", Type: config.ProviderTypeClaudeAPI},
		"c": {Name: "c", Type: config.ProviderTypeClaude},
	}}
	if checks := checkBackends(env, cfg); len(checks) != 0 || probes != 0 {
		t.Errorf("doctor probed the servers of Claude profiles (%d probes): %+v", probes, checks)
	}

	// A claude_api profile without its key is a warning, like any provider without one.
	env.loginStateFn = loginState
	env.store = creds.Store{Dir: t.TempDir()}
	checks := checkLogins(env, &config.Config{Providers: map[string]*config.Provider{"a": cfg.Providers["a"]}})
	if len(checks) != 1 || checks[0].Status != "warn" || !strings.Contains(checks[0].Detail, "no API key (ccgp --login a)") {
		t.Errorf("login check = %+v", checks)
	}
	if err := env.store.SaveAPIKey("a", "sk-ant-x"); err != nil {
		t.Fatal(err)
	}
	if checks = checkLogins(env, &config.Config{Providers: map[string]*config.Provider{"a": cfg.Providers["a"]}}); checks[0].Status != "ok" {
		t.Errorf("login check with a key = %+v", checks)
	}
}

func TestStatusShowsClaudeAPIProfile(t *testing.T) {
	ownSetup(t)
	addAPI(t, "api", "")
	var runErr error
	out := captureStdout(t, func() { runErr = status([]string{"--json"}) })
	_ = runErr // exit 3 when no server runs
	for _, want := range []string{`"type": "claude_api"`, "no API key (ccgp --login api)", `"name": "api"`} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestProfileListShowsClaudeAPIWithItsKeyState(t *testing.T) {
	writeTestConfig(t, testConfig)
	cfg := addAPI(t, "api", "")
	store := creds.Store{Dir: t.TempDir()}
	var buf bytes.Buffer
	if err := profileList(&buf, buildProfileRows(cfg, store, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if line := lineWith(buf.String(), "api "); !strings.Contains(line, "claude_api") || !strings.Contains(line, "no API key") {
		t.Errorf("list line = %q\n%s", line, buf.String())
	}
	if err := store.SaveAPIKey("api", "sk-ant-x"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := profileList(&buf, buildProfileRows(cfg, store, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if line := lineWith(buf.String(), "api "); !strings.Contains(line, "API key saved") || strings.Contains(line, "sk-ant-x") {
		t.Errorf("list line with a key = %q", line)
	}
	if strings.Contains(buf.String(), "sk-ant-x") {
		t.Error("the key is in the listing")
	}
}

func lineWith(text, prefix string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}
