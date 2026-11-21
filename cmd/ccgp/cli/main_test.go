package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/core"
	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/catalog"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	peakcfg "github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
	"github.com/PHPCraftdream/cc-go-proxy/internal/preset"
	"golang.org/x/crypto/x509roots/fallback/bundle"
)

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

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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

// --home puts the config in that directory and nothing in the default one; the
// variable is set so the background server started later finds the same home.
func TestHomeFlagMovesTheConfig(t *testing.T) {
	defaults := isolate(t)
	dir := filepath.Join(t.TempDir(), "other")
	if err := run([]string{"--home", dir, "--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.ktav"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "listen.port: 60093") || !strings.Contains(string(b), "listen.port: ") {
		t.Errorf("a new home must get a port of its own:\n%.300s", b)
	}
	if home.Root() != dir {
		t.Errorf("CCGP_HOME = %q, want %q", home.Root(), dir)
	}
	entries, _ := os.ReadDir(defaults)
	if len(entries) != 0 {
		t.Errorf("the default location was touched: %v", entries)
	}
}

func TestHomeFlagWithEquals(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "eq")
	if err := run([]string{"--home=" + dir, "--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "config.ktav")) {
		t.Error("no config in the --home= directory")
	}
}

// --local makes ./.ccgp, and a .gitignore in it keeps the secrets out of git.
func TestLocalFlagMakesTheDirectoryHere(t *testing.T) {
	isolate(t)
	wd := t.TempDir()
	t.Chdir(wd)
	if err := run([]string{"--local", "--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config.ktav", ".gitignore"} {
		if !exists(filepath.Join(wd, ".ccgp", f)) {
			t.Errorf(".ccgp/%s is missing", f)
		}
	}
}

// A project that has a .ccgp directory runs on it with no flag.
func TestProjectDirectoryIsUsedWithoutAFlag(t *testing.T) {
	defaults := isolate(t)
	wd := t.TempDir()
	t.Chdir(wd)
	if err := os.Mkdir(filepath.Join(wd, ".ccgp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(wd, ".ccgp", "config.ktav")) {
		t.Error("the config was not written into ./.ccgp")
	}
	if entries, _ := os.ReadDir(defaults); len(entries) != 0 {
		t.Errorf("the default location was touched: %v", entries)
	}
}

// Without the directory and without a flag nothing changes: the default layout.
func TestNoHomeKeepsTheDefaultLayout(t *testing.T) {
	defaults := isolate(t)
	t.Chdir(t.TempDir())
	if err := run([]string{"--config", "reset"}); err != nil {
		t.Fatal(err)
	}
	if home.Root() != "" {
		t.Errorf("CCGP_HOME = %q", home.Root())
	}
	cfg, _ := home.Dir()
	if !strings.HasPrefix(cfg, defaults) || !exists(filepath.Join(cfg, "config.ktav")) {
		t.Errorf("config dir %q (default location %q)", cfg, defaults)
	}
}

// Help, the version and completion never make the directory.
func TestInfoCommandsDoNotCreateTheHome(t *testing.T) {
	isolate(t)
	wd := t.TempDir()
	t.Chdir(wd)
	for _, args := range [][]string{
		{"--local", "--version"},
		{"--local", "--help"},
		{"--local"},
		{"--local", "--status", "--help"},
		{"--local", "--login", "-h"},
		{"--local", "--complete", "ccgp", ""},
	} {
		if err := run(args); err != nil {
			t.Errorf("%q: %v", args, err)
		}
		if exists(filepath.Join(wd, ".ccgp")) {
			t.Fatalf("%q made ./.ccgp", args)
		}
	}
}

func TestHomeFlagErrors(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"--home"},
		{"--home", "--status"},
		{"--local", "--home", "x", "--version"},
		{"--local", "--local", "--version"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run(%q) must fail", args)
		}
	}
}

func TestStatusShowsTheHome(t *testing.T) {
	var b strings.Builder
	if err := renderStatusText(&b, statusData{Home: "/p/.ccgp", LogFile: "/p/.ccgp/logs/ccgp.log"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "home:    /p/.ccgp\n") {
		t.Errorf("status:\n%s", b.String())
	}
	b.Reset()
	_ = renderStatusText(&b, statusData{})
	if strings.Contains(b.String(), "home:") {
		t.Errorf("the default layout prints no home line:\n%s", b.String())
	}
}

// Config, secrets, Claude Code dirs and the log all follow the home.
func TestEveryPathFollowsTheHome(t *testing.T) {
	isolate(t)
	check := func(name, got string, want ...string) {
		t.Helper()
		if w := filepath.Join(want...); got != w {
			t.Errorf("%s = %q, want %q", name, got, w)
		}
	}
	base := func() (cfg, cr, claude, log string) {
		var err error
		if cfg, err = config.DefaultConfigPath(); err != nil {
			t.Fatal(err)
		}
		if cr, err = creds.DefaultDir(); err != nil {
			t.Fatal(err)
		}
		if claude, err = common.ClaudeHomeBase(); err != nil {
			t.Fatal(err)
		}
		if log, err = angel.LogPath(); err != nil {
			t.Fatal(err)
		}
		return
	}

	cfg, cr, cl, lg := base()
	confDir, _ := os.UserConfigDir()
	cacheDir, _ := os.UserCacheDir()
	app := filepath.Join(confDir, "cc-go-proxy")
	check("default config", cfg, app, "config.ktav")
	check("default creds", cr, app, "creds")
	check("default claude", cl, app, "claude")
	check("default log", lg, cacheDir, "cc-go-proxy", "logs", "ccgp.log")

	dir := t.TempDir()
	t.Setenv(home.EnvVar, dir)
	cfg, cr, cl, lg = base()
	check("config", cfg, dir, "config.ktav")
	check("creds", cr, dir, "creds")
	check("claude", cl, dir, "claude")
	check("log", lg, dir, "logs", "ccgp.log")
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := common.WriteJSON(&buf, map[string]int{"b": 2, "a": 1}); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": 1,\n  \"b\": 2\n}\n"
	if buf.String() != want {
		t.Errorf("writeJSON = %q, want %q", buf.String(), want)
	}
}

func TestRenderStatusText(t *testing.T) {
	up := statusServer{Running: true, Version: "ccgp v9", PID: 42, URL: "http://127.0.0.1:1"}
	tests := []struct {
		name string
		data statusData
		want string
	}{
		{
			name: "down, file mode",
			data: statusData{
				Server:    statusServer{URL: "http://127.0.0.1:1"},
				LogFile:   "x.log",
				Config:    statusConfig{Mode: "file", Path: "x.ktav"},
				CredsDir:  "creds",
				Providers: []statusProvider{{Name: "p1", Type: "codex", Login: "not logged in"}},
				Profiles:  []statusProfile{{Name: "alpha", Provider: "p1"}},
			},
			want: "server:  not running at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  x.ktav\n" +
				"creds:   creds\n" +
				"  provider p1           codex   not logged in\n" +
				"  profile  alpha        -> p1\n",
		},
		{
			name: "up, env mode",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "env"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  env-only mode (no config.ktav)\n",
		},
		{
			name: "error mode",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "error", Error: "parse: line 3"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  error: parse: line 3\n",
		},
		{
			name: "missing mode",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "missing", Path: "x.ktav"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  x.ktav (not created yet; created on first run)\n",
		},
		{
			name: "file mode without creds dir",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "file", Path: "x.ktav"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  x.ktav\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderStatusText(&buf, tt.data); err != nil {
				t.Fatal(err)
			}
			if buf.String() != tt.want {
				t.Errorf("renderStatusText =\n%q\nwant\n%q", buf.String(), tt.want)
			}
		})
	}
}

// The fake secret must never reach the JSON payload; status only carries
// non-secret fields (paths, names, login descriptions).
const testSecretToken = "tok-SUPERSECRET-123"

func TestStatusJSON(t *testing.T) {
	t.Setenv("CCGP_TEST_SECRET", "sk-FAKEKEY-not-real-000")
	cfg := &config.Config{
		ConfigFile: filepath.Join(t.TempDir(), "x.ktav"),
		Providers: map[string]*config.Provider{
			"p1": {Name: "p1", Type: config.ProviderTypeAnthropicCompat, BaseURL: "http://127.0.0.1:9/" + testSecretToken, APIKeyEnv: "CCGP_TEST_SECRET"},
		},
		Profiles: map[string]*config.Profile{
			"alpha": {Name: "alpha", Provider: "p1"},
		},
	}
	providers := []statusProvider{{Name: "p1", Type: config.ProviderTypeAnthropicCompat, Login: "key from $CCGP_TEST_SECRET"}}
	profiles := []statusProfile{{Name: "alpha", Provider: "p1"}}
	st := buildStatus("http://127.0.0.1:1", "x.log", &angel.Health{Version: "ccgp v9", PID: 42}, nil, cfg, nil, filepath.Join(t.TempDir(), "creds"), nil, providers, profiles)

	var buf bytes.Buffer
	if err := common.WriteJSON(&buf, st); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, testSecretToken) {
		t.Error("JSON output contains the planted base URL token")
	}
	if strings.Contains(out, "sk-FAKEKEY-not-real-000") {
		t.Error("JSON output contains the API key env value")
	}

	var got statusData
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Server.Running || got.Server.PID != 42 || got.Server.URL != "http://127.0.0.1:1" {
		t.Errorf("server = %+v", got.Server)
	}
	if got.Config.Mode != "file" || got.Config.Path != cfg.ConfigFile {
		t.Errorf("config = %+v", got.Config)
	}
	if len(got.Providers) != 1 || got.Providers[0].Name != "p1" {
		t.Errorf("providers = %+v", got.Providers)
	}
	if len(got.Profiles) != 1 || got.Profiles[0].Provider != "p1" {
		t.Errorf("profiles = %+v", got.Profiles)
	}
}

func TestBuildProfileRowsAndText(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]*config.Provider{
			"p1": {Name: "p1", Type: config.ProviderTypeCodex, BaseURL: "http://127.0.0.1:9/" + testSecretToken},
			"p2": {Name: "p2", Type: config.ProviderTypeAnthropicCompat, APIKeyEnv: "CCGP_TEST_SECRET"},
		},
		Profiles: map[string]*config.Profile{
			"beta":  {Name: "beta", Provider: "p2"},
			"alpha": {Name: "alpha", Provider: "p1", Models: []config.ModelRule{{Match: "sonnet*", Model: "gpt-x"}}},
		},
	}
	store := creds.Store{Dir: filepath.Join(t.TempDir(), "creds")}
	t.Setenv("CCGP_TEST_SECRET", "sk-FAKEKEY-not-real-000")
	claudeBase := t.TempDir()
	if err := os.Mkdir(filepath.Join(claudeBase, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}

	rows := core.BuildProfileRows(cfg, store, claudeBase)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Name != "alpha" || rows[1].Name != "beta" {
		t.Errorf("row order = %s, %s", rows[0].Name, rows[1].Name)
	}
	if rows[0].MainModel != "gpt-x" || rows[1].MainModel != "-" {
		t.Errorf("main models = %q, %q", rows[0].MainModel, rows[1].MainModel)
	}
	if !rows[0].ClaudeDir || rows[1].ClaudeDir {
		t.Errorf("claudeDir = %v, %v", rows[0].ClaudeDir, rows[1].ClaudeDir)
	}
	if rows[0].Login != "not logged in (ccgp --login p1)" {
		t.Errorf("codex login = %q", rows[0].Login)
	}
	if rows[1].Login != "key from $CCGP_TEST_SECRET" {
		t.Errorf("compat login = %q", rows[1].Login)
	}

	var buf bytes.Buffer
	if err := core.ProfileList(&buf, rows); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, testSecretToken) || strings.Contains(out, "sk-FAKEKEY-not-real-000") {
		t.Error("text output contains a planted secret")
	}
	var want bytes.Buffer
	fmt.Fprintf(&want, "%-16s %-16s %-17s %-38s %-16s %s\n", "PROFILE", "PROVIDER", "TYPE", "LOGIN", "MAIN MODEL", "CLAUDE DIR")
	for _, r := range rows {
		dir := "no"
		if r.ClaudeDir {
			dir = "yes"
		}
		fmt.Fprintf(&want, "%-16s %-16s %-17s %-38s %-16s %s\n", r.Name, r.Provider, r.Type, r.Login, r.MainModel, dir)
	}
	if out != want.String() {
		t.Errorf("profileList output mismatch:\n%q\nwant\n%q", out, want.String())
	}
}

func TestProfileListJSONNoSecrets(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]*config.Provider{
			"p1": {Name: "p1", Type: config.ProviderTypeAnthropicCompat, BaseURL: "http://127.0.0.1:9/" + testSecretToken},
		},
		Profiles: map[string]*config.Profile{
			"alpha": {Name: "alpha", Provider: "p1", DefaultModel: "m"},
		},
	}
	rows := core.BuildProfileRows(cfg, creds.Store{Dir: t.TempDir()}, t.TempDir())
	var buf bytes.Buffer
	if err := common.WriteJSON(&buf, core.ProfileListData{Profiles: rows}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, testSecretToken) {
		t.Error("JSON output contains the planted base URL token")
	}
	var got core.ProfileListData
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Profiles) != 1 || got.Profiles[0].Name != "alpha" || got.Profiles[0].Provider != "p1" {
		t.Errorf("profiles = %+v", got.Profiles)
	}
}

func TestModelsJSON(t *testing.T) {
	sel := catalog.Selection{
		Fable: "f", FableWindow: 1000,
		Opus: "o", OpusWindow: 2000,
		Sonnet: "s", SonnetWindow: 3000,
		Haiku: "h", HaikuWindow: 4000,
		Notes: []string{"picked by generation"},
	}
	mp := core.CatalogModelsProfile("alpha", "p1", sel)
	if len(mp.Tiers) != 5 || mp.Tiers[4].Tier != "other" || mp.Tiers[4].Model != "s" || mp.Tiers[4].ContextWindow != 3000 {
		t.Errorf("catalog tiers = %+v", mp.Tiers)
	}
	if len(mp.Notes) != 1 || mp.Notes[0] != "picked by generation" {
		t.Errorf("notes = %+v", mp.Notes)
	}
	pp := core.PresetModelsProfile("beta", "p2", preset.Preset{
		Name: "zai",
		Rules: []config.ModelRule{
			{Match: "*fable*", Model: "f-model", ContextWindow: 1000},
			{Match: "*", Model: "s-model", ContextWindow: 3000},
		},
	})
	if pp.Preset != "zai" || len(pp.Tiers) != 2 || pp.Tiers[0].Tier != "*fable*" {
		t.Errorf("preset profile = %+v", pp)
	}

	var buf bytes.Buffer
	if err := common.WriteJSON(&buf, core.ModelsData{Profiles: []core.ModelsProfile{mp, pp}}); err != nil {
		t.Fatal(err)
	}
	var got core.ModelsData
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(got.Profiles))
	}
	if got.Profiles[0].Tiers[2].Tier != "sonnet" || got.Profiles[0].Tiers[2].Model != "s" {
		t.Errorf("sonnet tier = %+v", got.Profiles[0].Tiers[2])
	}
	if got.Profiles[0].Notes[0] != "picked by generation" {
		t.Errorf("notes = %+v", got.Profiles[0].Notes)
	}
	if got.Profiles[1].Preset != "zai" || got.Profiles[1].Tiers[1].Model != "s-model" {
		t.Errorf("preset profile = %+v", got.Profiles[1])
	}

	// --models apply --json is rejected before any catalog fetch.
	err := core.Models(&config.Config{
		ConfigFile: "x.ktav",
		Providers:  map[string]*config.Provider{},
		Profiles:   map[string]*config.Profile{},
	}, []string{"apply", "--json"})
	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("models(apply, --json) error = %v, want a --json usage error", err)
	}
}

func TestStatusUnknownFlag(t *testing.T) {
	if err := Status([]string{"--bogus"}); err == nil {
		t.Error("status(--bogus) = nil error, want unknown-flag error")
	}
}

// testConfig plus a provider and a profile of the real Claude Code.
func claudeTestConfig() string {
	text := strings.Replace(testConfig, "providers: [\n", "providers: [\n    {\n        name: work\n        type: claude\n    }\n", 1)
	return strings.Replace(text, "profiles: [\n", "profiles: [\n    {\n        name: work\n        provider: work\n    }\n", 1)
}

func TestProfileSet(t *testing.T) {
	path, _ := writeTestConfig(t, claudeTestConfig())
	var buf bytes.Buffer
	run := func(unset bool, args ...string) error {
		buf.Reset()
		return core.ProfileSet(&buf, loadTestConfig(t), args, unset)
	}

	if err := run(false, "zai2", "default_model", "glm-5.3"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "config.ktav updated") || loadTestConfig(t).Profiles["zai2"].DefaultModel != "glm-5.3" {
		t.Errorf("default_model: %q, %+v", buf.String(), loadTestConfig(t).Profiles["zai2"])
	}
	if err := run(false, "zai2", "default_model", "glm-5.3"); err != nil || !strings.Contains(buf.String(), "already had this value") {
		t.Errorf("the same value again: %v %q", err, buf.String())
	}
	if err := run(false, "zai2", "context_window", "500000"); err != nil {
		t.Fatal(err)
	}
	if err := run(false, "zai2", "compact_percent", "70"); err != nil {
		t.Fatal(err)
	}
	if pf := loadTestConfig(t).Profiles["zai2"]; pf.ContextWindow != 500000 || pf.CompactPercent != 70 || pf.DefaultModel != "glm-5.3" {
		t.Errorf("profile zai2 = %+v", pf)
	}
	if err := run(true, "zai2", "compact_percent"); err != nil {
		t.Fatal(err)
	}
	if pf := loadTestConfig(t).Profiles["zai2"]; pf.CompactPercent != 0 || pf.ContextWindow != 500000 {
		t.Errorf("after unset: %+v", pf)
	}
	if err := run(false, "zai2", "provider", "codex"); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Profiles["zai2"].Provider; got != "codex" {
		t.Errorf("provider after the move = %q", got)
	}

	// Refused: nothing is written.
	before, _ := os.ReadFile(path)
	for name, call := range map[string]func() error{
		"unknown profile":     func() error { return run(false, "nope", "default_model", "m") },
		"unknown key":         func() error { return run(false, "zai", "models", "m") },
		"proxy has its own":   func() error { return run(false, "zai", "proxy", "none") },
		"not a number":        func() error { return run(false, "zai", "context_window", "big") },
		"negative":            func() error { return run(false, "zai", "compact_percent", "-5") },
		"over 100":            func() error { return run(false, "zai", "compact_percent", "150") },
		"unknown provider":    func() error { return run(false, "zai", "provider", "nope") },
		"a claude provider":   func() error { return run(false, "zai", "provider", "work") },
		"unset the provider":  func() error { return run(true, "zai", "provider") },
		"a claude profile":    func() error { return run(false, "work", "default_model", "m") },
		"too few words":       func() error { return run(false, "zai", "default_model") },
		"too many words":      func() error { return run(false, "zai", "default_model", "a", "b") },
		"unset with a value":  func() error { return run(true, "zai", "default_model", "a") },
		"unset without a key": func() error { return run(true, "zai") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("config changed by refused edits:\n%s", after)
	}
}

func TestProfileSetThroughTheDispatcher(t *testing.T) {
	writeTestConfig(t, testConfig)
	t.Setenv("CCGP_HOME", t.TempDir())
	if err := core.ProfileCmd(loadTestConfig(t), []string{"set", "zai", "default_model", "m"}); err != nil {
		t.Fatal(err)
	}
	if err := core.ProfileCmd(loadTestConfig(t), []string{"unset", "zai", "default_model"}); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Profiles["zai"].DefaultModel; got != "" {
		t.Errorf("default_model = %q after unset", got)
	}
}

func providerRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("CCGP_HOME", t.TempDir())
	var buf bytes.Buffer
	store := creds.Store{Dir: t.TempDir()}
	err := core.Provider(&buf, loadTestConfig(t), store, peakcfg.NewPauses(store.Dir), time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), args)
	return buf.String(), err
}

func TestProviderListAndShow(t *testing.T) {
	text := strings.Replace(testConfig, "        api_key_env: ZAI_KEY_TEST\n",
		"        api_key_env: ZAI_KEY_TEST\n        proxy: http://corp:3128\n        peak: [\n            {\n                days: mon-fri\n                from: 14:00\n                to: 18:00\n            }\n        ]\n", 1)
	writeTestConfig(t, text)

	out, err := providerRun(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PROVIDER", "codex", "codex ", "zai", "anthropic_compat", "yes", "zai, zai2"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}

	out, err = providerRun(t, "zai")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type         anthropic_compat", "base_url     https://api.z.ai/api/anthropic", "api_key_env  ZAI_KEY_TEST",
		"proxy        http://corp:3128 (its own)", "effort_map   max=max medium=high", "peak hours   mon-fri 14:00-18:00", "profiles     zai, zai2"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lacks %q:\n%s", want, out)
		}
	}

	out, err = providerRun(t, "codex")
	if err != nil || !strings.Contains(out, "proxy        global (") || !strings.Contains(out, "peak hours   none") {
		t.Errorf("codex: %v\n%s", err, out)
	}

	out, err = providerRun(t, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got core.ProviderListData
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Providers) != 2 {
		t.Fatalf("json %s: %v", out, err)
	}
	if z := got.Providers[1]; z.Name != "zai" || z.Proxy != "http://corp:3128" || !z.ProxyOwn || z.APIKeyEnv != "ZAI_KEY_TEST" || len(z.Profiles) != 2 {
		t.Errorf("zai = %+v", z)
	}
	out, err = providerRun(t, "zai", "--json")
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || len(got.Providers) != 1 || got.Providers[0].Name != "zai" {
		t.Errorf("one provider as json: %v\n%s", err, out)
	}

	for _, args := range [][]string{{"nope"}, {"zai", "codex"}, {"--bogus"}} {
		if _, err := providerRun(t, args...); err == nil {
			t.Errorf("%q: accepted", args)
		}
	}
}

func TestProviderSet(t *testing.T) {
	path, _ := writeTestConfig(t, claudeTestConfig())
	out, err := providerRun(t, "set", "zai", "base_url", "https://example.internal/api")
	if err != nil || !strings.Contains(out, "config.ktav updated") {
		t.Fatalf("set base_url: %v %q", err, out)
	}
	if got := loadTestConfig(t).Providers["zai"].BaseURL; got != "https://example.internal/api" {
		t.Errorf("base_url = %q", got)
	}
	if out, err = providerRun(t, "set", "zai", "base_url", "https://example.internal/api"); err != nil || !strings.Contains(out, "already had this value") {
		t.Errorf("the same value again: %v %q", err, out)
	}
	if _, err = providerRun(t, "unset", "zai", "api_key_env"); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Providers["zai"].APIKeyEnv; got != "" {
		t.Errorf("api_key_env = %q after unset", got)
	}
	if _, err = providerRun(t, "set", "zai", "api_key_env", "MY_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Providers["zai"].APIKeyEnv; got != "MY_KEY" {
		t.Errorf("api_key_env = %q", got)
	}

	before, _ := os.ReadFile(path)
	for name, args := range map[string][]string{
		"unknown provider":     {"set", "nope", "base_url", "x"},
		"unknown key":          {"set", "zai", "type", "codex"},
		"proxy has its own":    {"set", "zai", "proxy", "none"},
		"codex has no key env": {"set", "codex", "api_key_env", "K"},
		"a claude provider":    {"set", "work", "base_url", "x"},
		"base_url is required": {"unset", "zai", "base_url"},
		"too few words":        {"set", "zai", "base_url"},
		"unset with a value":   {"unset", "zai", "base_url", "x"},
		"set without anything": {"set"},
		"unset without a key":  {"unset", "zai"},
		"two lines in a value": {"set", "zai", "base_url", "a\nb"},
	} {
		if _, err := providerRun(t, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("config changed by refused edits:\n%s", after)
	}
}

func TestProviderNeedsAConfigFile(t *testing.T) {
	cfg := &config.Config{}
	if err := core.Provider(&bytes.Buffer{}, cfg, creds.Store{}, peakcfg.NewPauses(""), time.Now(), nil); err == nil {
		t.Error("env-only mode accepted")
	}
}

func TestRunUnknownProfileNeedsConfig(t *testing.T) {
	t.Setenv("CCGP_CONFIG", t.TempDir()+"/absent.ktav")
	if err := run([]string{"nope"}); err == nil {
		t.Fatal("run(unknown) succeeded")
	}
}

// A system without root certificates (busybox, scratch, other minimal
// containers) must still reach HTTPS: ccgp carries a fallback bundle, which
// crypto/x509 uses only when the system has none.
func TestFallbackRootsAreCarried(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `_ "golang.org/x/crypto/x509roots/fallback"`) {
		t.Fatal("main.go no longer imports the fallback root certificates")
	}
	n := 0
	for range bundle.Roots() {
		n++
	}
	if n < 100 {
		t.Errorf("the fallback bundle holds %d roots, want the full Mozilla set", n)
	}
}

func loadTemplate(t *testing.T) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, config.DefaultTemplate(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCGP_CONFIG", p)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The generated z.ai block in the default config must be exactly the preset:
// Fable -> glm-5.3, everything else -> glm-5.3-flash, 1M windows.
func TestTemplateZAIMatchesPreset(t *testing.T) {
	cfg := loadTemplate(t)
	pf := cfg.Profiles["zai"]
	if pf == nil || pf.Provider != "zai" {
		t.Fatalf("zai profile = %+v", pf)
	}
	if !reflect.DeepEqual(pf.Models, preset.ZAI.Rules) {
		t.Errorf("template rules\n%+v\nare not the preset\n%+v", pf.Models, preset.ZAI.Rules)
	}
	if got := pf.ContextWindowFor("sonnet"); got != 1_000_000 {
		t.Errorf("window for sonnet = %d", got)
	}
	pr := cfg.Providers["zai"]
	if pr.Type != config.ProviderTypeAnthropicCompat || !reflect.DeepEqual(pr.EffortMap, preset.ZAI.EffortMap) {
		t.Errorf("zai provider = %+v, want the preset's effort map %v", pr, preset.ZAI.EffortMap)
	}
	if r, _ := cfg.ForProfile("zai", "key"); r == nil || r.ProviderType != config.ProviderTypeAnthropicCompat || r.OpenAIAPIKey != "key" {
		t.Errorf("ForProfile(zai) = %+v", r)
	}
	if m, ok := (&config.Config{ModelRules: pf.Models}).ResolveModel("claude-opus-5-5"); !ok || m != "glm-5.3-flash" {
		t.Errorf("opus -> %q", m)
	}
	if m, _ := (&config.Config{ModelRules: pf.Models}).ResolveModel("claude-fable-5-1"); m != "glm-5.3" {
		t.Errorf("fable -> %q", m)
	}
}

// Generated mappings exist for codex profiles and for anthropic_compat
// providers that have a preset; other profiles are skipped.
func TestManagedProfiles(t *testing.T) {
	cfg := loadTemplate(t)
	got, err := core.ManagedProfiles(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if !reflect.DeepEqual(names, []string{"codex", "zai"}) {
		t.Errorf("managed profiles = %v, want [codex zai] (codex_api has no generator)", names)
	}
	if one, err := core.ManagedProfiles(cfg, []string{"zai"}); err != nil || len(one) != 1 || one[0].Name != "zai" {
		t.Errorf("only zai = %v, %v", one, err)
	}
	if _, err := core.ManagedProfiles(cfg, []string{"codex_api"}); err == nil {
		t.Error("codex_api accepted as a generated profile")
	}

	// An anthropic_compat provider without a preset is not generated.
	cfg.Providers["zai"].BaseURL = "https://example.com/anthropic"
	got, _ = core.ManagedProfiles(cfg, nil)
	for _, p := range got {
		if p.Name == "zai" {
			t.Error("provider without a preset was offered")
		}
	}
}

func TestConfigCmdUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"reset", "extra"}} {
		if err := configCmd(args); err == nil {
			t.Errorf("configCmd(%v) succeeded", args)
		}
	}
}
