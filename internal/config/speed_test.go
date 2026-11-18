package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpeedRulesRequireCodex(t *testing.T) {
	for _, provider := range []string{ProviderTypeCodexAPI, ProviderTypeAnthropicCompat, ProviderTypeClaude, ProviderTypeClaudeAPI} {
		t.Run(provider, func(t *testing.T) {
			baseURL := ""
			if provider == ProviderTypeAnthropicCompat {
				baseURL = "        base_url: https://example.com\n"
			}
			text := "providers: [\n    {\n        name: p\n        type: " + provider + "\n" + baseURL + "    }\n]\nprofiles: [\n    {\n        name: p\n        provider: p\n    }\n]\n"
			p := filepath.Join(t.TempDir(), "config.ktav")
			if err := os.WriteFile(p, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadFile(p); err != nil {
				t.Fatal(err)
			}
			_, err := EditFile(p, func(s string) (string, error) { return SetProfileSpeedText(s, "p", "astra", SpeedFast) })
			if err == nil || !strings.Contains(err.Error(), "speed_rules require a codex provider") {
				t.Fatalf("unexpected error: %v", err)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != text {
				t.Fatal("invalid edit changed file")
			}
			invalid, err := SetProfileSpeedText(text, "p", "astra", SpeedFast)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(invalid), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadFile(p); err == nil || !strings.Contains(err.Error(), "speed_rules require a codex provider") {
				t.Fatalf("unexpected load error: %v", err)
			}
		})
	}
}

func TestProfileSpeedRules(t *testing.T) {
	base := "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: one\n        provider: codex\n        ## ccgp:managed-models begin\n        models: [\n        ]\n        ## ccgp:managed-models end\n    }\n    {\n        name: two\n        provider: codex\n    }\n]\n"
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	edit := func(profile, family, speed string) string {
		t.Helper()
		backup, err := EditFile(p, func(s string) (string, error) { return SetProfileSpeedText(s, profile, family, speed) })
		if err != nil {
			t.Fatal(err)
		}
		return backup
	}
	if edit("one", "astra", "fast") == "" {
		t.Fatal("missing backup")
	}
	edit("one", "sol", "standard")
	edit("two", "astra", "standard")
	fc, err := loadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Profiles[0].SpeedFor("astra") != SpeedFast || fc.Profiles[0].SpeedFor("sol") != SpeedStandard || fc.Profiles[1].SpeedFor("astra") != SpeedStandard || fc.Profiles[1].SpeedFor("sol") != "" {
		t.Fatal("incorrect profile rules")
	}
	if !fc.Profiles[0].EffectiveFast("astra", false) || fc.Profiles[0].EffectiveFast("sol", true) || !fc.Profiles[1].EffectiveFast("sol", true) || fc.Profiles[1].EffectiveFast("sol", false) {
		t.Fatal("incorrect effective speed")
	}
	edit("one", "astra", "")
	if backup := edit("one", "astra", ""); backup != "" {
		t.Fatal("no-op made backup")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "## ccgp:managed-models begin\n        models: [\n        ]\n        ## ccgp:managed-models end") {
		t.Fatal("managed block changed")
	}
	fc, err = loadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Profiles[0].SpeedFor("sol") != SpeedStandard || fc.Profiles[1].SpeedFor("astra") != SpeedStandard {
		t.Fatal("unrelated rule changed")
	}
	edit("one", "sol", "")
	if _, err := loadFile(p); err != nil {
		t.Fatalf("last rule removal produced invalid config: %v", err)
	}
	if backup := edit("one", "sol", ""); backup != "" {
		t.Fatal("repeated last-rule unset made backup")
	}
	if _, err := SetProfileSpeedText(base, "one", "astra", "turbo"); err == nil {
		t.Fatal("accepted invalid speed")
	}
}
