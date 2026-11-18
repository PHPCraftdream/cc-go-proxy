package cli

import (
	"bytes"
	"encoding/json"
	"errors"
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
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/catalog"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	peakcfg "github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/preset"
	"golang.org/x/crypto/x509roots/fallback/bundle"
)

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

func TestRunUnknownProfileNeedsConfig(t *testing.T) {
	t.Setenv("CCGP_CONFIG", t.TempDir()+"/absent.ktav")
	if err := run([]string{"nope"}); err == nil {
		t.Fatal("run(unknown) succeeded")
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
