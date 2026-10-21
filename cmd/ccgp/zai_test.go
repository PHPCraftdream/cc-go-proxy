package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/preset"
)

func loadTemplate(t *testing.T) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, config.DefaultTemplate(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCGP_CONFIG", p)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The generated z.ai block in the default config must be exactly the preset:
// Fable -> glm-5.3, everything else -> glm-5.3-flash, 1M windows.
func TestTemplateZAIMatchesPreset(t *testing.T) {
	cfg := loadTemplate(t)
	pf := cfg.Profiles["zai"]
	if pf == nil || pf.Provider != "zai" {
		t.Fatalf("zai profile = %+v", pf)
	}
	if !reflect.DeepEqual(pf.Models, preset.ZAI.Rules) {
		t.Errorf("template rules\n%+v\nare not the preset\n%+v", pf.Models, preset.ZAI.Rules)
	}
	if got := pf.ContextWindowFor("sonnet"); got != 1_000_000 {
		t.Errorf("window for sonnet = %d", got)
	}
	pr := cfg.Providers["zai"]
	if pr.Type != config.ProviderTypeAnthropicCompat || !reflect.DeepEqual(pr.EffortMap, preset.ZAI.EffortMap) {
		t.Errorf("zai provider = %+v, want the preset's effort map %v", pr, preset.ZAI.EffortMap)
	}
	if r, _ := cfg.ForProfile("zai", "key"); r == nil || r.ProviderType != config.ProviderTypeAnthropicCompat || r.OpenAIAPIKey != "key" {
		t.Errorf("ForProfile(zai) = %+v", r)
	}
	if m, ok := (&config.Config{ModelRules: pf.Models}).ResolveModel("claude-opus-5-5"); !ok || m != "glm-5.3-flash" {
		t.Errorf("opus -> %q", m)
	}
	if m, _ := (&config.Config{ModelRules: pf.Models}).ResolveModel("claude-fable-5-1"); m != "glm-5.3" {
		t.Errorf("fable -> %q", m)
	}
}

// Generated mappings exist for codex profiles and for anthropic_compat
// providers that have a preset; other profiles are skipped.
func TestManagedProfiles(t *testing.T) {
	cfg := loadTemplate(t)
	got, err := managedProfiles(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if !reflect.DeepEqual(names, []string{"codex", "zai"}) {
		t.Errorf("managed profiles = %v, want [codex zai] (codex_api has no generator)", names)
	}
	if one, err := managedProfiles(cfg, []string{"zai"}); err != nil || len(one) != 1 || one[0].Name != "zai" {
		t.Errorf("only zai = %v, %v", one, err)
	}
	if _, err := managedProfiles(cfg, []string{"codex_api"}); err == nil {
		t.Error("codex_api accepted as a generated profile")
	}

	// An anthropic_compat provider without a preset is not generated.
	cfg.Providers["zai"].BaseURL = "https://example.com/anthropic"
	got, _ = managedProfiles(cfg, nil)
	for _, p := range got {
		if p.Name == "zai" {
			t.Error("provider without a preset was offered")
		}
	}
}

func TestConfigCmdUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"reset", "extra"}} {
		if err := configCmd(args); err == nil {
			t.Errorf("configCmd(%v) succeeded", args)
		}
	}
}
