package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/internal/clientver"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
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

func whichTestConfig() *config.Config {
	return &config.Config{
		ConfigFile: "config.ktav", // file mode
		Providers: map[string]*config.Provider{
			"codex": {Name: "codex", Type: config.ProviderTypeCodex},
		},
		Profiles: map[string]*config.Profile{
			"codex": {
				Name:     "codex",
				Provider: "codex",
				Models: []config.ModelRule{
					{Match: "*haiku*", Model: "mini", ContextWindow: 272000},
					{Match: "claude-opus-*", Model: "big", ContextWindow: 0},
				},
			},
			"empty": {Name: "empty", Provider: "codex"},
		},
	}
}

func TestWhichRouteTiers(t *testing.T) {
	cfg := whichTestConfig()
	for _, tc := range []struct{ model, backend, matchedBy string }{
		{"claude-3-5-haiku-20241022", "mini", "rule *haiku*"},
		{"claude-opus-4-1", "big", "rule claude-opus-*"},
		{"claude-sonnet-4", "gpt-5", "tier setting"},
		{"gpt-4o", "gpt-4o", "passthrough"},
	} {
		ri, err := WhichRoute(cfg, "codex", tc.model)
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		if ri.BackendModel != tc.backend {
			t.Errorf("%s: backend = %q, want %q", tc.model, ri.BackendModel, tc.backend)
		}
		if ri.MatchedBy != tc.matchedBy {
			t.Errorf("%s: matchedBy = %q, want %q", tc.model, ri.MatchedBy, tc.matchedBy)
		}
	}
}

func TestWhichRouteDefaultModel(t *testing.T) {
	cfg := whichTestConfig()
	cfg.Profiles["codex"].DefaultModel = "fallback-model"
	ri, err := WhichRoute(cfg, "codex", "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if ri.BackendModel != "fallback-model" || ri.MatchedBy != "default_model" {
		t.Errorf("got %q / %q, want fallback-model / default_model", ri.BackendModel, ri.MatchedBy)
	}
}

func TestWhichRouteContextWindow(t *testing.T) {
	cfg := whichTestConfig()
	if ri, _ := WhichRoute(cfg, "codex", "claude-3-5-haiku-20241022"); ri.ContextWindow != "272000" {
		t.Errorf("haiku window = %q, want 272000", ri.ContextWindow)
	}
	// A rule without a window falls back to the smallest window of the profile.
	if ri, _ := WhichRoute(cfg, "codex", "claude-opus-4-1"); ri.ContextWindow != "272000" {
		t.Errorf("opus window = %q, want the profile fallback 272000", ri.ContextWindow)
	}
	// No window anywhere in the profile: unknown.
	if ri, err := WhichRoute(cfg, "empty", "gpt-4o"); err != nil || ri.ContextWindow != "unknown" {
		t.Errorf("empty profile window = %q (%v), want unknown", ri.ContextWindow, err)
	}
}

func TestWhichRouteUnknownProfile(t *testing.T) {
	cfg := whichTestConfig()
	_, err := WhichRoute(cfg, "nope", "claude-sonnet-4")
	if err == nil {
		t.Fatal("expected error")
	}
	want := `unknown profile "nope" (configured: codex, empty)`
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

func TestWhichRouteJSONShape(t *testing.T) {
	cfg := whichTestConfig()
	ri, err := WhichRoute(cfg, "codex", "claude-3-5-haiku-20241022")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := common.WriteJSON(&buf, ri); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"profile":       "codex",
		"provider":      "codex",
		"providerType":  "codex",
		"requested":     "claude-3-5-haiku-20241022",
		"backendModel":  "mini",
		"contextWindow": "272000",
		"matchedBy":     "rule *haiku*",
	}
	for k, v := range want {
		if doc[k] != v {
			t.Errorf("%s = %v, want %q", k, doc[k], v)
		}
	}
	for k := range doc {
		if !strings.Contains(strings.Join([]string{"profile", "provider", "providerType", "requested", "backendModel", "contextWindow", "matchedBy"}, " "), k) {
			t.Errorf("unexpected JSON field %q", k)
		}
	}
}

func TestWhichRouteTextOutput(t *testing.T) {
	cfg := whichTestConfig()
	ri, err := WhichRoute(cfg, "empty", "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := renderRouteText(&buf, ri); err != nil {
		t.Fatal(err)
	}
	// Values start in one column, after the longest label ("context window:").
	row := func(label, value string) string {
		return label + strings.Repeat(" ", 16-len(label)) + value + "\n"
	}
	want := row("profile:", "empty") +
		row("provider:", "codex (codex)") +
		row("requested:", "gpt-4o") +
		row("backend model:", "gpt-4o") +
		row("context window:", "unknown") +
		row("matched by:", "passthrough")
	if buf.String() != want {
		t.Errorf("output:\n%q\nwant:\n%q", buf.String(), want)
	}
}

func TestParseWhichArgs(t *testing.T) {
	p, m, j, err := parseWhichArgs([]string{"codex", "--json", "claude-opus-4-1"})
	if err != nil || p != "codex" || m != "claude-opus-4-1" || !j {
		t.Errorf("got %q %q %v %v", p, m, j, err)
	}
	p, m, j, err = parseWhichArgs([]string{"codex", "x"})
	if err != nil || p != "codex" || m != "x" || j {
		t.Errorf("got %q %q %v %v", p, m, j, err)
	}
}

// Env-mode tier overrides apply in env mode only; file mode ignores them.
func TestWhichRouteEnvModeTierOverrides(t *testing.T) {
	env := &config.Config{
		ProviderName: "env", ProviderType: config.ProviderTypeCodexAPI,
		OpusModel: "my-opus", HaikuModel: "my-haiku", SonnetModel: "my-sonnet",
		Profiles: map[string]*config.Profile{"default": {Name: "default"}},
	}
	for model, want := range map[string]string{
		"claude-opus-4-1": "my-opus", "claude-haiku-4": "my-haiku", "claude-sonnet-4": "my-sonnet",
	} {
		ri, err := WhichRoute(env, "default", model)
		if err != nil || ri.BackendModel != want {
			t.Errorf("%s: backend %q (%v), want %q", model, ri.BackendModel, err, want)
		}
	}
	// File mode (the tier fallback above is the converter default, not an override).
	ri, err := WhichRoute(whichTestConfig(), "empty", "claude-opus-4-1")
	if err != nil || ri.BackendModel != "gpt-5" {
		t.Errorf("file mode opus: %q (%v), want gpt-5", ri.BackendModel, err)
	}
}

func TestWhichCmdUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"onlyone"},
		{"a", "b", "c"},
		{"--json"},
	} {
		if err := WhichCmd(args); err == nil || !strings.Contains(err.Error(), "usage: ccgp --which <profile> <model> [--json]") {
			t.Errorf("whichCmd(%v) = %v, want usage error", args, err)
		}
	}
}
