package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
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

func TestCommandHelpTextHasEntryOrFallback(t *testing.T) {
	for _, c := range Commands() {
		var buf bytes.Buffer
		printCommandHelp(&buf, c)
		out := buf.String()
		if out == "" {
			t.Errorf("%s: empty help text", c.Name)
		}
		if !strings.Contains(out, c.Name) {
			t.Errorf("%s: help text does not mention the command:\n%s", c.Name, out)
		}
		if _, ok := CommandHelp[c.Name]; !ok && !strings.Contains(out, c.Desc) {
			t.Errorf("%s: fallback help must contain the one-line desc, got:\n%s", c.Name, out)
		}
	}
}

// Every visible command has its own long help with an example, so a new
// command cannot ship with the one-line fallback only.
func TestEveryVisibleCommandHasFullHelpWithExample(t *testing.T) {
	for _, c := range Commands() {
		if c.Hidden {
			continue
		}
		text, ok := CommandHelp[c.Name]
		if !ok {
			t.Errorf("%s: no entry in commandHelp", c.Name)
			continue
		}
		if !strings.Contains(text, "Example") {
			t.Errorf("%s: help has no example:\n%s", c.Name, text)
		}
		if !strings.HasPrefix(text, "ccgp "+c.Name) {
			t.Errorf("%s: help does not start with the command line:\n%s", c.Name, text)
		}
	}
}

// The long help names the flags the command's synopsis lists.
func TestCommandHelpMentionsSynopsisFlags(t *testing.T) {
	want := map[string][]string{
		"--status":      {"--json"},
		"--models":      {"--json", "apply", "-v"},
		"--which":       {"--json"},
		"--logs":        {"-f", "-n"},
		"--profile":     {"--like", "--provider", "--type", "--purge", "--yes", "--json", "claude"},
		"--connections": {"active", "total", "proxy", "--json"},
		"--peak":        {"set", "add", "rm", "tz", "clear", "pause", "resume", "--json"},
		"--doctor":      {"--offline", "--json"},
		"--update":      {"--check"},
		"--login":       {"--device", "claude"},
		"--serve":       {"-d", "-s"},
		"--send":        {"[--fork] [--force]", "--fork --force"},
		"--import":      {"[--fork] [--force]", "--fork --force"},
		"--export":      {"[--fork] [--force]", "--fork --force"},
	}
	for name, flags := range want {
		text := CommandHelp[name]
		for _, f := range flags {
			if !strings.Contains(text, f) {
				t.Errorf("%s: help does not mention %q", name, f)
			}
		}
	}
}

func TestCommandHelpTextFallback(t *testing.T) {
	c := Command{Name: "--nosuch", Args: "", Desc: "Restart the background server"}
	var buf bytes.Buffer
	printCommandHelp(&buf, c)
	want := "ccgp --nosuch\n\nRestart the background server\n"
	if buf.String() != want {
		t.Errorf("fallback help = %q, want %q", buf.String(), want)
	}
}

func TestWantsHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"command with --help", []string{"--login", "--help"}, true},
		{"command with -h", []string{"--login", "-h"}, true},
		{"command with later --help only", []string{"--login", "codex", "--help"}, false},
		{"command without help", []string{"--status"}, false},
		{"profile run with --help", []string{"codex", "--help"}, false},
		{"profile run with -h", []string{"codex", "-h"}, false},
		{"bare help", []string{"--help"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := ParseInvocation(tt.args)
			if err != nil {
				t.Fatalf("parseInvocation(%v): %v", tt.args, err)
			}
			if got := wantsHelp(inv); got != tt.want {
				t.Errorf("wantsHelp(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestHandleHelp covers the extracted decision run() relies on.
func TestHandleHelp(t *testing.T) {
	inv, err := ParseInvocation([]string{"--models", "-h"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if !handleHelp(inv, &buf) {
		t.Error("handleHelp = false, want true for a command help request")
	}
	if !strings.Contains(buf.String(), "ccgp --models") {
		t.Errorf("output = %q", buf.String())
	}
	inv, err = ParseInvocation([]string{"--models"})
	if err != nil {
		t.Fatal(err)
	}
	if handleHelp(inv, &buf) {
		t.Error("handleHelp = true for an invocation without a help argument")
	}
	inv, err = ParseInvocation([]string{"codex", "--help"})
	if err != nil {
		t.Fatal(err)
	}
	if handleHelp(inv, &buf) {
		t.Error("handleHelp = true for a profile run")
	}
}

// TestRunPrintsCommandHelp drives the real run() dispatch: `--login --help`
// must print the command help and return without ensureConfig or the login
// command. The config env points at a fresh path; if the interception were
// removed, run() would create that file instead of printing help.
func TestRunPrintsCommandHelp(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.ktav")
	t.Setenv(config.ConfigFileEnv, cfgPath)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := run([]string{"--login", "--help"})
	w.Close()
	os.Stdout = old
	if runErr != nil {
		t.Fatalf("run: %v", runErr)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "ccgp --login <provider> [--device]") {
		t.Errorf("run printed no per-command help, got:\n%s", out)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("config file was created; the command ran instead of printing help (stat err %v)", err)
	}
}

// --fork and --force work together; no text offers them as alternatives.
func TestNoHelpOffersForkAndForceAsAlternatives(t *testing.T) {
	for _, c := range Commands() {
		if strings.Contains(c.Args, "[--fork|--force]") {
			t.Errorf("%s: synopsis offers --fork and --force as alternatives: %q", c.Name, c.Args)
		}
	}
	for name, text := range CommandHelp {
		if strings.Contains(text, "[--fork|--force]") {
			t.Errorf("%s: help offers --fork and --force as alternatives", name)
		}
	}
}

func TestParseInvocation(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCmd     string   // "" for help (no command) or a profile run
		wantProfile string   // set for profile runs
		wantArgs    []string // remaining args
		wantErr     string   // "" means no error
	}{
		{"no args", nil, "", "", nil, ""},
		{"--help", []string{"--help"}, "", "", nil, ""},
		{"-h", []string{"-h"}, "", "", nil, ""},
		{"--serve with flags", []string{"--serve", "-d"}, "--serve", "", []string{"-d"}, ""},
		{"--start", []string{"--start"}, "--start", "", nil, ""},
		{"--stop", []string{"--stop"}, "--stop", "", nil, ""},
		{"--status", []string{"--status"}, "--status", "", nil, ""},
		{"--login with device", []string{"--login", "codex", "--device"}, "--login", "", []string{"codex", "--device"}, ""},
		{"--models apply", []string{"--models", "apply", "codex"}, "--models", "", []string{"apply", "codex"}, ""},
		{"--config reset", []string{"--config", "reset"}, "--config", "", []string{"reset"}, ""},
		{"--profile add", []string{"--profile", "add", "x", "--like", "codex"}, "--profile", "", []string{"add", "x", "--like", "codex"}, ""},
		{"--profile list", []string{"--profile", "list"}, "--profile", "", []string{"list"}, ""},
		{"--profile rm", []string{"--profile", "rm", "x", "--yes"}, "--profile", "", []string{"rm", "x", "--yes"}, ""},
		{"--send", []string{"--send", "codex", "claude", "3"}, "--send", "", []string{"codex", "claude", "3"}, ""},
		{"--import", []string{"--import", "zai"}, "--import", "", []string{"zai"}, ""},
		{"--export", []string{"--export", "zai", "2"}, "--export", "", []string{"zai", "2"}, ""},
		{"--version", []string{"--version"}, "--version", "", nil, ""},
		{"bare word is a profile", []string{"codex"}, "", "codex", nil, ""},
		{"profile args verbatim", []string{"codex", "-p", "x"}, "", "codex", []string{"-p", "x"}, ""},
		{"profile with dash flags", []string{"codex", "--status"}, "", "codex", []string{"--status"}, ""},
		{"former command is a profile", []string{"status"}, "", "status", nil, ""},
		{"login is a profile", []string{"login"}, "", "login", nil, ""},
		{"serve is a profile", []string{"serve", "-d"}, "", "serve", []string{"-d"}, ""},
		{"unknown command", []string{"--stauts"}, "", "", nil, "unknown command"},
		{"single dash is not a profile", []string{"-x"}, "", "", nil, "unknown command"},
		{"empty first argument", []string{""}, "", "", nil, "empty first argument"},
	}
	for _, tt := range tests {
		inv, err := ParseInvocation(tt.args)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", tt.name, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		wantName := tt.wantCmd
		if inv.Cmd == nil {
			if wantName != "" {
				t.Errorf("%s: got help/profile, want command %s", tt.name, wantName)
			}
		} else if inv.Cmd.Name != wantName {
			t.Errorf("%s: cmd = %s, want %s", tt.name, inv.Cmd.Name, wantName)
		}
		if inv.Profile != tt.wantProfile {
			t.Errorf("%s: profile = %q, want %q", tt.name, inv.Profile, tt.wantProfile)
		}
		if strings.Join(inv.Args, "\x00") != strings.Join(tt.wantArgs, "\x00") {
			t.Errorf("%s: args = %q, want %q", tt.name, inv.Args, tt.wantArgs)
		}
	}
}

func TestCommandTable(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Commands() {
		if seen[c.Name] {
			t.Errorf("duplicate command %s", c.Name)
		}
		seen[c.Name] = true
		if c.Desc == "" && !c.Hidden {
			t.Errorf("%s: empty description", c.Name)
		}
		if c.Run == nil {
			t.Errorf("%s: nil run", c.Name)
		}
	}
	for _, name := range []string{"--serve", "--start", "--stop", "--status", "--login", "--models", "--send", "--import", "--export", "--profile", "--config", "--version", "--help"} {
		if _, ok := FindCommand(name); !ok {
			t.Errorf("findCommand(%s) not found", name)
		}
		if _, ok := FindCommand(strings.TrimPrefix(name, "--")); ok {
			t.Errorf("bare word %q must not be a command", name)
		}
	}
}

func TestPrintHelpListsAllCommands(t *testing.T) {
	var buf bytes.Buffer
	printHelp(&buf)
	out := buf.String()
	for _, c := range Commands() {
		if c.Hidden {
			if strings.Contains(out, "ccgp "+c.Name) {
				t.Errorf("help mentions hidden command %s", c.Name)
			}
			continue
		}
		if !strings.Contains(out, "ccgp "+c.Name) {
			t.Errorf("help does not mention %s", c.Name)
		}
	}
	if !strings.Contains(out, "profile codex") {
		t.Error("help does not document the bare-word-is-a-profile rule")
	}
	if !strings.Contains(out, "ccgpy <profile>") {
		t.Error("help does not mention ccgpy")
	}
}

func TestAngelServeArgs(t *testing.T) {
	got := common.AngelServeArgs([]string{"-d"})
	want := []string{"--serve", "-s", "-d"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("angelServeArgs = %q, want %q", got, want)
	}
}

// Commands that only read state must not create the default config.
func TestEnsureConfigPolicy(t *testing.T) {
	noConfig := map[string]bool{
		"--stop": true, "--status": true, "--config": true, "--logs": true, "--version": true, "--help": true,
		"--completion": true, "--complete": true, "--doctor": true, "--update": true, "--speed": true,
	}
	for _, c := range Commands() {
		if c.EnsureConfig == noConfig[c.Name] {
			t.Errorf("%s: ensureConfig = %v", c.Name, c.EnsureConfig)
		}
	}
}

var testNames = CompletionNames{
	Profiles:  func() []string { return []string{"codex", "codex2", "zai"} },
	Providers: func() []string { return []string{"codex", "zai"} },
}
