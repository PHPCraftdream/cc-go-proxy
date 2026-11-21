package tui

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/cli"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/core"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
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

	got := launchedEnv(t, out, 3, core.Launch("api", nil))
	if !strings.Contains(got, "ANTHROPIC_API_KEY=sk-ant-typed") {
		t.Errorf("the typed key did not reach claude:\n%s", got)
	}
	store, err := common.CredsStore()
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := store.APIKey("api"); k != "sk-ant-typed" {
		t.Errorf("saved key = %q", k)
	}

	// The next start does not ask again.
	withStdin(t, "")
	_ = os.Remove(out)
	if got = launchedEnv(t, out, 3, core.Launch("api", nil)); !strings.Contains(got, "ANTHROPIC_API_KEY=sk-ant-typed") {
		t.Errorf("the saved key was not used:\n%s", got)
	}
}

func TestLaunchClaudeAPIWithoutAKeyDoesNotStart(t *testing.T) {
	ownSetup(t)
	out := fakeClaude(t)
	addAPI(t, "api", "")
	withStdin(t, "") // nothing typed
	if err := core.Launch("api", nil); err == nil {
		t.Fatal("launch without a key succeeded")
	}
	withStdin(t, "\n") // an empty line
	if err := core.Launch("api", nil); err == nil || !strings.Contains(err.Error(), "empty API key") {
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
	if err := core.ProfileAdd(&bytes.Buffer{}, core.ProfileAddOpts{Type: config.ProviderTypeCodexAPI, Name: "ollama", BaseURL: "http://localhost:11434/v1"},
		path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	cfg = loadTestConfig(t)
	store := creds.Store{Dir: t.TempDir()}
	if got := common.LoginState(store, cfg.Providers["loc"]); got != "no API key (ccgp --login loc)" {
		t.Errorf("claude_api on localhost: %q", got)
	}
	if got := common.LoginState(store, cfg.Providers["ollama"]); got != "no key needed (local)" {
		t.Errorf("codex_api on localhost: %q", got)
	}
	isolate(t)
	t.Setenv(config.ConfigFileEnv, path)
	withStdin(t, "")
	if err := core.Login(cfg, "loc", false, false); err == nil {
		t.Error("login of a claude_api provider on localhost did not ask for a key")
	}
	if err := core.Login(cfg, "ollama", false, false); err != nil {
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

func TestStatusShowsClaudeAPIProfile(t *testing.T) {
	ownSetup(t)
	addAPI(t, "api", "")
	var runErr error
	out := captureStdout(t, func() { runErr = cli.Status([]string{"--json"}) })
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
	if err := core.ProfileList(&buf, core.BuildProfileRows(cfg, store, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if line := lineWith(buf.String(), "api "); !strings.Contains(line, "claude_api") || !strings.Contains(line, "no API key") {
		t.Errorf("list line = %q\n%s", line, buf.String())
	}
	if err := store.SaveAPIKey("api", "sk-ant-x"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := core.ProfileList(&buf, core.BuildProfileRows(cfg, store, t.TempDir())); err != nil {
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

// addAPI adds a claude_api profile to the config in use (CCGP_CONFIG) and
// returns the reloaded config.
func addAPI(t *testing.T, name, baseURL string) *config.Config {
	t.Helper()
	if err := core.ProfileAdd(&bytes.Buffer{}, core.ProfileAddOpts{Type: config.ProviderTypeClaudeAPI, Name: name, BaseURL: baseURL},
		os.Getenv(config.ConfigFileEnv), loadTestConfig(t), creds.Store{}); err != nil {
		t.Fatal(err)
	}
	return loadTestConfig(t)
}

// captureStdout returns what fn prints to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// fakeClaude puts a `claude` that records its arguments and environment into
// the file named by CCGP_FAKE_OUT and exits with status 3 first on PATH. The
// rest of PATH stays, the Windows script needs cmd.exe.
func fakeClaude(t *testing.T) (out string) {
	t.Helper()
	dir := t.TempDir()
	out = filepath.Join(t.TempDir(), "claude.out")
	if runtime.GOOS == "windows" {
		script := "@echo off\r\n(echo ARGS=%*& set) > \"%CCGP_FAKE_OUT%\"\r\nexit /b 3\r\n"
		if err := os.WriteFile(filepath.Join(dir, "claude.cmd"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		script := "#!/bin/sh\n{ echo \"ARGS=$*\"; env; } > \"$CCGP_FAKE_OUT\"\nexit 3\n"
		if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CCGP_FAKE_OUT", out)
	return out
}

// isolate points every default location at empty temp dirs, so a test sees
// whether anything was written to the default layout, and clears CCGP_HOME and
// CCGP_CONFIG (restored afterwards, whatever run() sets).
func isolate(t *testing.T) (defaults string) {
	t.Helper()
	defaults = t.TempDir()
	for _, k := range []string{"APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "HOME"} {
		t.Setenv(k, defaults)
	}
	t.Setenv(home.EnvVar, "")
	t.Setenv("CCGP_CONFIG", "")
	t.Setenv("PROXY_TOKEN", "")
	return defaults
}

func launchedEnv(t *testing.T, out string, wantStatus int, err error) string {
	t.Helper()
	var ee common.ExitError
	if !errors.As(err, &ee) || int(ee) != wantStatus {
		t.Fatalf("launch err = %v, want the exit status %d of claude", err, wantStatus)
	}
	b, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("claude was not started: %v", rerr)
	}
	return string(b)
}

// ownSetup isolates every location, gives the user a Claude Code dir full of
// account data and makes a Claude profile "work". It returns the profile dir.
func ownSetup(t *testing.T) (userDir, profileDir string) {
	t.Helper()
	defaults := isolate(t)
	t.Setenv("USERPROFILE", defaults)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	userDir = filepath.Join(defaults, ".claude")
	files := map[string]string{
		filepath.Join(userDir, "CLAUDE.md"):                 "rules v1",
		filepath.Join(userDir, ".credentials.json"):         `{"claudeAiOauth":{"accessToken":"official"}}`,
		filepath.Join(userDir, "history.jsonl"):             "secret history",
		filepath.Join(userDir, "projects", "p", "s1.jsonl"): "{}\n",
		filepath.Join(userDir, "skills", "s1", "SKILL.md"):  "skill",
		filepath.Join(defaults, ".claude.json"):             `{"userID":"user-id-1","oauthAccount":{"emailAddress":"me@example.com"},"mcpServers":{"a":{}}}`,
	}
	for p, body := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	addClaude(t, "work")
	base, err := common.ClaudeHomeBase()
	if err != nil {
		t.Fatal(err)
	}
	return userDir, filepath.Join(base, "work")
}

// addClaude creates profile name of type claude in a fresh test config.
func addClaude(t *testing.T, name string) (string, *config.Config) {
	t.Helper()
	path, cfg := writeTestConfig(t, testConfig)
	if err := core.ProfileAdd(&bytes.Buffer{}, core.ProfileAddOpts{Type: config.ProviderTypeClaude, Name: name}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	return path, loadTestConfig(t)
}

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runLogout(t *testing.T, store creds.Store, cfg *config.Config, provider, input string, yes bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := core.Logout(&out, strings.NewReader(input), store, cfg, provider, yes)
	return out.String(), err
}

// writeTestConfig writes a fixture and returns (path, loaded config).
func writeTestConfig(t *testing.T, text string) (string, *config.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCGP_CONFIG", path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("fixture does not load: %v", err)
	}
	return path, cfg
}

// testConfig covers one provider per interesting shape: codex (managed
// mapping), zai (preset, effort_map, api_key_env) and a rule-less profile.
const testConfig = `listen.host: 127.0.0.1
listen.port: 60093

providers: [
    {
        name: codex
        type: codex
    }
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
        api_key_env: ZAI_KEY_TEST
        effort_map: {
            medium: high
            max: max
        }
    }
]

profiles: [
    {
        name: codex
        provider: codex
        ## ccgp:managed-models begin codex
        models: [
            {
                match: *sonnet*
                model: gpt-6-luna
                context_window: 872000
            }
            {
                match: *
                model: gpt-6-luna
                context_window: 872000
            }
        ]
        ## ccgp:managed-models end codex
    }
    {
        name: zai
        provider: zai
        models: [
            {
                match: *
                model: glm-5.3-flash
                context_window: 1000000
            }
        ]
    }
    {
        name: zai2
        provider: zai
    }
]
`

func replaceAndEnter(h *tui, text string) {
	h.replace(text)
	h.press("enter")
}
