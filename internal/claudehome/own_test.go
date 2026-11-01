package claudehome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareOwnMirrorsSettingsOnly(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")

	write(t, filepath.Join(src, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"official"}}`)
	write(t, filepath.Join(src, ".claude.json"), `{"oauthAccount":{"emailAddress":"me@example.com"},"userID":"u","projects":{"/p":{}}}`)
	write(t, filepath.Join(src, "CLAUDE.md"), "rules")
	write(t, filepath.Join(src, "settings.json"), `{"apiKeyHelper":"x","forceLoginOrgUUID":"org","model":"opus","env":{"ANTHROPIC_API_KEY":"k","KEEP":"v"}}`)
	write(t, filepath.Join(src, "history.jsonl"), "secret history")
	write(t, filepath.Join(src, "projects", "p", "s1.jsonl"), "{}\n")
	write(t, filepath.Join(src, "skills", "s1", "SKILL.md"), "skill")

	// A login made inside the profile is its own and survives every launch.
	write(t, filepath.Join(dst, ".credentials.json"), "own login")
	write(t, filepath.Join(dst, ".claude.json"), `{"numStartups":3}`)

	for i := 0; i < 2; i++ {
		if err := PrepareOwn(src, dst); err != nil {
			t.Fatal(err)
		}
	}
	if read(t, filepath.Join(dst, ".credentials.json")) != "own login" {
		t.Error("the profile's own login was removed")
	}
	if read(t, filepath.Join(dst, ".claude.json")) != `{"numStartups":3}` {
		t.Error("the profile's own state was touched")
	}
	if read(t, filepath.Join(dst, "CLAUDE.md")) != "rules" || read(t, filepath.Join(dst, "skills", "s1", "SKILL.md")) != "skill" {
		t.Error("shared instructions and skills not mirrored")
	}
	s := read(t, filepath.Join(dst, "settings.json"))
	for _, bad := range []string{"apiKeyHelper", "forceLoginOrgUUID", "ANTHROPIC_API_KEY"} {
		if strings.Contains(s, bad) {
			t.Errorf("settings keep %q: %s", bad, s)
		}
	}
	if !strings.Contains(s, "opus") || !strings.Contains(s, "KEEP") {
		t.Errorf("settings lost unrelated entries: %s", s)
	}
	for _, name := range []string{"history.jsonl", "projects"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err == nil {
			t.Errorf("%s was copied", name)
		}
	}
}

func TestPrepareOwnStartsWithoutState(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, ".claude.json"), `{"userID":"u","theme":"dark","mcpServers":{"a":{}}}`)
	if err := PrepareOwn(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".claude.json")); err == nil {
		t.Error("a new own profile must start without .claude.json (Claude Code makes its own)")
	}
	if HasCredentials(dst) {
		t.Error("credentials appeared in a new own profile")
	}
}

func TestPrepareOwnRefusesUsersOwnDir(t *testing.T) {
	d := t.TempDir()
	if err := PrepareOwn(d, d); err == nil {
		t.Fatal("PrepareOwn(src == dst) succeeded")
	}
}
