package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
)

func TestDefaultTemplateIsValid(t *testing.T) {
	fc, err := loadFile(writeKtav(t, string(DefaultTemplate())))
	if err != nil {
		t.Fatalf("default template invalid: %v", err)
	}
	if len(fc.Providers) != 3 || len(fc.Profiles) != 3 {
		t.Errorf("providers=%d profiles=%d", len(fc.Providers), len(fc.Profiles))
	}
	if fc.Listen.MaxBodyMB != DefaultMaxBodyMB {
		t.Errorf("template listen.max_body_mb = %d, want %d", fc.Listen.MaxBodyMB, DefaultMaxBodyMB)
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

// A setup in its own home directory gets a port of its own, the default one
// keeps 60093: a server is found by its port.
func TestNewConfigInAHomeGetsItsOwnPort(t *testing.T) {
	t.Setenv(home.EnvVar, "")
	if got := newConfig(); string(got) != string(DefaultTemplate()) {
		t.Error("the default layout must get the template as it is")
	}
	dir := t.TempDir()
	t.Setenv(home.EnvVar, dir)
	t.Setenv(ConfigFileEnv, "")
	t.Setenv("PROXY_TOKEN", "")
	got := string(newConfig())
	if strings.Contains(got, "listen.port: "+DefaultPort+"\n") {
		t.Errorf("the home kept the default port")
	}
	i := strings.Index(got, "listen.port: ")
	if i < 0 {
		t.Fatal("no listen.port")
	}
	port := strings.Fields(got[i+len("listen.port: "):])[0]
	if n, err := strconv.Atoi(port); err != nil || n < 1024 || n > 65535 {
		t.Errorf("port %q", port)
	}
	if strings.Replace(got, port, DefaultPort, 1) != string(DefaultTemplate()) {
		t.Error("only the port may differ from the template")
	}

	path, created, err := EnsureFile()
	if err != nil || !created || path != filepath.Join(dir, "config.ktav") {
		t.Fatalf("EnsureFile = %q, %v, %v", path, created, err)
	}
	if _, err := loadFile(path); err != nil {
		t.Errorf("the config of a home does not load: %v", err)
	}
}
