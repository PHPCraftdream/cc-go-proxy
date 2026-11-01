package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

// addClaude creates profile name of type claude in a fresh test config.
func addClaude(t *testing.T, name string) (string, *config.Config) {
	t.Helper()
	path, cfg := writeTestConfig(t, testConfig)
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Type: config.ProviderTypeClaude, Name: name}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	return path, loadTestConfig(t)
}

func TestProfileAddClaude(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	var out bytes.Buffer
	if err := profileAdd(&out, profileAddOpts{Type: config.ProviderTypeClaude, Name: "work"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	cfg2 := loadTestConfig(t)
	pr := cfg2.Providers["work"]
	if pr == nil || pr.Type != config.ProviderTypeClaude || pr.BaseURL != "" {
		t.Fatalf("provider = %+v", pr)
	}
	pf := profileByName(t, cfg2, "work")
	if pf.Provider != "work" || len(pf.Models) != 0 {
		t.Errorf("profile = %+v", pf)
	}
	b, _ := os.ReadFile(path)
	if config.HasManaged(string(b), "work") {
		t.Error("a Claude profile must not get a managed model block")
	}
	for _, want := range []string{`created Claude profile "work"`, "nothing goes through ccgp", "run: ccgp work", "/login"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "models apply") {
		t.Errorf("output offers model mapping:\n%s", out.String())
	}

	// A second account is another profile of its own, also via --like.
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Like: "work"}, path, cfg2, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	if p := loadTestConfig(t).Providers["work2"]; p == nil || p.Type != config.ProviderTypeClaude {
		t.Fatalf("--like of a Claude profile gave provider %+v", p)
	}
}

func TestProfileAddClaudeRefusals(t *testing.T) {
	path, cfg := addClaude(t, "work")
	tests := []struct {
		name string
		opts profileAddOpts
		want string
	}{
		{"base url", profileAddOpts{Type: "claude", Name: "w2", BaseURL: "https://example.com"}, "do not apply"},
		{"model", profileAddOpts{Type: "claude", Name: "w2", Model: "m", HasModel: true}, "do not apply"},
		{"window", profileAddOpts{Type: "claude", Name: "w2", Window: 1000, HasWindow: true}, "do not apply"},
		{"share the account", profileAddOpts{Provider: "work", Name: "w2"}, "own profile"},
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

func TestProfileListClaude(t *testing.T) {
	_, cfg := addClaude(t, "work")
	base := t.TempDir()
	row := func() profileRow {
		for _, r := range buildProfileRows(cfg, creds.Store{Dir: t.TempDir()}, base) {
			if r.Name == "work" {
				return r
			}
		}
		t.Fatal("no row for work")
		return profileRow{}
	}
	if r := row(); r.Type != "claude" || r.MainModel != "-" || !strings.Contains(r.Login, "sign in inside Claude Code") {
		t.Errorf("new Claude profile row = %+v", r)
	}
	if err := os.MkdirAll(filepath.Join(base, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "work", ".credentials.json"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := row(); r.Login != "signed in inside Claude Code" || !r.ClaudeDir {
		t.Errorf("signed-in Claude profile row = %+v", r)
	}
}

func TestProfileRemoveClaude(t *testing.T) {
	path, cfg := addClaude(t, "work")
	base := t.TempDir()
	dir := filepath.Join(base, "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := creds.Store{Dir: t.TempDir()}

	var out bytes.Buffer
	if err := profileRemove(&out, profileRmOpts{Name: "work", Yes: true}, path, cfg, store, base, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "with its sign-in") || strings.Contains(out.String(), "credentials of provider") {
		t.Errorf("output:\n%s", out.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the Claude dir was deleted without --purge: %v", err)
	}

	path, cfg = addClaude(t, "work")
	if err := profileRemove(&bytes.Buffer{}, profileRmOpts{Name: "work", Yes: true, Purge: true}, path, cfg, store, base, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("--purge left the Claude dir: %v", err)
	}
}

func TestClaudeProfileHasNoRoute(t *testing.T) {
	_, cfg := addClaude(t, "work")
	if _, err := whichRoute(cfg, "work", "sonnet"); err == nil || !strings.Contains(err.Error(), "does not go through ccgp") {
		t.Errorf("--which of a Claude profile: err = %v", err)
	}
	if _, err := proxyValue(cfg, "socks5://127.0.0.1:1080", "work"); err == nil || !strings.Contains(err.Error(), "does not go through ccgp") {
		t.Errorf("proxy of a Claude profile: err = %v", err)
	}
	if err := login(cfg, "work", false, false); err == nil || !strings.Contains(err.Error(), "/login") {
		t.Errorf("--login of a Claude profile: err = %v", err)
	}
}

func TestOwnEnvDropsEveryOfficialCredential(t *testing.T) {
	env := []string{
		"PATH=/bin",
		"ANTHROPIC_API_KEY=k",
		"ANTHROPIC_AUTH_TOKEN=t",
		"ANTHROPIC_BASE_URL=https://x",
		"CLAUDE_CODE_OAUTH_TOKEN=o",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CONFIG_DIR=/users/own",
		"KEEP=1",
	}
	joined := strings.Join(ownEnv(env, "/profile/work"), "\n")
	for _, bad := range []string{"ANTHROPIC_", "OAUTH", "BEDROCK", "/users/own"} {
		if strings.Contains(joined, bad) {
			t.Errorf("%q survived:\n%s", bad, joined)
		}
	}
	for _, want := range []string{"PATH=/bin", "KEEP=1", "CLAUDE_CONFIG_DIR=/profile/work"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q lost:\n%s", want, joined)
		}
	}
	// No gateway is set up: the session talks to Claude directly.
	if strings.Contains(joined, "ENABLE_CLAUDEAI_MCP_SERVERS") {
		t.Errorf("gateway defaults leaked into a Claude profile:\n%s", joined)
	}
}
