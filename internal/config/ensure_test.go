package config

import (
	"os"
	"strings"
	"testing"
)

func TestDefaultTemplateIsValid(t *testing.T) {
	fc, err := loadFile(writeKtav(t, string(DefaultTemplate())))
	if err != nil {
		t.Fatalf("default template invalid: %v", err)
	}
	if len(fc.Providers) != 3 || len(fc.Profiles) != 3 {
		t.Errorf("providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}
	var zai *Provider
	for i := range fc.Providers {
		if fc.Providers[i].Name == "zai" {
			zai = &fc.Providers[i]
		}
	}
	if zai == nil || zai.Type != ProviderTypeAnthropicCompat || zai.BaseURL != "https://api.z.ai/api/anthropic" ||
		zai.EffortMap["low"] != "low" || zai.EffortMap["medium"] != "high" || zai.EffortMap["xhigh"] != "max" || zai.EffortMap["max"] != "max" {
		t.Errorf("zai provider = %+v", zai)
	}
}

func TestEnsureFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv(ConfigFileEnv, "")
	t.Setenv("PROXY_TOKEN", "")

	p, created, err := EnsureFile()
	if err != nil || !created {
		t.Fatalf("first call = %q, %v, %v", p, created, err)
	}
	if _, err := loadFile(p); err != nil {
		t.Fatalf("created file does not load: %v", err)
	}

	// Never overwrites user edits.
	if err := os.WriteFile(p, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := EnsureFile(); err != nil || created {
		t.Fatalf("second call created=%v err=%v", created, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "# mine\n" {
		t.Errorf("file overwritten: %q", b)
	}
}

func TestEnsureFileSkippedByModeEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)

	t.Setenv("PROXY_TOKEN", "x")
	if p, created, err := EnsureFile(); p != "" || created || err != nil {
		t.Errorf("PROXY_TOKEN: %q %v %v", p, created, err)
	}
	t.Setenv("PROXY_TOKEN", "")
	t.Setenv(ConfigFileEnv, dir+"/custom.ktav")
	if p, created, err := EnsureFile(); p != "" || created || err != nil {
		t.Errorf("CCGP_CONFIG: %q %v %v", p, created, err)
	}
}

func TestResetFileBacksUpAndRegenerates(t *testing.T) {
	dir := t.TempDir()
	cfg := dir + "/config.ktav"
	t.Setenv(ConfigFileEnv, cfg)

	// No config yet: just written, no backup.
	path, backup, err := ResetFile()
	if err != nil || path != cfg || backup != "" {
		t.Fatalf("first reset = %q, %q, %v", path, backup, err)
	}
	if _, err := loadFile(path); err != nil {
		t.Fatalf("regenerated config invalid: %v", err)
	}

	// An edited config is kept as a backup before being replaced.
	if err := os.WriteFile(cfg, []byte("# my edits\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, backup, err = ResetFile()
	if err != nil || backup == "" || !strings.HasPrefix(backup, cfg+".bak-") {
		t.Fatalf("second reset = %q, %q, %v", path, backup, err)
	}
	if b, _ := os.ReadFile(backup); string(b) != "# my edits\n" {
		t.Errorf("backup = %q", b)
	}
	if b, _ := os.ReadFile(cfg); string(b) != string(DefaultTemplate()) {
		t.Error("config is not the default template after reset")
	}
}
