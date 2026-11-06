package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

// addAPI adds a claude_api profile to the config in use (CCGP_CONFIG) and
// returns the reloaded config.
func addAPI(t *testing.T, name, baseURL string) *config.Config {
	t.Helper()
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Type: config.ProviderTypeClaudeAPI, Name: name, BaseURL: baseURL},
		os.Getenv(config.ConfigFileEnv), loadTestConfig(t), creds.Store{}); err != nil {
		t.Fatal(err)
	}
	return loadTestConfig(t)
}

func saveKey(t *testing.T, provider, key string) {
	t.Helper()
	store, err := credsStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAPIKey(provider, key); err != nil {
		t.Fatal(err)
	}
}

func TestProfileAddClaudeAPI(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	var out bytes.Buffer
	if err := profileAdd(&out, profileAddOpts{Type: config.ProviderTypeClaudeAPI, Name: "api"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	cfg2 := loadTestConfig(t)
	pr := cfg2.Providers["api"]
	if pr == nil || pr.Type != config.ProviderTypeClaudeAPI || pr.BaseURL != "" || pr.EffectiveBaseURL() != config.OfficialAnthropicURL {
		t.Fatalf("provider = %+v", pr)
	}
	if pf := profileByName(t, cfg2, "api"); pf.Provider != "api" || len(pf.Models) != 0 {
		t.Errorf("profile = %+v", pf)
	}
	text, _ := os.ReadFile(path)
	if config.HasManaged(string(text), "api") || strings.Contains(string(text), "base_url: https://api.anthropic.com") {
		t.Errorf("a claude_api profile must get neither a model block nor the default server written:\n%s", text)
	}
	for _, want := range []string{`created Claude API profile "api"`, config.OfficialAnthropicURL + " (the official one)", "ccgp --login api", "nothing goes through ccgp"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "models apply") {
		t.Errorf("output offers model mapping:\n%s", out.String())
	}

	// Another server, and --like keeps it for the next key.
	out.Reset()
	if err := profileAdd(&out, profileAddOpts{Type: config.ProviderTypeClaudeAPI, Name: "gw", BaseURL: "https://gw.example"}, path, cfg2, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	cfg3 := loadTestConfig(t)
	if pr := cfg3.Providers["gw"]; pr == nil || pr.BaseURL != "https://gw.example" {
		t.Fatalf("provider gw = %+v", pr)
	}
	if !strings.Contains(out.String(), "server https://gw.example,") {
		t.Errorf("output does not name the server:\n%s", out.String())
	}
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Like: "gw", Name: "gw2"}, path, cfg3, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	if pr := loadTestConfig(t).Providers["gw2"]; pr == nil || pr.Type != config.ProviderTypeClaudeAPI || pr.BaseURL != "https://gw.example" {
		t.Errorf("--like of a claude_api profile gave %+v", pr)
	}
}

func TestProfileAddClaudeAPIRefusals(t *testing.T) {
	path, _ := writeTestConfig(t, testConfig)
	cfg := addAPI(t, "api", "")
	tests := []struct {
		name string
		opts profileAddOpts
		want string
	}{
		{"model", profileAddOpts{Type: "claude_api", Name: "w2", Model: "m", HasModel: true}, "do not apply"},
		{"window", profileAddOpts{Type: "claude_api", Name: "w2", Window: 1000, HasWindow: true}, "do not apply"},
		{"share the key", profileAddOpts{Provider: "api", Name: "w2"}, "own profile"},
		{"not a server", profileAddOpts{Type: "claude_api", Name: "w2", BaseURL: "gw.example"}, "base_url must be an http(s) URL"},
		{"not a type", profileAddOpts{Type: "claude_key", Name: "w2"}, "claude_api"},
	}
	for _, tt := range tests {
		if err := profileAdd(&bytes.Buffer{}, tt.opts, path, cfg, creds.Store{}); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}
	if cfg2 := loadTestConfig(t); len(cfg2.Profiles) != len(cfg.Profiles) {
		t.Errorf("a refusal changed the config: %v", names(cfg2.Profiles))
	}
}

func TestClaudeAPIViews(t *testing.T) {
	writeTestConfig(t, testConfig)
	cfg := addAPI(t, "api", "")
	store := creds.Store{Dir: t.TempDir()}
	pauses := config.NewPeakPauses(store.Dir)
	show := func(cfg *config.Config) string {
		return providerText(cfg, store, pauses, time.Now(), "api")
	}

	text := show(cfg)
	for _, want := range []string{"type         claude_api", "base_url     " + config.OfficialAnthropicURL + " (the default)", "login        no API key (ccgp --login api)"} {
		if !strings.Contains(text, want) {
			t.Errorf("provider text lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"proxy", "peak hours"} {
		if strings.Contains(text, bad) {
			t.Errorf("a claude_api provider shows %q:\n%s", bad, text)
		}
	}

	rows := buildProfileRows(cfg, store, t.TempDir())
	var row profileRow
	for _, r := range rows {
		if r.Name == "api" {
			row = r
		}
	}
	if row.Type != "claude_api" || row.MainModel != "-" || row.Login != "no API key (ccgp --login api)" {
		t.Errorf("profile row = %+v", row)
	}
	if err := store.SaveAPIKey("api", "sk-ant-x"); err != nil {
		t.Fatal(err)
	}
	if got := loginState(store, cfg.Providers["api"]); got != "API key saved" {
		t.Errorf("login state with a key = %q", got)
	}
	if loginIsWarn(loginState(store, cfg.Providers["api"])) {
		t.Error("a saved key is not a doctor warning")
	}

	// The server is the provider's own setting, set the way any provider's is.
	if err := providerSet(io.Discard, cfg, []string{"api", "base_url", "https://gw.example"}, false); err != nil {
		t.Fatal(err)
	}
	cfg = loadTestConfig(t)
	if text = show(cfg); !strings.Contains(text, "base_url     https://gw.example\n") || strings.Contains(text, "(the default)") {
		t.Errorf("provider text after set base_url:\n%s", text)
	}
	if err := providerSet(io.Discard, cfg, []string{"api", "base_url", "gw.example"}, false); err == nil || !strings.Contains(err.Error(), "http(s) URL") {
		t.Errorf("a server without a scheme: err = %v", err)
	}
	if err := providerSet(io.Discard, cfg, []string{"api", "base_url"}, true); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Providers["api"].EffectiveBaseURL(); got != config.OfficialAnthropicURL {
		t.Errorf("after unset base_url the server is %q", got)
	}

	// JSON shows the server in use.
	rowsJSON := providerRows(loadTestConfig(t), store, pauses, time.Now())
	for _, r := range rowsJSON {
		if r.Name == "api" && (r.BaseURL != config.OfficialAnthropicURL || r.Proxy != "" || r.PeakHours != "") {
			t.Errorf("provider row = %+v", r)
		}
	}
}

func TestClaudeAPIGoesNowhereThroughCcgp(t *testing.T) {
	writeTestConfig(t, testConfig)
	cfg := addAPI(t, "api", "")
	for name, err := range map[string]error{
		"which": func() error { _, err := whichRoute(cfg, "api", "sonnet"); return err }(),
		"proxy": func() error { _, err := proxyValue(cfg, "socks5://127.0.0.1:1080", "api"); return err }(),
		"provider proxy": func() error {
			_, err := providerProxyValue(cfg, "socks5://127.0.0.1:1080", "api")
			return err
		}(),
	} {
		if err == nil || !strings.Contains(err.Error(), "does not go through ccgp") {
			t.Errorf("%s of a claude_api profile: err = %v", name, err)
		}
	}
	if err := profileSet(io.Discard, cfg, []string{"api", "default_model", "m"}, false); err == nil || !strings.Contains(err.Error(), "does not go through ccgp") {
		t.Errorf("--profile set on a claude_api profile: err = %v", err)
	}
	if got := providerNames(cfg); contains(got, "api") {
		t.Errorf("a claude_api provider is not a backend to move a profile to: %v", got)
	}
	if _, err := peakProvider(cfg, "api"); err == nil || !strings.Contains(err.Error(), "no peak hours") {
		t.Errorf("--peak of a claude_api provider: err = %v", err)
	}
}

func TestProfileRemoveClaudeAPIKeepsOrDeletesTheKey(t *testing.T) {
	path, _ := writeTestConfig(t, testConfig)
	base := t.TempDir()
	store := creds.Store{Dir: t.TempDir()}
	for _, purge := range []bool{false, true} {
		cfg := addAPI(t, "api", "")
		if err := store.SaveAPIKey("api", "sk-ant-x"); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := profileRemove(&out, profileRmOpts{Name: "api", Yes: true, Purge: purge}, path, cfg, store, base, strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
		if purge {
			if store.HasProviderCreds("api") || !strings.Contains(out.String(), "deleted the credentials of provider") {
				t.Errorf("--purge left the key (%v):\n%s", store.HasProviderCreds("api"), out.String())
			}
		} else if !store.HasProviderCreds("api") || !strings.Contains(out.String(), "left behind: the credentials of provider") {
			t.Errorf("without --purge the key must stay and be reported:\n%s", out.String())
		}
	}
}

// ---- the launch ----

// envHas reports whether the dump of claude's environment has a variable whose
// name starts with prefix: a line that starts with it. The dump also holds
// whatever the CI sets (a commit message, say), which may name these variables.
func envHas(dump, prefix string) bool {
	for _, l := range strings.Split(dump, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return true
		}
	}
	return false
}

func launchedEnv(t *testing.T, out string, wantStatus int, err error) string {
	t.Helper()
	var ee exitError
	if !errors.As(err, &ee) || int(ee) != wantStatus {
		t.Fatalf("launch err = %v, want the exit status %d of claude", err, wantStatus)
	}
	b, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("claude was not started: %v", rerr)
	}
	return string(b)
}

func TestLaunchClaudeAPIPassesItsKey(t *testing.T) {
	ownSetup(t)
	out := fakeClaude(t)
	addAPI(t, "api", "")
	saveKey(t, "api", "sk-ant-profile")
	t.Setenv("ANTHROPIC_API_KEY", "sk-parent")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "token-parent")
	t.Setenv("ANTHROPIC_BASE_URL", "https://parent.example")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-parent")

	got := launchedEnv(t, out, 3, launch("api", []string{"--foo", "bar"}))
	base, err := claudeHomeBase()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ARGS=--foo bar", "ANTHROPIC_API_KEY=sk-ant-profile", "CLAUDE_CONFIG_DIR=" + filepath.Join(base, "api")} {
		if !strings.Contains(got, want) {
			t.Errorf("claude's environment lacks %q:\n%s", want, got)
		}
	}
	// The official server is Claude Code's own default: nothing is set for it.
	for _, bad := range []string{"sk-parent", "token-parent", "parent.example", "oauth-parent", "/call/"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q reached a claude_api profile:\n%s", bad, got)
		}
	}
	for _, bad := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_AUTO_COMPACT", "CLAUDE_AUTOCOMPACT", "ENABLE_CLAUDEAI_MCP_SERVERS"} {
		if envHas(got, bad) {
			t.Errorf("the variable %s reached a claude_api profile:\n%s", bad, got)
		}
	}
	// Settings crossed, nothing of the user's account did.
	for _, name := range []string{".claude.json", ".credentials.json", "history.jsonl", "projects"} {
		if _, err := os.Stat(filepath.Join(base, "api", name)); err == nil {
			t.Errorf("%s was copied into a claude_api profile", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(base, "api", "CLAUDE.md")); string(b) != "rules v1" {
		t.Errorf("CLAUDE.md = %q", b)
	}
}

func TestLaunchClaudeAPIServer(t *testing.T) {
	ownSetup(t)
	out := fakeClaude(t)
	addAPI(t, "gw", "https://gw.example/anthropic")
	saveKey(t, "gw", "sk-ant-gw")
	t.Setenv("ANTHROPIC_BASE_URL", "https://parent.example")

	got := launchedEnv(t, out, 3, launch("gw", nil))
	for _, want := range []string{"ANTHROPIC_API_KEY=sk-ant-gw", "ANTHROPIC_BASE_URL=https://gw.example/anthropic"} {
		if !strings.Contains(got, want) {
			t.Errorf("claude's environment lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "parent.example") {
		t.Errorf("the parent's server reached the profile:\n%s", got)
	}
}

func TestLaunchClaudeAPIKeyFromTheEnvironment(t *testing.T) {
	ownSetup(t)
	out := fakeClaude(t)
	cfg := addAPI(t, "api", "")
	if err := providerSet(io.Discard, cfg, []string{"api", "api_key_env", "CCGP_TEST_ANTHROPIC_KEY"}, false); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CCGP_TEST_ANTHROPIC_KEY", "")
	if err := launch("api", nil); err == nil || !strings.Contains(err.Error(), "$CCGP_TEST_ANTHROPIC_KEY") {
		t.Fatalf("launch with the variable empty: err = %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("claude was started without a key")
	}

	t.Setenv("CCGP_TEST_ANTHROPIC_KEY", "sk-ant-env")
	got := launchedEnv(t, out, 3, launch("api", nil))
	if !strings.Contains(got, "ANTHROPIC_API_KEY=sk-ant-env") {
		t.Errorf("the key of api_key_env did not reach claude:\n%s", got)
	}
}

// A subscription profile and an API profile side by side: each sees only its own.
func TestLaunchClaudeAndClaudeAPIDoNotShareCredentials(t *testing.T) {
	ownSetup(t)
	out := fakeClaude(t)
	addAPI(t, "api", "")
	saveKey(t, "api", "sk-ant-profile")

	for _, c := range []struct {
		profile  string
		wantKey  bool
		wantHome string
	}{{"work", false, "work"}, {"api", true, "api"}} {
		_ = os.Remove(out)
		got := launchedEnv(t, out, 3, launch(c.profile, nil))
		if has := envHas(got, "ANTHROPIC_API_KEY="); has != c.wantKey {
			t.Errorf("%s: ANTHROPIC_API_KEY present = %v, want %v:\n%s", c.profile, has, c.wantKey, got)
		}
		base, _ := claudeHomeBase()
		if !strings.Contains(got, "CLAUDE_CONFIG_DIR="+filepath.Join(base, c.wantHome)) {
			t.Errorf("%s: wrong config dir:\n%s", c.profile, got)
		}
	}
}

func TestOwnEnvAddsTheProfilesKeyAfterTheDrop(t *testing.T) {
	env := []string{"PATH=/bin", "ANTHROPIC_API_KEY=parent", "ANTHROPIC_BASE_URL=https://parent", "CLAUDE_CONFIG_DIR=/users/own"}
	joined := strings.Join(ownEnv(env, "/profile/api", "ANTHROPIC_API_KEY=own", "ANTHROPIC_BASE_URL=https://gw"), "\n")
	for _, bad := range []string{"parent", "/users/own"} {
		if strings.Contains(joined, bad) {
			t.Errorf("%q survived:\n%s", bad, joined)
		}
	}
	for _, want := range []string{"PATH=/bin", "ANTHROPIC_API_KEY=own", "ANTHROPIC_BASE_URL=https://gw", "CLAUDE_CONFIG_DIR=/profile/api"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q lost:\n%s", want, joined)
		}
	}
}

func TestHelpAndCompletionKnowClaudeAPIType(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)
	if !strings.Contains(buf.String(), "--type claude_api") {
		t.Errorf("help does not mention --type claude_api:\n%s", buf.String())
	}
	inv, err := parseInvocation([]string{"--profile", "-h"})
	if err != nil || inv.cmd == nil {
		t.Fatalf("parseInvocation: %v", err)
	}
	if text := commandHelpText(*inv.cmd); !strings.Contains(text, "--type claude_api") || !strings.Contains(text, "--base-url") {
		t.Errorf("--profile help does not mention --type claude_api [--base-url]:\n%s", text)
	}
	if got := completeWords([]string{"ccgp", "--profile", "add", "x", "--type", "claude_"}, testNames); len(got) != 1 || got[0] != "claude_api" {
		t.Errorf("--type claude_ = %q", got)
	}
}
