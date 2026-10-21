package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

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
	got := claudeEnv(in, "http://127.0.0.1:60093/call/tok", "/iso")
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
	got = claudeEnv([]string{"claude_code_auto_mode_server=1", "ENABLE_CLAUDEAI_MCP_SERVERS=true"}, "http://x", "/iso")
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
	got := compactEnv([]string{"PATH=/bin"}, pf, "sonnet")
	want := []string{"PATH=/bin", "CLAUDE_CODE_AUTO_COMPACT_WINDOW=800000", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=70"}
	if !slices.Equal(got, want) {
		t.Errorf("compactEnv = %q, want %q", got, want)
	}

	// User-set values win; no window means no change.
	got = compactEnv([]string{"claude_code_auto_compact_window=123", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=50"}, pf, "sonnet")
	if len(got) != 2 {
		t.Errorf("overrode user env: %q", got)
	}
	if got = compactEnv([]string{"PATH=/bin"}, &config.Profile{}, "sonnet"); len(got) != 1 {
		t.Errorf("no window: %q", got)
	}
	if got = compactEnv([]string{"PATH=/bin"}, nil, "sonnet"); len(got) != 1 {
		t.Errorf("nil profile: %q", got)
	}
	// Percent alone (window known) only sets the percent when asked.
	got = compactEnv(nil, &config.Profile{ContextWindow: 100}, "sonnet")
	if !slices.Equal(got, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=100"}) {
		t.Errorf("window only: %q", got)
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
		if got := compactEnv(nil, pf, model); !slices.Equal(got, []string{want}) {
			t.Errorf("main model %q: %q, want %q", model, got, want)
		}
	}

	// A matching rule without a window falls back to the smallest known one.
	partial := &config.Profile{Models: []config.ModelRule{
		{Match: "*sonnet*", Model: "a"},
		{Match: "*haiku*", Model: "b", ContextWindow: 272000},
	}}
	if got := compactEnv(nil, partial, "sonnet"); !slices.Equal(got, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=272000"}) {
		t.Errorf("fallback: %q", got)
	}
}

func TestMainModel(t *testing.T) {
	dir := t.TempDir()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if got := mainModel(nil, env(nil), dir); got != "sonnet" {
		t.Errorf("default = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mainModel(nil, env(nil), dir); got != "opus" {
		t.Errorf("settings model = %q", got)
	}
	if got := mainModel(nil, env(map[string]string{"ANTHROPIC_MODEL": "haiku"}), dir); got != "haiku" {
		t.Errorf("env model = %q", got)
	}
	if got := mainModel([]string{"-p", "hi", "--model", "fable"}, env(map[string]string{"ANTHROPIC_MODEL": "haiku"}), dir); got != "fable" {
		t.Errorf("--model = %q", got)
	}
	if got := mainModel([]string{"--model=claude-sonnet-5-5"}, env(nil), dir); got != "claude-sonnet-5-5" {
		t.Errorf("--model= = %q", got)
	}
	// A trailing --model without a value is ignored.
	if got := mainModel([]string{"--model"}, env(nil), dir); got != "opus" {
		t.Errorf("dangling --model = %q", got)
	}
}
