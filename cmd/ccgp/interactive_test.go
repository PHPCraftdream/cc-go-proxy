package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

// Profiles, sorted: codex, work, zai. Providers, sorted: codex, work, zai.
const uiConfig = `listen.port: 60093

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
        name: work
        type: claude
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
        models: [
            {
                match: *
                model: glm-5.3-flash
                context_window: 1000000
            }
        ]
    }
    {
        name: work
        provider: work
    }
]
`

type uiResult struct {
	out, cfgPath, claudeBase string
	store                    creds.Store
}

func (r uiResult) text(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(r.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (r uiResult) config(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("the config does not load after the session: %v\n%s", err, r.text(t))
	}
	return cfg
}

// runUI plays a session: script is what the user types, one answer per line.
func runUI(t *testing.T, cfgText, script string) uiResult {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CCGP_HOME", filepath.Join(root, "home"))
	path := filepath.Join(root, "config.ktav")
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	r := uiResult{cfgPath: path, claudeBase: filepath.Join(root, "claude"), store: creds.Store{Dir: filepath.Join(root, "creds")}}
	var out bytes.Buffer
	u := &ui{
		in: bufio.NewReader(strings.NewReader(script)), out: &out, now: func() time.Time { return peakNow },
		store: r.store, claudeBase: r.claudeBase, pauses: config.NewPeakPauses(r.store.Dir),
	}
	if err := u.run(); err != nil {
		t.Fatalf("session: %v\n%s", err, out.String())
	}
	r.out = out.String()
	return r
}

var spaces = regexp.MustCompile(` +`)

// has checks that the output holds each text, ignoring runs of spaces.
func has(t *testing.T, out string, wants ...string) {
	t.Helper()
	norm := spaces.ReplaceAllString(out, " ")
	for _, w := range wants {
		if !strings.Contains(norm, spaces.ReplaceAllString(w, " ")) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func lacks(t *testing.T, out string, unwanted ...string) {
	t.Helper()
	norm := spaces.ReplaceAllString(out, " ")
	for _, w := range unwanted {
		if strings.Contains(norm, spaces.ReplaceAllString(w, " ")) {
			t.Errorf("output has %q:\n%s", w, out)
		}
	}
}

func TestInteractiveQuits(t *testing.T) {
	for name, script := range map[string]string{"zero": "0\n", "empty line": "\n", "end of input": "", "no final newline": "0"} {
		r := runUI(t, uiConfig, script)
		has(t, r.out, "ccgp: "+r.cfgPath, "1) profiles", "2) providers", "0) quit")
		if r.text(t) != uiConfig {
			t.Errorf("%s: the config changed", name)
		}
	}
	r := runUI(t, uiConfig, "x\n9\n0\n")
	has(t, r.out, `not a choice: "x"`, `not a choice: "9"`)
	if strings.Count(r.out, "1) profiles") != 1 {
		t.Errorf("the menu was printed again after a wrong word:\n%s", r.out)
	}
}

func TestInteractiveListsAndShowsAProfile(t *testing.T) {
	r := runUI(t, uiConfig, "1\n3\n0\n0\n0\n")
	has(t, r.out,
		"1) codex codex codex", "2) work work claude", "3) zai zai anthropic_compat", "a) add a profile",
		"profile zai", "provider: zai (anthropic_compat)", "default_model: -", "context_window: -",
		"proxy: global (environment)", "models: 1 rule(s)", "* -> glm-5.3-flash (1000000)")
}

func TestInteractiveShowsAClaudeProfileWithoutSettings(t *testing.T) {
	r := runUI(t, uiConfig, "1\n2\n0\n0\n0\n")
	has(t, r.out, "profile work", "provider: work (claude)", "the real Claude Code: nothing goes through ccgp", "r) remove the profile")
	lacks(t, r.out, "set default_model", "move to another provider")
}

func TestInteractiveEditsProfileSettings(t *testing.T) {
	r := runUI(t, uiConfig, "1\n3\n1\nglm-5.3\n2\n500000\n3\n70\n0\n0\n0\n")
	pf := r.config(t).Profiles["zai"]
	if pf.DefaultModel != "glm-5.3" || pf.ContextWindow != 500000 || pf.CompactPercent != 70 || len(pf.Models) != 1 {
		t.Errorf("profile after the session: %+v", pf)
	}
	has(t, r.out, "new default_model (now -; Enter keeps, - removes):", "default_model: glm-5.3", "context_window: 500000", "compact_percent: 70")
	if n := strings.Count(r.out, "config.ktav updated (old one saved as"); n != 3 {
		t.Errorf("%d updates, want 3:\n%s", n, r.out)
	}

	// A dash removes a setting; an empty answer keeps it.
	r = runUI(t, strings.Replace(uiConfig, "        provider: zai\n", "        provider: zai\n        default_model: glm-5.3\n", 1), "1\n3\n1\n\n1\n-\n0\n0\n0\n")
	has(t, r.out, "unchanged", "default_model: -")
	if pf := r.config(t).Profiles["zai"]; pf.DefaultModel != "" {
		t.Errorf("default_model after '-': %q", pf.DefaultModel)
	}
}

func TestInteractiveRefusesWrongValues(t *testing.T) {
	r := runUI(t, uiConfig, "1\n3\n3\n150\n2\nabc\n0\n0\n0\n")
	if strings.Count(r.out, "error: ") < 2 {
		t.Errorf("want an error for each wrong value:\n%s", r.out)
	}
	if r.text(t) != uiConfig {
		t.Errorf("a refused value changed the config:\n%s", r.text(t))
	}
}

func TestInteractiveSetsAndRemovesTheProfileProxy(t *testing.T) {
	r := runUI(t, uiConfig, "1\n3\n4\nsocks5://127.0.0.1:1080\n0\n0\n0\n")
	has(t, r.out, "proxy: socks5://127.0.0.1:1080 (its own)")
	if pf := r.config(t).Profiles["zai"]; pf.Proxy != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy = %q", pf.Proxy)
	}
	r = runUI(t, uiConfig, "1\n3\n4\nnone\n4\ninherit\n0\n0\n0\n")
	has(t, r.out, "proxy: none (its own)", "proxy: global (")
	if pf := r.config(t).Profiles["zai"]; pf.Proxy != "" {
		t.Errorf("proxy after inherit = %q", pf.Proxy)
	}
	r = runUI(t, uiConfig, "1\n3\n4\nftp://nope\n4\n\n0\n0\n0\n")
	has(t, r.out, "error: ", "unchanged")
	if r.text(t) != uiConfig {
		t.Error("a wrong proxy changed the config")
	}
}

func TestInteractiveMovesAProfileToAnotherProvider(t *testing.T) {
	r := runUI(t, uiConfig, "1\n1\n5\n1\n0\n0\n0\n")
	has(t, r.out, "move codex to which provider?", "1) zai")
	lacks(t, r.out, "1) work")
	if pf := r.config(t).Profiles["codex"]; pf.Provider != "zai" {
		t.Errorf("provider = %q", pf.Provider)
	}
	r = runUI(t, uiConfig, "1\n1\n5\n0\n0\n0\n0\n")
	if r.text(t) != uiConfig {
		t.Error("a cancelled move changed the config")
	}
}

func TestInteractiveRemovesAProfile(t *testing.T) {
	r := runUI(t, uiConfig, "1\n1\nr\ny\nn\n0\n0\n")
	cfg := r.config(t)
	if _, ok := cfg.Profiles["codex"]; ok {
		t.Error("profile codex is still there")
	}
	if _, ok := cfg.Providers["codex"]; ok {
		t.Error("provider codex was left although no profile uses it")
	}
	has(t, r.out, "remove profile codex? [y/N]", "also delete its Claude dir", `removed profile "codex"`)

	// No at the first question: nothing happens and the profile screen stays.
	r = runUI(t, uiConfig, "1\n1\nr\nn\n0\n0\n0\n")
	has(t, r.out, "aborted")
	if r.text(t) != uiConfig {
		t.Error("an aborted removal changed the config")
	}
}

func TestInteractivePurgeDeletesTheClaudeDirAndLogin(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CCGP_HOME", filepath.Join(root, "home"))
	path := filepath.Join(root, "config.ktav")
	if err := os.WriteFile(path, []byte(uiConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	store := creds.Store{Dir: filepath.Join(root, "creds")}
	base := filepath.Join(root, "claude")
	if err := os.MkdirAll(filepath.Join(base, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	login := store.OAuthPath("codex")
	if err := os.WriteFile(login, []byte("x: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	u := &ui{in: bufio.NewReader(strings.NewReader("1\n1\nr\ny\ny\n0\n0\n")), out: &out, now: func() time.Time { return peakNow },
		store: store, claudeBase: base, pauses: config.NewPeakPauses(store.Dir)}
	if err := u.run(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(base, "codex"), login} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived --purge:\n%s", p, out.String())
		}
	}
	has(t, out.String(), "deleted the Claude dir", "deleted the credentials of provider")
}

func TestInteractiveAddsAProfileLikeAnother(t *testing.T) {
	r := runUI(t, uiConfig, "1\na\n1\n3\n\n\n0\n0\n")
	cfg := r.config(t)
	pf, pr := cfg.Profiles["zai2"], cfg.Providers["zai2"]
	if pf == nil || pr == nil || pf.Provider != "zai2" || pr.BaseURL != "https://api.z.ai/api/anthropic" {
		t.Errorf("zai2: %+v on %+v", pf, pr)
	}
	has(t, r.out, "copy which profile?", "profile name [zai2]:", `created profile "zai2" with its own provider "zai2"`)

	// Like a Claude profile: no model question, a Claude profile comes out.
	r = runUI(t, uiConfig, "1\na\n1\n2\n\n0\n0\n")
	if pr := r.config(t).Providers["work2"]; pr == nil || pr.Type != config.ProviderTypeClaude {
		t.Errorf("work2: %+v", pr)
	}
	lacks(t, r.out, "catch-all model")
}

func TestInteractiveAddsAProfileOnAnExistingProvider(t *testing.T) {
	r := runUI(t, uiConfig, "1\na\n2\n2\nextra\nmy-model\n123456\n0\n0\n")
	pf := r.config(t).Profiles["extra"]
	if pf == nil || pf.Provider != "zai" || len(pf.Models) != 1 || pf.Models[0].Model != "my-model" || pf.Models[0].ContextWindow != 123456 {
		t.Errorf("extra: %+v", pf)
	}
	has(t, r.out, "on which provider?", "1) codex", "2) zai", `created profile "extra" sharing provider "zai"`)
	lacks(t, r.out, "3) work")

	// The window has a default.
	r = runUI(t, uiConfig, "1\na\n2\n2\nextra\nmy-model\n\n0\n0\n")
	if pf := r.config(t).Profiles["extra"]; pf == nil || pf.Models[0].ContextWindow != defaultCatchallWindow {
		t.Errorf("extra: %+v", pf)
	}

	// A window that is no number is refused, nothing is added.
	r = runUI(t, uiConfig, "1\na\n2\n2\nextra\nmy-model\nabc\n0\n0\n")
	has(t, r.out, "error: the context window must be a positive number")
	if r.text(t) != uiConfig {
		t.Error("a refused window changed the config")
	}
}

func TestInteractiveAddsABackendOfEachKind(t *testing.T) {
	r := runUI(t, uiConfig, "1\na\n3\n3\nhttps://example.com/anthropic\nraw\n\n0\n0\n")
	if pr := r.config(t).Providers["raw"]; pr == nil || pr.Type != config.ProviderTypeAnthropicCompat || pr.BaseURL != "https://example.com/anthropic" {
		t.Errorf("raw: %+v", pr)
	}

	r = runUI(t, uiConfig, "1\na\n3\n2\n\napi\ngpt-x\n\n0\n0\n")
	cfg := r.config(t)
	if pr := cfg.Providers["api"]; pr == nil || pr.Type != config.ProviderTypeCodexAPI {
		t.Errorf("api: %+v", pr)
	}
	if pf := cfg.Profiles["api"]; pf == nil || len(pf.Models) != 1 || pf.Models[0].Model != "gpt-x" || pf.Models[0].ContextWindow != defaultCatchallWindow {
		t.Errorf("api profile: %+v", pf)
	}

	r = runUI(t, uiConfig, "1\na\n3\n1\n\n\n0\n0\n")
	if pr := r.config(t).Providers["codex2"]; pr == nil || pr.Type != config.ProviderTypeCodex {
		t.Errorf("codex2: %+v", pr)
	}

	r = runUI(t, uiConfig, "1\na\n3\n4\nmine\n0\n0\n")
	if pr := r.config(t).Providers["mine"]; pr == nil || pr.Type != config.ProviderTypeClaude {
		t.Errorf("mine: %+v", pr)
	}
	has(t, r.out, "created Claude profile", "profile name [work2]:")
	lacks(t, r.out, "catch-all model")
}

func TestInteractiveAddRefusals(t *testing.T) {
	for name, script := range map[string]string{
		"cancel at the first choice":   "1\na\n0\n0\n0\n",
		"cancel the backend kind":      "1\na\n3\n0\n0\n0\n",
		"cancel the profile to copy":   "1\na\n1\n0\n0\n0\n",
		"cancel the provider":          "1\na\n2\n0\n0\n0\n",
		"a url is required":            "1\na\n3\n3\n\n0\n0\n",
		"a name that is taken":         "1\na\n2\n2\nzai\n\n0\n0\n",
		"a name that cannot be a name": "1\na\n2\n2\nBad Name\n\n0\n0\n",
	} {
		r := runUI(t, uiConfig, script)
		if r.text(t) != uiConfig {
			t.Errorf("%s: the config changed:\n%s", name, r.text(t))
		}
	}
	r := runUI(t, uiConfig, "1\na\n3\n3\n\n0\n0\n")
	has(t, r.out, "a base URL is required for anthropic_compat")
	r = runUI(t, uiConfig, "1\na\n2\n2\nzai\n\n0\n0\n")
	has(t, r.out, `error: name "zai" is already in use`)
}

func TestInteractiveListsAndShowsProviders(t *testing.T) {
	r := runUI(t, uiConfig, "2\n3\n0\n0\n0\n")
	has(t, r.out,
		"1) codex codex codex", "2) work claude work", "3) zai anthropic_compat zai", "a) add a provider",
		"provider zai", "type: anthropic_compat", "base_url: https://api.z.ai/api/anthropic", "api_key_env: -",
		"peak hours: none", "profiles: zai", "1) set base_url", "2) set api_key_env", "3) peak hours")

	r = runUI(t, uiConfig, "2\n1\n0\n0\n0\n")
	has(t, r.out, "provider codex", "1) set base_url", "3) peak hours")
	lacks(t, r.out, "set api_key_env")

	r = runUI(t, uiConfig, "2\n2\n0\n0\n0\n")
	has(t, r.out, "provider work", "type: claude", "profiles: work")
	lacks(t, r.out, "set base_url", "peak hours", "base_url:")
}

func TestInteractiveEditsAProvider(t *testing.T) {
	r := runUI(t, uiConfig, "2\n3\n1\nhttps://example.internal/api\n2\nZAI_KEY\n0\n0\n0\n")
	pr := r.config(t).Providers["zai"]
	if pr.BaseURL != "https://example.internal/api" || pr.APIKeyEnv != "ZAI_KEY" {
		t.Errorf("provider: %+v", pr)
	}
	has(t, r.out, "base_url: https://example.internal/api", "api_key_env: ZAI_KEY")

	r = runUI(t, strings.Replace(uiConfig, "        base_url: https://api.z.ai/api/anthropic\n", "        base_url: https://api.z.ai/api/anthropic\n        api_key_env: ZAI_KEY\n", 1),
		"2\n3\n2\n-\n0\n0\n0\n")
	if pr := r.config(t).Providers["zai"]; pr.APIKeyEnv != "" {
		t.Errorf("api_key_env after '-': %q", pr.APIKeyEnv)
	}

	// An anthropic_compat provider cannot do without its base_url.
	r = runUI(t, uiConfig, "2\n3\n1\n-\n0\n0\n0\n")
	has(t, r.out, "error: ")
	if r.text(t) != uiConfig {
		t.Error("the refused removal changed the config")
	}
}

func TestInteractiveAddsAProviderFromTheProviderList(t *testing.T) {
	r := runUI(t, uiConfig, "2\na\n3\nhttps://example.com/anthropic\nraw\n\n0\n0\n")
	cfg := r.config(t)
	if pr := cfg.Providers["raw"]; pr == nil || cfg.Profiles["raw"] == nil {
		t.Errorf("raw: %+v", pr)
	}
	has(t, r.out, "the kind of backend")
}

func TestInteractiveManagesPeakHours(t *testing.T) {
	r := runUI(t, uiConfig, "2\n3\n3\n1\nmon-fri 14:00-18:00\n4\n+08:00\n0\n0\n0\n0\n")
	pr := r.config(t).Providers["zai"]
	if len(pr.Peak) != 1 || pr.Peak[0].Days != "mon-fri" || pr.PeakTZ != "+08:00" {
		t.Errorf("peak hours: %+v in %q", pr.Peak, pr.PeakTZ)
	}
	has(t, r.out, "peak hours of zai", "zai: no peak hours", "peak hours: mon-fri 14:00-18:00 (+08:00 time); PEAK NOW")

	// Add, remove by number, clear.
	r = runUI(t, uiConfig, "2\n3\n3\n1\nmon-fri 14:00-18:00\n2\nsat,sun 10:00-12:00\n3\n1\n0\n0\n0\n0\n")
	if pr := r.config(t).Providers["zai"]; len(pr.Peak) != 1 || pr.Peak[0].Days != "sat,sun" {
		t.Errorf("peak hours after add and rm: %+v", pr.Peak)
	}
	r = runUI(t, uiConfig, "2\n3\n3\n1\nmon-fri 14:00-18:00\n5\n0\n0\n0\n0\n")
	if r.text(t) != uiConfig {
		t.Errorf("clear did not give the config back:\n%s", r.text(t))
	}
}

func TestInteractivePausesAndResumesPeakHours(t *testing.T) {
	r := runUI(t, uiConfig, "2\n3\n3\n1\nmon-fri 14:00-18:00\n4\n+08:00\n6\n\n7\n0\n0\n0\n0\n")
	has(t, r.out,
		"peak hours of zai do not refuse requests until 18:00 +08:00",
		"now: peak, but paused: requests go through until 18:00 +08:00",
		"peak hours of zai apply again")
	if _, ok := config.NewPeakPauses(r.store.Dir).Until("zai", peakNow); ok {
		t.Error("the pause survived the resume")
	}
	// A pause of a given length.
	r = runUI(t, uiConfig, "2\n3\n3\n1\nmon-fri 14:00-18:00\n6\n2h\n0\n0\n0\n0\n")
	if _, ok := config.NewPeakPauses(r.store.Dir).Until("zai", peakNow); !ok {
		t.Error("no pause after 'pause 2h'")
	}
}

func TestInteractivePeakMenuReportsWrongInput(t *testing.T) {
	r := runUI(t, uiConfig, "2\n3\n3\n1\nbogus\n2\n\n4\nMars/Base\n0\n0\n0\n0\n")
	has(t, r.out, "error: ", "nothing entered")
	if r.text(t) != uiConfig {
		t.Errorf("wrong input changed the config:\n%s", r.text(t))
	}
}

func TestInteractiveEndOfInputStopsAnywhere(t *testing.T) {
	for name, script := range map[string]string{
		"in a value":     "1\n3\n1\n",
		"in a menu":      "1\n3\n",
		"in a question":  "1\na\n2\n2\nextra\n",
		"in a peak menu": "2\n3\n3\n",
		"in a confirm":   "1\n1\nr\n",
		"in a pause":     "2\n3\n3\n6\n",
	} {
		r := runUI(t, uiConfig, script)
		if r.text(t) != uiConfig || !strings.HasSuffix(r.out, "\n") {
			t.Errorf("%s: config changed or no final line:\n%s", name, r.out)
		}
	}
}

func TestInteractiveNeedsAConfigFile(t *testing.T) {
	t.Setenv("CCGP_HOME", t.TempDir())
	t.Setenv(config.ConfigFileEnv, "")
	t.Setenv("PROXY_TOKEN", strings.Repeat("t", 64))
	t.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")
	u := &ui{in: bufio.NewReader(strings.NewReader("")), out: io.Discard, now: time.Now}
	err := u.run()
	if err == nil || !strings.Contains(err.Error(), "needs config.ktav") {
		t.Errorf("run = %v, want the env-only refusal", err)
	}
}

func TestInteractiveCommandIsRegistered(t *testing.T) {
	if err := interactiveCmd([]string{"extra"}); err == nil {
		t.Error("an argument was accepted")
	}
	for _, name := range []string{"--interactive", "-i"} {
		inv, err := parseInvocation([]string{name})
		if err != nil || inv.cmd == nil || inv.cmd.name != name || !inv.cmd.ensureConfig {
			t.Errorf("%s: %+v, %v", name, inv, err)
		}
	}
	// Nothing but the flag starts it: a bare word is a profile, no word is help.
	if inv, _ := parseInvocation([]string{"interactive"}); inv.cmd != nil || inv.profile != "interactive" {
		t.Errorf("a bare word started a command: %+v", inv)
	}
	if inv, _ := parseInvocation(nil); inv.cmd != nil {
		t.Errorf("no arguments started a command: %+v", inv)
	}
}
