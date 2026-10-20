package config

import (
	"os"
	"testing"
)

func TestDefaultTemplateIsValid(t *testing.T) {
	fc, err := loadFile(writeKtav(t, string(DefaultTemplate())))
	if err != nil {
		t.Fatalf("default template invalid: %v", err)
	}
	if len(fc.Providers) != 2 || len(fc.Profiles) != 2 {
		t.Errorf("providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
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
