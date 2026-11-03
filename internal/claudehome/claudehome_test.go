package claudehome

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSourceDir(t *testing.T) {
	d, s := SourceDir(func(string) string { return "" }, "/home/u")
	if d != filepath.Join("/home/u", ".claude") || s != filepath.Join("/home/u", ".claude.json") {
		t.Errorf("default = %q, %q", d, s)
	}
	d, s = SourceDir(func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return "/cfg"
		}
		return ""
	}, "/home/u")
	if d != "/cfg" || s != filepath.Join("/cfg", ".claude.json") {
		t.Errorf("override = %q, %q", d, s)
	}
}

func TestSanitizeSettings(t *testing.T) {
	out, err := SanitizeSettings([]byte(`{
		"apiKeyHelper": "get-key.sh",
		"forceLoginMethod": "claudeai",
		"permissions": {"allow": ["Bash(ls)"]},
		"env": {"ANTHROPIC_BASE_URL": "http://x", "anthropic_api_key": "k", "CLAUDE_CODE_USE_BEDROCK": "1", "KEEP": "v"},
		"hooks": {"Stop": []}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"apiKeyHelper", "forceLoginMethod"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s survived", k)
		}
	}
	env := m["env"].(map[string]any)
	if len(env) != 1 || env["KEEP"] != "v" {
		t.Errorf("env = %v", env)
	}
	if m["permissions"] == nil || m["hooks"] == nil {
		t.Errorf("unrelated settings lost: %v", m)
	}
	if _, err := SanitizeSettings([]byte("not json")); err == nil {
		t.Error("invalid JSON accepted")
	}
}

func TestPrepareIsolatesAndMirrors(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	state := filepath.Join(root, "src.json")

	write(t, filepath.Join(src, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"official"}}`)
	write(t, filepath.Join(src, "CLAUDE.md"), "rules")
	write(t, filepath.Join(src, "settings.json"), `{"apiKeyHelper":"x","model":"opus"}`)
	write(t, filepath.Join(src, "history.jsonl"), "secret history")
	write(t, filepath.Join(src, "skills", "s1", "SKILL.md"), "skill")
	write(t, state, `{"oauthAccount":{"emailAddress":"me@example.com"},"userID":"u","theme":"dark","mcpServers":{"a":{}},"projects":{"/p":{}}}`)

	// A credential left in the isolated dir (e.g. by /login) is wiped on launch.
	write(t, filepath.Join(dst, ".credentials.json"), "leftover")

	if err := Prepare(src, state, dst); err != nil {
		t.Fatal(err)
	}
	if HasCredentials(dst) {
		t.Fatal("credentials present in the isolated dir")
	}
	if read(t, filepath.Join(dst, "CLAUDE.md")) != "rules" {
		t.Error("CLAUDE.md not mirrored")
	}
	if s := read(t, filepath.Join(dst, "settings.json")); strings.Contains(s, "apiKeyHelper") || !strings.Contains(s, "opus") {
		t.Errorf("settings = %s", s)
	}
	if _, err := os.Stat(filepath.Join(dst, "history.jsonl")); err == nil {
		t.Error("history copied")
	}
	if read(t, filepath.Join(dst, "skills", "s1", "SKILL.md")) != "skill" {
		t.Error("skills not shared")
	}

	st := read(t, filepath.Join(dst, ".claude.json"))
	for _, bad := range []string{"oauthAccount", "me@example.com", "userID", "projects"} {
		if strings.Contains(st, bad) {
			t.Errorf("account/project data %q copied into state: %s", bad, st)
		}
	}
	for _, want := range []string{"hasCompletedOnboarding", "theme", "mcpServers"} {
		if !strings.Contains(st, want) {
			t.Errorf("state lacks %q: %s", want, st)
		}
	}
}

func TestPrepareIsIdempotentAndFollowsSource(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, filepath.Join(src, "CLAUDE.md"), "v1")
	if err := Prepare(src, filepath.Join(root, "none.json"), dst); err != nil {
		t.Fatal(err)
	}
	// Claude Code edits its own state; it must survive later launches.
	write(t, filepath.Join(dst, ".claude.json"), `{"hasCompletedOnboarding":true,"numStartups":3}`)
	write(t, filepath.Join(src, "CLAUDE.md"), "v2")
	if err := os.Remove(filepath.Join(src, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "keybindings.json"), "{}")
	if err := Prepare(src, filepath.Join(root, "none.json"), dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "CLAUDE.md")); err == nil {
		t.Error("CLAUDE.md removed at the source is still mirrored")
	}
	if read(t, filepath.Join(dst, "keybindings.json")) != "{}" {
		t.Error("keybindings.json not mirrored")
	}
	if !strings.Contains(read(t, filepath.Join(dst, ".claude.json")), "numStartups") {
		t.Error("existing state was overwritten")
	}
}

// The interface options that only .claude.json holds (leftArrowOpensAgents and
// the like) follow the user into a new profile; the account does not.
func TestPrepareCarriesInterfacePreferences(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	state := filepath.Join(root, "src.json")
	write(t, state, `{"leftArrowOpensAgents":false,"copyOnSelect":false,"diffTool":"terminal","claudeInChromeDefaultEnabled":true,"userID":"u","oauthAccount":{"emailAddress":"me@example.com"}}`)
	if err := Prepare(src, state, dst); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dst, ".claude.json"))), &got); err != nil {
		t.Fatal(err)
	}
	if got["leftArrowOpensAgents"] != false || got["copyOnSelect"] != false || got["diffTool"] != "terminal" {
		t.Errorf("preferences not carried over: %v", got)
	}
	for _, bad := range []string{"claudeInChromeDefaultEnabled", "userID", "oauthAccount"} {
		if _, ok := got[bad]; ok {
			t.Errorf("%s was carried over: %v", bad, got)
		}
	}
}

// A profile made before a preference was carried over gets it on its next
// launch; what it already has is its own, and nothing else in its state moves.
func TestPrepareAddsMissingPreferencesToAnOldProfile(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	state := filepath.Join(root, "src.json")
	write(t, state, `{"leftArrowOpensAgents":false,"copyOnSelect":false,"prStatusFooterEnabled":false}`)
	write(t, filepath.Join(dst, ".claude.json"), `{"hasCompletedOnboarding":true,"numStartups":3,"copyOnSelect":true,"projects":{"/p":{"allowedTools":[]}}}`)
	if err := Prepare(src, state, dst); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dst, ".claude.json"))), &got); err != nil {
		t.Fatal(err)
	}
	if got["leftArrowOpensAgents"] != false || got["prStatusFooterEnabled"] != false {
		t.Errorf("missing preferences not added: %v", got)
	}
	if got["copyOnSelect"] != true {
		t.Errorf("the profile's own choice was replaced: %v", got)
	}
	if got["numStartups"] != float64(3) || got["hasCompletedOnboarding"] != true || got["projects"] == nil {
		t.Errorf("the profile's state changed: %v", got)
	}

	// A second launch changes nothing.
	before := read(t, filepath.Join(dst, ".claude.json"))
	if err := Prepare(src, state, dst); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dst, ".claude.json")) != before {
		t.Error("the state file was rewritten without a change")
	}
}

// A state file that cannot be read or parsed is left alone; the launch goes on.
func TestPreferencesAreBestEffort(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	state := filepath.Join(root, "src.json")
	write(t, state, `{"leftArrowOpensAgents":false}`)
	write(t, filepath.Join(dst, ".claude.json"), "{not json")
	if err := Prepare(src, state, dst); err != nil {
		t.Fatalf("Prepare failed for a broken state file: %v", err)
	}
	if got := read(t, filepath.Join(dst, ".claude.json")); got != "{not json" {
		t.Errorf("broken state file was rewritten: %q", got)
	}
	write(t, state, "{also not json")
	write(t, filepath.Join(dst, ".claude.json"), `{"numStartups":1}`)
	if err := Prepare(src, state, dst); err != nil || read(t, filepath.Join(dst, ".claude.json")) != `{"numStartups":1}` {
		t.Errorf("a broken source changed the profile: %v", err)
	}
}

func TestPrepareRefusesUsersOwnDir(t *testing.T) {
	d := t.TempDir()
	if err := Prepare(d, filepath.Join(d, ".claude.json"), d); err == nil {
		t.Fatal("Prepare(src == dst) succeeded")
	}
}

func TestIsAuthEnv(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "anthropic_auth_token", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_VERTEX"} {
		if !IsAuthEnv(k) {
			t.Errorf("%s not treated as auth env", k)
		}
	}
	for _, k := range []string{"PATH", "ANTHROPIC_MODEL", "CLAUDE_CONFIG_DIR"} {
		if IsAuthEnv(k) {
			t.Errorf("%s wrongly treated as auth env", k)
		}
	}
}
