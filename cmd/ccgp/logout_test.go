package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

// logoutTestCfg is a file-mode config with a Codex and an API-key provider.
func logoutTestCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		ConfigFile: filepath.Join(t.TempDir(), "config.ktav"),
		Providers: map[string]*config.Provider{
			"codex": {Name: "codex", Type: config.ProviderTypeCodex},
			"api":   {Name: "api", Type: config.ProviderTypeCodexAPI},
			"envk":  {Name: "envk", Type: config.ProviderTypeCodexAPI, APIKeyEnv: "CCGP_TEST_KEY"},
		},
	}
}

func saveOAuth(t *testing.T, store creds.Store, provider string) {
	t.Helper()
	tok := &oauth.Tokens{AccessToken: "at", RefreshToken: "rt"}
	if err := (oauth.Store{Path: store.OAuthPath(provider)}).Save(tok); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runLogout(t *testing.T, store creds.Store, cfg *config.Config, provider, input string, yes bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := logout(&out, strings.NewReader(input), store, cfg, provider, yes)
	return out.String(), err
}

func TestLogoutRemovesOAuthLogin(t *testing.T) {
	store := creds.Store{Dir: t.TempDir()}
	saveOAuth(t, store, "codex")
	out, err := runLogout(t, store, logoutTestCfg(t), "codex", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if exists(store.OAuthPath("codex")) {
		t.Error("OAuth file still exists")
	}
	if !strings.Contains(out, "logged out of codex") || !strings.Contains(out, "ccgp --login codex") {
		t.Errorf("output = %q", out)
	}
}

func TestLogoutRemovesAPIKeyAndKeepsOthers(t *testing.T) {
	store := creds.Store{Dir: t.TempDir()}
	for _, p := range []string{"api", "envk"} {
		if err := store.SaveAPIKey(p, "sk-fake"); err != nil {
			t.Fatal(err)
		}
	}
	saveOAuth(t, store, "codex")
	if _, err := runLogout(t, store, logoutTestCfg(t), "api", "", true); err != nil {
		t.Fatal(err)
	}
	if store.HasProviderCreds("api") {
		t.Error("api key still stored")
	}
	if !store.HasProviderCreds("envk") || !store.HasProviderCreds("codex") {
		t.Error("other providers lost their logins")
	}
}

func TestLogoutStoredKeyOfEnvBackedProvider(t *testing.T) {
	store := creds.Store{Dir: t.TempDir()}
	cfg := logoutTestCfg(t)

	out, err := runLogout(t, store, cfg, "envk", "", true)
	if err != nil || !strings.Contains(out, "$CCGP_TEST_KEY") || !strings.Contains(out, "no stored login") {
		t.Fatalf("nothing stored: out %q err %v", out, err)
	}

	if err := store.SaveAPIKey("envk", "sk-fake"); err != nil {
		t.Fatal(err)
	}
	if _, err := runLogout(t, store, cfg, "envk", "", true); err != nil {
		t.Fatal(err)
	}
	if store.HasProviderCreds("envk") {
		t.Error("stored key of an env-backed provider was left behind")
	}
}

func TestLogoutRemovesCorruptFile(t *testing.T) {
	store := creds.Store{Dir: t.TempDir()}
	if err := os.WriteFile(store.OAuthPath("codex"), []byte("{ not a login"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runLogout(t, store, logoutTestCfg(t), "codex", "", true); err != nil {
		t.Fatal(err)
	}
	if exists(store.OAuthPath("codex")) {
		t.Error("corrupt file was left behind")
	}
}

func TestLogoutNotLoggedIn(t *testing.T) {
	store := creds.Store{Dir: t.TempDir()}
	out, err := runLogout(t, store, logoutTestCfg(t), "codex", "", false)
	if err != nil || !strings.Contains(out, `provider "codex" has no stored login`) {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestLogoutErrors(t *testing.T) {
	store := creds.Store{Dir: t.TempDir()}
	if _, err := runLogout(t, store, logoutTestCfg(t), "nope", "", true); err == nil || !strings.Contains(err.Error(), "api, codex, envk") {
		t.Errorf("unknown provider: %v", err)
	}
	if _, err := runLogout(t, store, &config.Config{}, "codex", "", true); err == nil || !strings.Contains(err.Error(), "env-only") {
		t.Errorf("env mode: %v", err)
	}
}

func TestLogoutConfirmation(t *testing.T) {
	for _, tt := range []struct {
		input   string
		removed bool
	}{
		{"n\n", false},
		{"\n", false},
		{"", false},
		{"maybe\n", false},
		{"y\n", true},
		{"YES\n", true},
	} {
		store := creds.Store{Dir: t.TempDir()}
		saveOAuth(t, store, "codex")
		out, err := runLogout(t, store, logoutTestCfg(t), "codex", tt.input, false)
		if err != nil {
			t.Fatalf("%q: %v", tt.input, err)
		}
		if !strings.Contains(out, "[y/N]") {
			t.Errorf("%q: no prompt in %q", tt.input, out)
		}
		if exists(store.OAuthPath("codex")) == tt.removed {
			t.Errorf("%q: removed = %v, want %v", tt.input, !exists(store.OAuthPath("codex")), tt.removed)
		}
		if !tt.removed && !strings.Contains(out, "aborted") {
			t.Errorf("%q: no abort message in %q", tt.input, out)
		}
	}
}

func TestLogoutNeverWritesConfig(t *testing.T) {
	cfg := logoutTestCfg(t)
	want := []byte("profiles: []\n")
	if err := os.WriteFile(cfg.ConfigFile, want, 0o600); err != nil {
		t.Fatal(err)
	}
	store := creds.Store{Dir: t.TempDir()}
	saveOAuth(t, store, "codex")
	if _, err := runLogout(t, store, cfg, "codex", "", true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(cfg.ConfigFile)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("config changed: %q, %v", got, err)
	}
}

func TestLogoutCmdUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"a", "b"}, {"--yes"}} {
		if err := logoutCmd(args); err == nil || !strings.Contains(err.Error(), "usage: ccgp --logout") {
			t.Errorf("%v: %v", args, err)
		}
	}
}
