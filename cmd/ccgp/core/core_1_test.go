package core

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/tune"
	"github.com/PHPCraftdream/cc-go-proxy/internal/clientver"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

const clientVersionProviders = "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: codex\n        provider: codex\n    }\n]\n"

func clientVersionConfig(t *testing.T, body string) (*config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path
}

func stubLatestRelease(t *testing.T, latest func(context.Context) (string, error)) {
	t.Helper()
	old := clientver.Default
	clientver.Default = &clientver.Upgrader{Latest: latest}
	t.Cleanup(func() { clientver.Default = old })
}

func fixedRelease(v string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return v, nil }
}

func TestClientVersionApplyRaisesTheConfig(t *testing.T) {
	cfg, path := clientVersionConfig(t, "codex.client_version:: 0.169.0\n"+clientVersionProviders)
	stubLatestRelease(t, fixedRelease("0.170.0"))
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, true, &out)
	if !strings.Contains(out.String(), "raised 0.169.0 -> 0.170.0") || cfg.CodexClientVersion() != "0.170.0" {
		t.Fatalf("output %q, version %s", out.String(), cfg.CodexClientVersion())
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "codex.client_version:: 0.170.0") {
		t.Errorf("config:\n%s", got)
	}
	if backups, _ := filepath.Glob(path + ".bak-*"); len(backups) != 1 {
		t.Errorf("backups: %v", backups)
	}
}

// The read-only form reads the catalog as the newer version and writes nothing.
func TestClientVersionShowDoesNotWrite(t *testing.T) {
	cfg, path := clientVersionConfig(t, "codex.client_version:: 0.169.0\n"+clientVersionProviders)
	before, _ := os.ReadFile(path)
	stubLatestRelease(t, fixedRelease("0.170.0"))
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, false, &out)
	if !strings.Contains(out.String(), "Codex CLI 0.170.0 is out") || cfg.CodexClientVersion() != "0.170.0" {
		t.Fatalf("output %q, version %s", out.String(), cfg.CodexClientVersion())
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Errorf("the config changed:\n%s", after)
	}
	if backups, _ := filepath.Glob(path + ".bak-*"); len(backups) != 0 {
		t.Errorf("backups: %v", backups)
	}
}

func TestClientVersionNothingNewerIsQuiet(t *testing.T) {
	for _, latest := range []string{"0.169.0", "0.100.0"} {
		cfg, path := clientVersionConfig(t, "codex.client_version:: 0.169.0\n"+clientVersionProviders)
		before, _ := os.ReadFile(path)
		stubLatestRelease(t, fixedRelease(latest))
		var out bytes.Buffer
		clientVersion(context.Background(), cfg, true, &out)
		if out.Len() != 0 || cfg.CodexClientVersion() != "0.169.0" {
			t.Errorf("latest %s: output %q, version %s", latest, out.String(), cfg.CodexClientVersion())
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Errorf("latest %s: the config changed", latest)
		}
	}
}

func TestClientVersionLookupFailureKeepsTheConfigured(t *testing.T) {
	cfg, path := clientVersionConfig(t, "codex.client_version:: 0.169.0\n"+clientVersionProviders)
	before, _ := os.ReadFile(path)
	stubLatestRelease(t, func(context.Context) (string, error) { return "", errors.New("offline") })
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, true, &out)
	if !strings.Contains(out.String(), "could not check for a newer Codex CLI (offline)") || cfg.CodexClientVersion() != "0.169.0" {
		t.Fatalf("output %q, version %s", out.String(), cfg.CodexClientVersion())
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the config changed")
	}
}

// A config without the key gets the built-in default first, then the newer release.
func TestClientVersionSeedsThenRaises(t *testing.T) {
	cfg, path := clientVersionConfig(t, clientVersionProviders)
	stubLatestRelease(t, fixedRelease("0.171.0"))
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, true, &out)
	text := out.String()
	if !strings.Contains(text, "codex.client_version "+config.DefaultCodexClientVersion+" written to") || !strings.Contains(text, "raised "+config.DefaultCodexClientVersion+" -> 0.171.0") {
		t.Fatalf("output %q", text)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "codex.client_version:: 0.171.0") {
		t.Errorf("config:\n%s", got)
	}
}

// Without a codex provider nothing is written, whatever the release says.
func TestClientVersionNeedsACodexProvider(t *testing.T) {
	cfg, path := clientVersionConfig(t, "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n]\n")
	before, _ := os.ReadFile(path)
	stubLatestRelease(t, fixedRelease(config.DefaultCodexClientVersion))
	var out bytes.Buffer
	clientVersion(context.Background(), cfg, true, &out)
	if out.Len() != 0 {
		t.Errorf("output %q", out.String())
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Errorf("the config changed:\n%s", after)
	}
}

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
	err := Logout(&out, strings.NewReader(input), store, cfg, provider, yes)
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
		if err := LogoutCmd(args); err == nil || !strings.Contains(err.Error(), "usage: ccgp --logout") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// addClaude creates profile name of type claude in a fresh test config.
func addClaude(t *testing.T, name string) (string, *config.Config) {
	t.Helper()
	path, cfg := writeTestConfig(t, testConfig)
	if err := ProfileAdd(&bytes.Buffer{}, ProfileAddOpts{Type: config.ProviderTypeClaude, Name: name}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	return path, loadTestConfig(t)
}

func TestProfileAddClaude(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	var out bytes.Buffer
	if err := ProfileAdd(&out, ProfileAddOpts{Type: config.ProviderTypeClaude, Name: "work"}, path, cfg, creds.Store{}); err != nil {
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
	if cfgedit.HasManaged(string(b), "work") {
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
	if err := ProfileAdd(&bytes.Buffer{}, ProfileAddOpts{Like: "work"}, path, cfg2, creds.Store{}); err != nil {
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
		opts ProfileAddOpts
		want string
	}{
		{"base url", ProfileAddOpts{Type: "claude", Name: "w2", BaseURL: "https://example.com"}, "do not apply"},
		{"model", ProfileAddOpts{Type: "claude", Name: "w2", Model: "m", HasModel: true}, "do not apply"},
		{"window", ProfileAddOpts{Type: "claude", Name: "w2", Window: 1000, HasWindow: true}, "do not apply"},
		{"share the account", ProfileAddOpts{Provider: "work", Name: "w2"}, "own profile"},
	}
	for _, tt := range tests {
		if err := ProfileAdd(&bytes.Buffer{}, tt.opts, path, cfg, creds.Store{}); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}
	if cfg2 := loadTestConfig(t); len(cfg2.Profiles) != len(cfg.Profiles) {
		t.Errorf("a refusal changed the config: %v", common.Names(cfg2.Profiles))
	}
}

func TestProfileListClaude(t *testing.T) {
	_, cfg := addClaude(t, "work")
	base := t.TempDir()
	row := func() ProfileRow {
		for _, r := range BuildProfileRows(cfg, creds.Store{Dir: t.TempDir()}, base) {
			if r.Name == "work" {
				return r
			}
		}
		t.Fatal("no row for work")
		return ProfileRow{}
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
	if err := ProfileRemove(&out, ProfileRmOpts{Name: "work", Yes: true}, path, cfg, store, base, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "with its sign-in") || strings.Contains(out.String(), "credentials of provider") {
		t.Errorf("output:\n%s", out.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the Claude dir was deleted without --purge: %v", err)
	}

	path, cfg = addClaude(t, "work")
	if err := ProfileRemove(&bytes.Buffer{}, ProfileRmOpts{Name: "work", Yes: true, Purge: true}, path, cfg, store, base, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("--purge left the Claude dir: %v", err)
	}
}

func TestClaudeProfileHasNoRoute(t *testing.T) {
	_, cfg := addClaude(t, "work")
	if _, err := WhichRoute(cfg, "work", "sonnet"); err == nil || !strings.Contains(err.Error(), "does not go through ccgp") {
		t.Errorf("--which of a Claude profile: err = %v", err)
	}
	if _, err := tune.ProxyValue(cfg, "socks5://127.0.0.1:1080", "work"); err == nil || !strings.Contains(err.Error(), "does not go through ccgp") {
		t.Errorf("proxy of a Claude profile: err = %v", err)
	}
	if err := Login(cfg, "work", false, false); err == nil || !strings.Contains(err.Error(), "/login") {
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
	joined := strings.Join(OwnEnv(env, "/profile/work"), "\n")
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
