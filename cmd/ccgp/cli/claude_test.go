package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/core"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/ops"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/tune"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
	peakcfg "github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

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

func saveKey(t *testing.T, provider string, key string) {
	t.Helper()
	store, err := common.CredsStore()
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
	if err := core.ProfileAdd(&out, core.ProfileAddOpts{Type: config.ProviderTypeClaudeAPI, Name: "api"}, path, cfg, creds.Store{}); err != nil {
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
	if cfgedit.HasManaged(string(text), "api") || strings.Contains(string(text), "base_url: https://api.anthropic.com") {
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
	if err := core.ProfileAdd(&out, core.ProfileAddOpts{Type: config.ProviderTypeClaudeAPI, Name: "gw", BaseURL: "https://gw.example"}, path, cfg2, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	cfg3 := loadTestConfig(t)
	if pr := cfg3.Providers["gw"]; pr == nil || pr.BaseURL != "https://gw.example" {
		t.Fatalf("provider gw = %+v", pr)
	}
	if !strings.Contains(out.String(), "server https://gw.example,") {
		t.Errorf("output does not name the server:\n%s", out.String())
	}
	if err := core.ProfileAdd(&bytes.Buffer{}, core.ProfileAddOpts{Like: "gw", Name: "gw2"}, path, cfg3, creds.Store{}); err != nil {
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
		opts core.ProfileAddOpts
		want string
	}{
		{"model", core.ProfileAddOpts{Type: "claude_api", Name: "w2", Model: "m", HasModel: true}, "do not apply"},
		{"window", core.ProfileAddOpts{Type: "claude_api", Name: "w2", Window: 1000, HasWindow: true}, "do not apply"},
		{"share the key", core.ProfileAddOpts{Provider: "api", Name: "w2"}, "own profile"},
		{"not a server", core.ProfileAddOpts{Type: "claude_api", Name: "w2", BaseURL: "gw.example"}, "base_url must be an http(s) URL"},
		{"not a type", core.ProfileAddOpts{Type: "claude_key", Name: "w2"}, "claude_api"},
	}
	for _, tt := range tests {
		if err := core.ProfileAdd(&bytes.Buffer{}, tt.opts, path, cfg, creds.Store{}); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}
	if cfg2 := loadTestConfig(t); len(cfg2.Profiles) != len(cfg.Profiles) {
		t.Errorf("a refusal changed the config: %v", common.Names(cfg2.Profiles))
	}
}

func TestClaudeAPIViews(t *testing.T) {
	writeTestConfig(t, testConfig)
	cfg := addAPI(t, "api", "")
	store := creds.Store{Dir: t.TempDir()}
	pauses := peakcfg.NewPauses(store.Dir)
	show := func(cfg *config.Config) string {
		return tune.ProviderText(cfg, store, pauses, time.Now(), "api")
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

	rows := core.BuildProfileRows(cfg, store, t.TempDir())
	var row core.ProfileRow
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
	if got := common.LoginState(store, cfg.Providers["api"]); got != "API key saved" {
		t.Errorf("login state with a key = %q", got)
	}
	if ops.LoginIsWarn(common.LoginState(store, cfg.Providers["api"])) {
		t.Error("a saved key is not a doctor warning")
	}

	// The server is the provider's own setting, set the way any provider's is.
	if err := core.ProviderSet(io.Discard, cfg, []string{"api", "base_url", "https://gw.example"}, false); err != nil {
		t.Fatal(err)
	}
	cfg = loadTestConfig(t)
	if text = show(cfg); !strings.Contains(text, "base_url     https://gw.example\n") || strings.Contains(text, "(the default)") {
		t.Errorf("provider text after set base_url:\n%s", text)
	}
	if err := core.ProviderSet(io.Discard, cfg, []string{"api", "base_url", "gw.example"}, false); err == nil || !strings.Contains(err.Error(), "http(s) URL") {
		t.Errorf("a server without a scheme: err = %v", err)
	}
	if err := core.ProviderSet(io.Discard, cfg, []string{"api", "base_url"}, true); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Providers["api"].EffectiveBaseURL(); got != config.OfficialAnthropicURL {
		t.Errorf("after unset base_url the server is %q", got)
	}

	// JSON shows the server in use.
	rowsJSON := core.ProviderRows(loadTestConfig(t), store, pauses, time.Now())
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
		"which": func() error { _, err := core.WhichRoute(cfg, "api", "sonnet"); return err }(),
		"proxy": func() error { _, err := tune.ProxyValue(cfg, "socks5://127.0.0.1:1080", "api"); return err }(),
		"provider proxy": func() error {
			_, err := tune.ProviderProxyValue(cfg, "socks5://127.0.0.1:1080", "api")
			return err
		}(),
	} {
		if err == nil || !strings.Contains(err.Error(), "does not go through ccgp") {
			t.Errorf("%s of a claude_api profile: err = %v", name, err)
		}
	}
	if err := core.ProfileSet(io.Discard, cfg, []string{"api", "default_model", "m"}, false); err == nil || !strings.Contains(err.Error(), "does not go through ccgp") {
		t.Errorf("--profile set on a claude_api profile: err = %v", err)
	}
	if got := common.ProviderNames(cfg); contains(got, "api") {
		t.Errorf("a claude_api provider is not a backend to move a profile to: %v", got)
	}
	if _, err := tune.PeakProvider(cfg, "api"); err == nil || !strings.Contains(err.Error(), "no peak hours") {
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
		if err := core.ProfileRemove(&out, core.ProfileRmOpts{Name: "api", Yes: true, Purge: purge}, path, cfg, store, base, strings.NewReader("")); err != nil {
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

func TestLaunchClaudeAPIPassesItsKey(t *testing.T) {
	ownSetup(t)
	out := fakeClaude(t)
	addAPI(t, "api", "")
	saveKey(t, "api", "sk-ant-profile")
	t.Setenv("ANTHROPIC_API_KEY", "sk-parent")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "token-parent")
	t.Setenv("ANTHROPIC_BASE_URL", "https://parent.example")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-parent")

	got := launchedEnv(t, out, 3, core.Launch("api", []string{"--foo", "bar"}))
	base, err := common.ClaudeHomeBase()
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

	got := launchedEnv(t, out, 3, core.Launch("gw", nil))
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
	if err := core.ProviderSet(io.Discard, cfg, []string{"api", "api_key_env", "CCGP_TEST_ANTHROPIC_KEY"}, false); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CCGP_TEST_ANTHROPIC_KEY", "")
	if err := core.Launch("api", nil); err == nil || !strings.Contains(err.Error(), "$CCGP_TEST_ANTHROPIC_KEY") {
		t.Fatalf("launch with the variable empty: err = %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("claude was started without a key")
	}

	t.Setenv("CCGP_TEST_ANTHROPIC_KEY", "sk-ant-env")
	got := launchedEnv(t, out, 3, core.Launch("api", nil))
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
		got := launchedEnv(t, out, 3, core.Launch(c.profile, nil))
		if has := envHas(got, "ANTHROPIC_API_KEY="); has != c.wantKey {
			t.Errorf("%s: ANTHROPIC_API_KEY present = %v, want %v:\n%s", c.profile, has, c.wantKey, got)
		}
		base, _ := common.ClaudeHomeBase()
		if !strings.Contains(got, "CLAUDE_CONFIG_DIR="+filepath.Join(base, c.wantHome)) {
			t.Errorf("%s: wrong config dir:\n%s", c.profile, got)
		}
	}
}

func TestOwnEnvAddsTheProfilesKeyAfterTheDrop(t *testing.T) {
	env := []string{"PATH=/bin", "ANTHROPIC_API_KEY=parent", "ANTHROPIC_BASE_URL=https://parent", "CLAUDE_CONFIG_DIR=/users/own"}
	joined := strings.Join(core.OwnEnv(env, "/profile/api", "ANTHROPIC_API_KEY=own", "ANTHROPIC_BASE_URL=https://gw"), "\n")
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
	inv, err := ParseInvocation([]string{"--profile", "-h"})
	if err != nil || inv.Cmd == nil {
		t.Fatalf("parseInvocation: %v", err)
	}
	if text := commandHelpText(*inv.Cmd); !strings.Contains(text, "--type claude_api") || !strings.Contains(text, "--base-url") {
		t.Errorf("--profile help does not mention --type claude_api [--base-url]:\n%s", text)
	}
	if got := CompleteWords([]string{"ccgp", "--profile", "add", "x", "--type", "claude_"}, testNames); len(got) != 1 || got[0] != "claude_api" {
		t.Errorf("--type claude_ = %q", got)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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

func profileByName(t *testing.T, cfg *config.Config, name string) *config.Profile {
	t.Helper()
	pf, ok := cfg.Profiles[name]
	if !ok {
		t.Fatalf("profile %q missing after the edit (have %v)", name, common.Names(cfg.Profiles))
	}
	return pf
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

func TestClaudeEnv(t *testing.T) {
	in := []string{
		"PATH=/bin",
		"ANTHROPIC_API_KEY=sk-ant",
		"anthropic_base_url=http://old",
		"ANTHROPIC_AUTH_TOKEN=x",
		"ANTHROPIC_MODEL=keep",
		"CLAUDE_CODE_OAUTH_TOKEN=official",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CONFIG_DIR=/real/.claude",
	}
	got := core.ClaudeEnv(in, "http://127.0.0.1:60093/call/tok", "/iso")
	want := []string{
		"PATH=/bin",
		"ANTHROPIC_MODEL=keep",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:60093/call/tok",
		"ANTHROPIC_AUTH_TOKEN=ccgp",
		"CLAUDE_CONFIG_DIR=/iso",
		"ENABLE_CLAUDEAI_MCP_SERVERS=false",
		"CLAUDE_CODE_AUTO_MODE_SERVER=0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("claudeEnv =\n%q\nwant\n%q", got, want)
	}

	// Values the user already set are kept.
	got = core.ClaudeEnv([]string{"claude_code_auto_mode_server=1", "ENABLE_CLAUDEAI_MCP_SERVERS=true"}, "http://x", "/iso")
	want = []string{
		"claude_code_auto_mode_server=1", "ENABLE_CLAUDEAI_MCP_SERVERS=true",
		"ANTHROPIC_BASE_URL=http://x", "ANTHROPIC_AUTH_TOKEN=ccgp", "CLAUDE_CONFIG_DIR=/iso",
	}
	if !slices.Equal(got, want) {
		t.Errorf("user overrides: claudeEnv =\n%q\nwant\n%q", got, want)
	}
}

func TestCompactEnv(t *testing.T) {
	pf := &config.Profile{ContextWindow: 800000, CompactPercent: 70}
	got := core.CompactEnv([]string{"PATH=/bin"}, pf, "sonnet")
	want := []string{"PATH=/bin", "CLAUDE_CODE_AUTO_COMPACT_WINDOW=800000", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=70"}
	if !slices.Equal(got, want) {
		t.Errorf("compactEnv = %q, want %q", got, want)
	}

	// User-set values win; no window means no change.
	got = core.CompactEnv([]string{"claude_code_auto_compact_window=123", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=50"}, pf, "sonnet")
	if len(got) != 2 {
		t.Errorf("overrode user env: %q", got)
	}
	if got = core.CompactEnv([]string{"PATH=/bin"}, &config.Profile{}, "sonnet"); len(got) != 1 {
		t.Errorf("no window: %q", got)
	}
	if got = core.CompactEnv([]string{"PATH=/bin"}, nil, "sonnet"); len(got) != 1 {
		t.Errorf("nil profile: %q", got)
	}
	// Percent alone (window known) only sets the percent when asked.
	got = core.CompactEnv(nil, &config.Profile{ContextWindow: 300000}, "sonnet")
	if !slices.Equal(got, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=300000"}) {
		t.Errorf("window only: %q", got)
	}
}

// Claude Code takes 100k-1M and prints a warning for any other value, so
// the window is clamped into that range.
func TestCompactEnvClampsToWhatClaudeCodeAccepts(t *testing.T) {
	for win, want := range map[int64]string{
		8_000:     "100000",
		60_000:    "100000",
		100_000:   "100000",
		872_000:   "872000",
		1_000_000: "1000000",
		2_000_000: "1000000",
	} {
		got := core.CompactEnv(nil, &config.Profile{ContextWindow: win}, "sonnet")
		if !slices.Equal(got, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=" + want}) {
			t.Errorf("window %d: %q, want %s", win, got, want)
		}
	}
}

// The window comes from the rule that serves the session's main model; a
// smaller window on another rule (Haiku) does not shrink the main session.
func TestCompactEnvUsesMainModelRule(t *testing.T) {
	pf := &config.Profile{Models: []config.ModelRule{
		{Match: "*opus*", Model: "sol", ContextWindow: 872000},
		{Match: "*sonnet*", Model: "luna", ContextWindow: 872000},
		{Match: "*haiku*", Model: "old-luna", ContextWindow: 272000},
		{Match: "*", Model: "luna", ContextWindow: 872000},
	}}
	for model, want := range map[string]string{
		"sonnet":            "CLAUDE_CODE_AUTO_COMPACT_WINDOW=872000",
		"claude-opus-5-5":   "CLAUDE_CODE_AUTO_COMPACT_WINDOW=872000",
		"haiku":             "CLAUDE_CODE_AUTO_COMPACT_WINDOW=272000", // when Haiku is the main model
		"custom-model-name": "CLAUDE_CODE_AUTO_COMPACT_WINDOW=872000",
	} {
		if got := core.CompactEnv(nil, pf, model); !slices.Equal(got, []string{want}) {
			t.Errorf("main model %q: %q, want %q", model, got, want)
		}
	}

	// A matching rule without a window falls back to the smallest known one.
	partial := &config.Profile{Models: []config.ModelRule{
		{Match: "*sonnet*", Model: "a"},
		{Match: "*haiku*", Model: "b", ContextWindow: 272000},
	}}
	if got := core.CompactEnv(nil, partial, "sonnet"); !slices.Equal(got, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=272000"}) {
		t.Errorf("fallback: %q", got)
	}
}

func TestMainModel(t *testing.T) {
	dir := t.TempDir()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if got := core.MainModel(nil, env(nil), dir); got != "sonnet" {
		t.Errorf("default = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := core.MainModel(nil, env(nil), dir); got != "opus" {
		t.Errorf("settings model = %q", got)
	}
	if got := core.MainModel(nil, env(map[string]string{"ANTHROPIC_MODEL": "haiku"}), dir); got != "haiku" {
		t.Errorf("env model = %q", got)
	}
	if got := core.MainModel([]string{"-p", "hi", "--model", "fable"}, env(map[string]string{"ANTHROPIC_MODEL": "haiku"}), dir); got != "fable" {
		t.Errorf("--model = %q", got)
	}
	if got := core.MainModel([]string{"--model=claude-sonnet-5-5"}, env(nil), dir); got != "claude-sonnet-5-5" {
		t.Errorf("--model= = %q", got)
	}
	// A trailing --model without a value is ignored.
	if got := core.MainModel([]string{"--model"}, env(nil), dir); got != "opus" {
		t.Errorf("dangling --model = %q", got)
	}
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

func TestLaunchOwnRunsClaudeDirectly(t *testing.T) {
	userDir, profileDir := ownSetup(t)
	out := fakeClaude(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-official")
	t.Setenv("ANTHROPIC_BASE_URL", "https://official.example")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-official")

	err := core.Launch("work", []string{"--foo", "bar"})
	var ee common.ExitError
	if !errors.As(err, &ee) || int(ee) != 3 {
		t.Fatalf("launch err = %v, want the exit status 3 of claude", err)
	}
	b, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("claude was not started: %v", rerr)
	}
	got := string(b)
	if !strings.Contains(got, "ARGS=--foo bar") {
		t.Errorf("arguments not passed:\n%s", got)
	}
	if !strings.Contains(got, "CLAUDE_CONFIG_DIR="+profileDir) {
		t.Errorf("CLAUDE_CONFIG_DIR is not the profile dir %s:\n%s", profileDir, got)
	}
	for _, bad := range []string{"sk-official", "/call/"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q reached a Claude profile:\n%s", bad, got)
		}
	}
	for _, bad := range []string{"ANTHROPIC_", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_AUTO_COMPACT", "CLAUDE_AUTOCOMPACT", "ENABLE_CLAUDEAI_MCP_SERVERS"} {
		if envHas(got, bad) {
			t.Errorf("the variable %s reached a Claude profile:\n%s", bad, got)
		}
	}

	// Settings crossed, nothing of the account did.
	if body, _ := os.ReadFile(filepath.Join(profileDir, "CLAUDE.md")); string(body) != "rules v1" {
		t.Errorf("CLAUDE.md = %q", body)
	}
	if body, _ := os.ReadFile(filepath.Join(profileDir, "skills", "s1", "SKILL.md")); string(body) != "skill" {
		t.Errorf("skill = %q", body)
	}
	for _, name := range []string{".claude.json", ".credentials.json", "history.jsonl", "projects"} {
		if _, err := os.Stat(filepath.Join(profileDir, name)); err == nil {
			t.Errorf("%s was copied into a Claude profile", name)
		}
	}

	// The login the user makes inside survives the next launch, and a nested
	// launch (CLAUDE_CONFIG_DIR already the profile dir) still mirrors the
	// user's own dir.
	if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte("own login"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "CLAUDE.md"), []byte("rules v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", profileDir)
	if err := core.Launch("work", nil); !errors.As(err, &ee) {
		t.Fatalf("second launch err = %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(profileDir, ".credentials.json")); string(body) != "own login" {
		t.Errorf("the profile's login was removed: %q", body)
	}
	if body, _ := os.ReadFile(filepath.Join(profileDir, "CLAUDE.md")); string(body) != "rules v2" {
		t.Errorf("a nested launch did not mirror the user's dir: %q", body)
	}
}

func TestLaunchOwnWithoutClaudeInPath(t *testing.T) {
	_, profileDir := ownSetup(t)
	t.Setenv("PATH", t.TempDir())
	err := core.Launch("work", nil)
	if err == nil || !strings.Contains(err.Error(), "claude not found in PATH") {
		t.Fatalf("launch err = %v", err)
	}
	if _, err := os.Stat(profileDir); err == nil {
		t.Error("the profile dir was created although claude is missing")
	}
}

func TestLoginStateOfClaudeProfile(t *testing.T) {
	_, profileDir := ownSetup(t)
	cfg := loadTestConfig(t)
	pr := cfg.Providers["work"]
	store := creds.Store{Dir: t.TempDir()}
	if got := common.LoginState(store, pr); got != "sign in inside Claude Code (/login)" {
		t.Errorf("new profile: %q", got)
	}
	if ops.LoginIsWarn(common.LoginState(store, pr)) {
		t.Error("a Claude profile that is not signed in yet is not a doctor warning")
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := common.LoginState(store, pr); got != "signed in inside Claude Code" {
		t.Errorf("signed in: %q", got)
	}
}

func TestStatusShowsClaudeProfile(t *testing.T) {
	ownSetup(t)
	var runErr error
	out := captureStdout(t, func() { runErr = Status([]string{"--json"}) })
	_ = runErr // exit 3 when no server runs
	for _, want := range []string{`"type": "claude"`, "sign in inside Claude Code (/login)", `"name": "work"`} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestHelpAndCompletionKnowClaudeType(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)
	if !strings.Contains(buf.String(), "--type claude") {
		t.Errorf("help does not mention --type claude:\n%s", buf.String())
	}
	inv, err := ParseInvocation([]string{"--profile", "-h"})
	if err != nil || inv.Cmd == nil {
		t.Fatalf("parseInvocation: %v", err)
	}
	if text := commandHelpText(*inv.Cmd); !strings.Contains(text, "--type claude") {
		t.Errorf("--profile help does not mention --type claude:\n%s", text)
	}
	got := CompleteWords([]string{"ccgp", "--profile", "add", "x", "--type", ""}, testNames)
	for _, want := range []string{"codex", "codex_api", "anthropic_compat", "claude"} {
		if !contains(got, want) {
			t.Errorf("--type does not offer %q: %q", want, got)
		}
	}
	if got := CompleteWords([]string{"ccgp", "--profile", "add", "x", "--type", "cl"}, testNames); !reflect.DeepEqual(got, []string{"claude", "claude_api"}) {
		t.Errorf("--type cl = %q", got)
	}
}
