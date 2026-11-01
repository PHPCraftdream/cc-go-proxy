package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

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
	base, err := claudeHomeBase()
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

	err := launch("work", []string{"--foo", "bar"})
	var ee exitError
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
	for _, bad := range []string{"ANTHROPIC_", "OAUTH_TOKEN", "sk-official", "AUTO_COMPACT", "ENABLE_CLAUDEAI_MCP_SERVERS", "/call/"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q reached a Claude profile:\n%s", bad, got)
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
	if err := launch("work", nil); !errors.As(err, &ee) {
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
	err := launch("work", nil)
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
	if got := loginState(store, pr); got != "sign in inside Claude Code (/login)" {
		t.Errorf("new profile: %q", got)
	}
	if loginIsWarn(loginState(store, pr)) {
		t.Error("a Claude profile that is not signed in yet is not a doctor warning")
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loginState(store, pr); got != "signed in inside Claude Code" {
		t.Errorf("signed in: %q", got)
	}
}

func TestStatusShowsClaudeProfile(t *testing.T) {
	ownSetup(t)
	var runErr error
	out := captureStdout(t, func() { runErr = status([]string{"--json"}) })
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
	inv, err := parseInvocation([]string{"--profile", "-h"})
	if err != nil || inv.cmd == nil {
		t.Fatalf("parseInvocation: %v", err)
	}
	if text := commandHelpText(*inv.cmd); !strings.Contains(text, "--type claude") {
		t.Errorf("--profile help does not mention --type claude:\n%s", text)
	}
	got := completeWords([]string{"ccgp", "--profile", "add", "x", "--type", ""}, testNames)
	for _, want := range []string{"codex", "codex_api", "anthropic_compat", "claude"} {
		if !contains(got, want) {
			t.Errorf("--type does not offer %q: %q", want, got)
		}
	}
	if got := completeWords([]string{"ccgp", "--profile", "add", "x", "--type", "cl"}, testNames); len(got) != 1 || got[0] != "claude" {
		t.Errorf("--type cl = %q", got)
	}
}
