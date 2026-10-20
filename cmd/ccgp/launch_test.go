package main

import (
	"slices"
	"testing"

	"github.com/claude-code-proxy/proxy/internal/config"
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
	got := compactEnv([]string{"PATH=/bin"}, pf)
	want := []string{"PATH=/bin", "CLAUDE_CODE_AUTO_COMPACT_WINDOW=800000", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=70"}
	if !slices.Equal(got, want) {
		t.Errorf("compactEnv = %q, want %q", got, want)
	}

	// User-set values win; no window means no change.
	got = compactEnv([]string{"claude_code_auto_compact_window=123", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=50"}, pf)
	if len(got) != 2 {
		t.Errorf("overrode user env: %q", got)
	}
	if got = compactEnv([]string{"PATH=/bin"}, &config.Profile{}); len(got) != 1 {
		t.Errorf("no window: %q", got)
	}
	if got = compactEnv([]string{"PATH=/bin"}, nil); len(got) != 1 {
		t.Errorf("nil profile: %q", got)
	}
	// Percent alone (window known) only sets the percent when asked.
	got = compactEnv(nil, &config.Profile{ContextWindow: 100})
	if !slices.Equal(got, []string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW=100"}) {
		t.Errorf("window only: %q", got)
	}
}
