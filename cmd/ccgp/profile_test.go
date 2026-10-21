package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/preset"
)

// testConfig covers one provider per interesting shape: codex (managed
// mapping), zai (preset, effort_map, api_key_env) and a rule-less profile.
const testConfig = `listen.host: 127.0.0.1
listen.port: 60093

providers: [
    {
        name: codex
        type: codex
    }
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
        api_key_env: ZAI_KEY_TEST
        effort_map: {
            medium: high
            max: max
        }
    }
]

profiles: [
    {
        name: codex
        provider: codex
        ## ccgp:managed-models begin codex
        models: [
            {
                match: *sonnet*
                model: gpt-6-luna
                context_window: 872000
            }
            {
                match: *
                model: gpt-6-luna
                context_window: 872000
            }
        ]
        ## ccgp:managed-models end codex
    }
    {
        name: zai
        provider: zai
        models: [
            {
                match: *
                model: glm-5.3-flash
                context_window: 1000000
            }
        ]
    }
    {
        name: zai2
        provider: zai
    }
]
`

// writeTestConfig writes a fixture and returns (path, loaded config).
func writeTestConfig(t *testing.T, text string) (string, *config.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCGP_CONFIG", path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("fixture does not load: %v", err)
	}
	return path, cfg
}

func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func profileByName(t *testing.T, cfg *config.Config, name string) *config.Profile {
	t.Helper()
	pf, ok := cfg.Profiles[name]
	if !ok {
		t.Fatalf("profile %q missing after the edit (have %v)", name, names(cfg.Profiles))
	}
	return pf
}

func TestProfileAddLikeZai(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	var out bytes.Buffer
	if err := profileAdd(&out, profileAddOpts{Like: "zai"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	// zai and zai2 are taken, so the default name is zai3.
	cfg2 := loadTestConfig(t)
	pf := profileByName(t, cfg2, "zai3")
	if pf.Provider != "zai3" {
		t.Errorf("provider = %q, want the new provider zai3", pf.Provider)
	}
	want := cfg.Profiles["zai"].Models
	if !reflect.DeepEqual(pf.Models, want) {
		t.Errorf("rules = %+v, want the source's %+v", pf.Models, want)
	}
	pr := cfg2.Providers["zai3"]
	if pr == nil {
		t.Fatal("provider zai3 missing")
	}
	if pr.Type != config.ProviderTypeAnthropicCompat || pr.BaseURL != "https://api.z.ai/api/anthropic" {
		t.Errorf("provider = %+v, want type and base_url copied", pr)
	}
	if pr.APIKeyEnv != "" {
		t.Errorf("api_key_env = %q, want it dropped (own credentials)", pr.APIKeyEnv)
	}
	if !reflect.DeepEqual(pr.EffortMap, map[string]string{"medium": "high", "max": "max"}) {
		t.Errorf("effort_map = %v, want it copied", pr.EffortMap)
	}
	b, _ := os.ReadFile(path)
	if config.HasManaged(string(b), "zai3") {
		t.Error("markers were added although the source has none")
	}
	if !strings.Contains(out.String(), "zai3") || !strings.Contains(out.String(), "run: ccgp zai3") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestProfileAddLikeCodexCopiesManagedMarkers(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	var out bytes.Buffer
	if err := profileAdd(&out, profileAddOpts{Like: "codex"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	cfg2 := loadTestConfig(t)
	pf := profileByName(t, cfg2, "codex2")
	if pf.Provider != "codex2" || len(pf.Models) != 2 {
		t.Fatalf("profile = %+v", pf)
	}
	if pf.Models[0].Model != "gpt-6-luna" || pf.Models[0].ContextWindow != 872000 {
		t.Errorf("first rule = %+v", pf.Models[0])
	}
	b, _ := os.ReadFile(path)
	if !config.HasManaged(string(b), "codex2") {
		t.Error("the managed markers of the source were not copied")
	}
	pr := cfg2.Providers["codex2"]
	if pr == nil || pr.Type != config.ProviderTypeCodex {
		t.Errorf("provider codex2 = %+v", pr)
	}
}

func TestProfileAddProviderShared(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)

	// With --model: one catch-all rule, 272000 window by default.
	var out bytes.Buffer
	err := profileAdd(&out, profileAddOpts{Provider: "codex", Name: "extra", Model: "m-one", HasModel: true}, path, cfg, creds.Store{})
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := loadTestConfig(t)
	pf := profileByName(t, cfg2, "extra")
	if pf.Provider != "codex" {
		t.Fatalf("provider = %q", pf.Provider)
	}
	want := []config.ModelRule{{Match: "*", Model: "m-one", ContextWindow: 272000}}
	if !reflect.DeepEqual(pf.Models, want) {
		t.Errorf("rules = %+v, want %+v", pf.Models, want)
	}
	if !strings.Contains(out.String(), "272000") || !strings.Contains(out.String(), `sharing provider "codex"`) {
		t.Errorf("output:\n%s", out.String())
	}

	// Without --model: the mapping of the provider's first profile (by name).
	path, cfg = writeTestConfig(t, testConfig)
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Provider: "zai", Name: "zai3"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	cfg3 := loadTestConfig(t)
	pf = profileByName(t, cfg3, "zai3")
	if !reflect.DeepEqual(pf.Models, cfg.Profiles["zai"].Models) {
		t.Errorf("rules = %+v, want the copied ones %+v", pf.Models, cfg.Profiles["zai"].Models)
	}
	b, _ := os.ReadFile(path)
	if config.HasManaged(string(b), "zai3") {
		t.Error("the source profile has no markers, so none must be added")
	}

	// Explicit --window overrides the default.
	path, cfg = writeTestConfig(t, testConfig)
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Provider: "codex", Name: "w", Model: "m2", HasModel: true, Window: 123456, HasWindow: true}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	pf = loadTestConfig(t).Profiles["w"]
	if len(pf.Models) != 1 || pf.Models[0].ContextWindow != 123456 {
		t.Errorf("rules = %+v, want window 123456", pf.Models)
	}
}

func TestProfileAddTypeNewBackend(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	var out bytes.Buffer
	err := profileAdd(&out, profileAddOpts{Type: config.ProviderTypeCodexAPI, Name: "api2", BaseURL: "https://example.com/v1", Model: "m-three", HasModel: true, Window: 100000, HasWindow: true}, path, cfg, creds.Store{})
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := loadTestConfig(t)
	pr := cfg2.Providers["api2"]
	if pr == nil || pr.Type != config.ProviderTypeCodexAPI || pr.BaseURL != "https://example.com/v1" {
		t.Fatalf("provider = %+v", pr)
	}
	pf := profileByName(t, cfg2, "api2")
	want := []config.ModelRule{{Match: "*", Model: "m-three", ContextWindow: 100000}}
	if !reflect.DeepEqual(pf.Models, want) {
		t.Errorf("rules = %+v, want %+v", pf.Models, want)
	}

	// codex without --model: ready for `ccgp models apply` (managed block).
	path, cfg = writeTestConfig(t, testConfig)
	out.Reset()
	if err := profileAdd(&out, profileAddOpts{Type: config.ProviderTypeCodex, Name: "fresh"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !config.HasManaged(string(b), "fresh") {
		t.Error("a codex profile without --model must get an empty managed block")
	}
	if !strings.Contains(out.String(), "then: ccgp models apply fresh") {
		t.Errorf("output:\n%s", out.String())
	}

	// A preset base URL behaves the same and carries the preset's effort map.
	path, cfg = writeTestConfig(t, testConfig)
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Type: config.ProviderTypeAnthropicCompat, Name: "glm2", BaseURL: preset.ZAIBaseURL}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if !config.HasManaged(string(b), "glm2") {
		t.Error("a preset provider without --model must get an empty managed block")
	}
	pr = loadTestConfig(t).Providers["glm2"]
	if !reflect.DeepEqual(pr.EffortMap, preset.ZAI.EffortMap) {
		t.Errorf("effort_map = %v, want the preset's", pr.EffortMap)
	}

	// anthropic_compat without preset and without --model: no rules, no block.
	path, cfg = writeTestConfig(t, testConfig)
	out.Reset()
	if err := profileAdd(&out, profileAddOpts{Type: config.ProviderTypeAnthropicCompat, Name: "raw", BaseURL: "https://example.com/anthropic"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "models apply") {
		t.Errorf("no generator exists, output must say to edit:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no model rules were copied") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestProfileAddRefusals(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	store := creds.Store{}
	tests := []struct {
		name string
		opts profileAddOpts
		want string
	}{
		{"existing name", profileAddOpts{Like: "codex", Name: "zai"}, "already in use"},
		{"reserved name", profileAddOpts{Like: "codex", Name: "serve"}, "command"},
		{"invalid name", profileAddOpts{Like: "codex", Name: "Bad"}, "[a-z0-9_-]"},
		{"unknown source", profileAddOpts{Like: "nope"}, "unknown profile"},
		{"unknown provider", profileAddOpts{Provider: "nope", Name: "x"}, "unknown provider"},
		{"bad type", profileAddOpts{Type: "weird", Name: "x"}, "--type must be"},
	}
	for _, tt := range tests {
		if err := profileAdd(&bytes.Buffer{}, tt.opts, path, cfg, store); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}
	// Nothing was written.
	cfg2 := loadTestConfig(t)
	if len(cfg2.Profiles) != len(cfg.Profiles) || len(cfg2.Providers) != len(cfg.Providers) {
		t.Errorf("a refusal changed the config: %v / %v", names(cfg2.Profiles), names(cfg2.Providers))
	}
}

func TestParseProfileAdd(t *testing.T) {
	o, err := parseProfileAdd([]string{"p1", "--like", "codex", "--model", "m", "--window", "5"})
	if err != nil || o.Name != "p1" || o.Like != "codex" || o.Model != "m" || !o.HasModel || o.Window != 5 || !o.HasWindow {
		t.Fatalf("parse = %+v, %v", o, err)
	}
	if o, err = parseProfileAdd([]string{"--provider=oa", "p2"}); err != nil || o.Provider != "oa" || o.Name != "p2" {
		t.Fatalf("--flag=value: %+v, %v", o, err)
	}
	// The parser only checks the syntax of the flags; the choice of the source
	// (--like / --provider / --type) is validated by profileAdd, below.
	for _, bad := range [][]string{
		{"--nope"},
		{"x", "y"},
		{"--model"},
		{"--window", "x", "n"},
		{"--window", "0", "n"},
		{"-x"},
	} {
		if _, err := parseProfileAdd(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	// Exactly one source flag: two of them are a usage error in the core.
	path, cfg := writeTestConfig(t, testConfig)
	err = profileAdd(&bytes.Buffer{}, profileAddOpts{Name: "x", Like: "codex", Provider: "zai"}, path, cfg, creds.Store{})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("two source flags: %v", err)
	}
	err = profileAdd(&bytes.Buffer{}, profileAddOpts{Name: "x"}, path, cfg, creds.Store{})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("no source flag: %v", err)
	}
}

func TestProfileList(t *testing.T) {
	_, cfg := writeTestConfig(t, testConfig)
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := profileList(&out, cfg, creds.Store{Dir: t.TempDir()}, base); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"PROFILE", "PROVIDER", "TYPE", "LOGIN", "MAIN MODEL", "CLAUDE DIR",
		"not logged in (ccgp login codex)",
		"gpt-6-luna", // sonnet tier of the codex profile
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	rows := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(rows) != 4 {
		t.Fatalf("want a header and 3 rows:\n%s", got)
	}
	if f := strings.Fields(rows[1]); len(f) < 3 || f[0] != "codex" || f[1] != "codex" || f[2] != "codex" {
		t.Errorf("codex row = %v", f)
	}
	if !strings.Contains(rows[1], "yes") {
		t.Errorf("the existing claude dir of codex is not reported:\n%s", got)
	}
	if f := strings.Fields(rows[3]); len(f) < 3 || f[0] != "zai2" || f[1] != "zai" || f[2] != "anthropic_compat" {
		t.Errorf("zai2 row = %v", f)
	}
	if !strings.Contains(rows[3], "no") {
		t.Errorf("the missing claude dir of zai2 is not reported:\n%s", got)
	}
	// Rows are sorted by profile name.
	if strings.Index(got, "codex ") > strings.Index(got, "zai ") {
		t.Errorf("rows are not sorted:\n%s", got)
	}
}

func TestProfileRemoveSharedAndUnusedProvider(t *testing.T) {
	// zai is used by zai and zai2: removing zai2 keeps provider zai.
	path, cfg := writeTestConfig(t, testConfig)
	store := creds.Store{Dir: t.TempDir()}
	if _, err := store.EnsureProfileToken("zai2"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := profileRemove(&out, profileRmOpts{Name: "zai2", Yes: true}, path, cfg, store, t.TempDir(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	cfg2 := loadTestConfig(t)
	if _, ok := cfg2.Profiles["zai2"]; ok {
		t.Error("zai2 still exists")
	}
	if cfg2.Providers["zai"] == nil {
		t.Error("provider zai was removed although zai still uses it")
	}
	if !strings.Contains(out.String(), "still has other profiles") {
		t.Errorf("output:\n%s", out.String())
	}
	if m, _ := store.ProfileTokens(); m["zai2"] != "" {
		t.Errorf("token entry kept: %v", m)
	}

	// codex is the only profile on provider codex: both go.
	path, cfg = writeTestConfig(t, testConfig)
	store = creds.Store{Dir: t.TempDir()}
	if _, err := store.EnsureProfileToken("codex"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAPIKey("codex", "sk-test"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := profileRemove(&out, profileRmOpts{Name: "codex", Yes: true}, path, cfg, store, t.TempDir(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	// No --purge: the login of the removed provider stays on disk and is said so.
	if !strings.Contains(out.String(), `left behind: the credentials of provider "codex"`) {
		t.Errorf("output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "apikey-codex.json")); err != nil {
		t.Errorf("credentials were deleted without --purge: %v", err)
	}
	cfg3 := loadTestConfig(t)
	if cfg3.Profiles["codex"] != nil || cfg3.Providers["codex"] != nil {
		t.Errorf("profile or provider kept: %v / %v", names(cfg3.Profiles), names(cfg3.Providers))
	}
	if m, _ := store.ProfileTokens(); m["codex"] != "" {
		t.Error("token entry kept")
	}
}

func TestProfileRemoveLastProfileRefused(t *testing.T) {
	one := `listen.host: 127.0.0.1

providers: [
    {
        name: codex
        type: codex
    }
]

profiles: [
    {
        name: codex
        provider: codex
    }
]
`
	path, cfg := writeTestConfig(t, one)
	before, _ := os.ReadFile(path)
	if err := profileRemove(&bytes.Buffer{}, profileRmOpts{Name: "codex", Yes: true}, path, cfg, creds.Store{Dir: t.TempDir()}, t.TempDir(), strings.NewReader("")); err == nil {
		t.Fatal("the last profile was removed")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("a refused removal changed the config")
	}
}

func TestProfileRemovePurge(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "zai2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := creds.Store{Dir: t.TempDir()}
	if _, err := store.EnsureProfileToken("zai2"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAPIKey("zai", "sk-test"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := profileRemove(&out, profileRmOpts{Name: "zai2", Yes: true, Purge: true}, path, cfg, store, base, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "zai2")); !os.IsNotExist(err) {
		t.Errorf("the profile's Claude dir survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "keep")); err != nil {
		t.Errorf("a sibling directory was touched: %v", err)
	}
	// Provider zai stays (zai still uses it), so its credentials stay too.
	if _, err := os.Stat(filepath.Join(store.Dir, "apikey-zai.json")); err != nil {
		t.Errorf("credentials of the kept provider were deleted: %v", err)
	}

	// --purge on the only profile of a provider deletes the credentials too.
	path, cfg = writeTestConfig(t, testConfig)
	store = creds.Store{Dir: t.TempDir()}
	if err := store.SaveAPIKey("zai", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if err := profileRemove(&bytes.Buffer{}, profileRmOpts{Name: "zai2", Yes: true, Purge: true}, path, cfg, store, t.TempDir(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	// zai is still used by profile zai, so nothing was deleted yet; remove zai itself.
	if err := profileRemove(&bytes.Buffer{}, profileRmOpts{Name: "zai", Yes: true, Purge: true}, path, loadTestConfig(t), store, t.TempDir(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "apikey-zai.json")); !os.IsNotExist(err) {
		t.Errorf("credentials of the removed provider survived: %v", err)
	}
}

func TestProfileRemoveConfirmation(t *testing.T) {
	path, cfg := writeTestConfig(t, testConfig)
	var out bytes.Buffer
	if err := profileRemove(&out, profileRmOpts{Name: "zai2"}, path, cfg, creds.Store{Dir: t.TempDir()}, t.TempDir(), strings.NewReader("n\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `remove profile "zai2"? [y/N]`) || !strings.Contains(out.String(), "aborted") {
		t.Errorf("output:\n%s", out.String())
	}
	if loadTestConfig(t).Profiles["zai2"] == nil {
		t.Error("a declined prompt removed the profile")
	}

	out.Reset()
	if err := profileRemove(&out, profileRmOpts{Name: "zai2"}, path, cfg, creds.Store{Dir: t.TempDir()}, t.TempDir(), strings.NewReader("y\n")); err != nil {
		t.Fatal(err)
	}
	if loadTestConfig(t).Profiles["zai2"] != nil {
		t.Error("an accepted prompt did not remove the profile")
	}

	// --yes skips the prompt entirely.
	path, cfg = writeTestConfig(t, testConfig)
	out.Reset()
	if err := profileRemove(&out, profileRmOpts{Name: "zai2", Yes: true}, path, cfg, creds.Store{Dir: t.TempDir()}, t.TempDir(), strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("--yes still prompted:\n%s", out.String())
	}
}

func TestClaudeProfileDirSafety(t *testing.T) {
	base := t.TempDir()
	p, err := claudeProfileDir(base, "codex")
	if err != nil || p != filepath.Join(base, "codex") {
		t.Fatalf("claudeProfileDir = %q, %v", p, err)
	}
	for _, bad := range []string{"", "..", "Foo", "a b", "serve", filepath.Join(base, "x")} {
		if _, err := claudeProfileDir(base, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestProfileCmdUsage(t *testing.T) {
	path, _ := writeTestConfig(t, testConfig)
	for _, args := range [][]string{nil, {"bogus"}, {"add", "x"}, {"rm"}} {
		if err := profileCmd(loadTestConfig(t), args); err == nil {
			t.Errorf("profileCmd(%v) succeeded", args)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config vanished: %v", err)
	}
}

func TestProfileAddKeepsCRLF(t *testing.T) {
	crlf := strings.ReplaceAll(testConfig, "\n", "\r\n")
	path, cfg := writeTestConfig(t, crlf)
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Like: "zai"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg2 := loadTestConfig(t)
	if cfg2.Profiles["zai3"] == nil {
		t.Fatal("zai3 missing")
	}
	lines := strings.Split(string(b), "\n")
	crlfCount := 0
	for _, l := range lines[:len(lines)-1] {
		if strings.HasSuffix(l, "\r") {
			crlfCount++
		}
	}
	// Every line, the inserted ones included, must keep the file's CRLF.
	if crlfCount != len(lines)-1 {
		t.Errorf("CRLF endings were not preserved: %d of %d lines", crlfCount, len(lines)-1)
	}
}
