package config

import (
	"os"
	"strings"
	"testing"
)

const codexProviderKtav = "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\n"

// The template carries the built-in version, and it reads back as a string.
func TestTemplateCodexClientVersion(t *testing.T) {
	fc, err := loadFile(writeKtav(t, string(DefaultTemplate())))
	if err != nil {
		t.Fatal(err)
	}
	if fc.Codex.ClientVersion != DefaultCodexClientVersion {
		t.Errorf("template codex.client_version = %q, want %q", fc.Codex.ClientVersion, DefaultCodexClientVersion)
	}
	if CompareClientVersions(DefaultCodexClientVersion, "0.169.0") < 0 {
		t.Errorf("default %s is older than 0.169.0, which brought gpt-6.2", DefaultCodexClientVersion)
	}
}

func TestCodexClientVersionLoad(t *testing.T) {
	t.Setenv(ConfigFileEnv, writeKtav(t, "codex.client_version:: 0.171.2\n"+codexProviderKtav))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CodexClientVersion(); got != "0.171.2" {
		t.Errorf("CodexClientVersion = %q", got)
	}
	if got := (&Config{}).CodexClientVersion(); got != DefaultCodexClientVersion {
		t.Errorf("unset CodexClientVersion = %q, want the default", got)
	}

	t.Setenv(ConfigFileEnv, writeKtav(t, "codex.client_version:: 0.171\n"+codexProviderKtav))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MAJOR.MINOR.PATCH") {
		t.Errorf("bad version: err = %v", err)
	}
}

func TestCompareClientVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"0.169.0", "0.169.0", 0},
		{"0.170.0", "0.169.9", 1},
		{"0.99.0", "0.100.0", -1},
		{"1.0.0", "0.999.999", 1},
	} {
		if got := CompareClientVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestSetCodexClientVersionText(t *testing.T) {
	tests := []struct {
		name, in, v, want string
	}{
		{"added after connections", "listen.port: 1\nconnections.total: 5\n" + codexProviderKtav, "0.170.0",
			"listen.port: 1\nconnections.total: 5\ncodex.client_version:: 0.170.0\n" + codexProviderKtav},
		{"added on top", "## c\n" + codexProviderKtav, "0.170.0", "## c\ncodex.client_version:: 0.170.0\n" + codexProviderKtav},
		{"raised", "codex.client_version:: 0.169.0\n" + codexProviderKtav, "0.170.0", "codex.client_version:: 0.170.0\n" + codexProviderKtav},
		{"never lowered", "codex.client_version:: 0.171.0\n" + codexProviderKtav, "0.170.0", "codex.client_version:: 0.171.0\n" + codexProviderKtav},
		{"same", "codex.client_version:: 0.170.0\n" + codexProviderKtav, "0.170.0", "codex.client_version:: 0.170.0\n" + codexProviderKtav},
		{"single colon", "codex.client_version: 0.169.0\r\n", "0.170.0", "codex.client_version:: 0.170.0\r\n"},
		{"crlf", "listen.port: 1\r\nx: y\r\n", "0.170.0", "listen.port: 1\r\ncodex.client_version:: 0.170.0\r\nx: y\r\n"},
	}
	for _, tt := range tests {
		got, err := SetCodexClientVersionText(tt.in, tt.v)
		if err != nil || got != tt.want {
			t.Errorf("%s: got %q, %v\nwant %q", tt.name, got, err, tt.want)
		}
	}
	if _, err := SetCodexClientVersionText(codexProviderKtav, "0.170"); err == nil {
		t.Error("an invalid version was accepted")
	}
	if _, err := SetCodexClientVersionText("codex: {\n    client_version:: 0.1.0\n}\n", "0.170.0"); err == nil {
		t.Error("a codex block was edited")
	}
}

func TestRaiseAndSeedCodexClientVersion(t *testing.T) {
	path := writeKtav(t, "connections.total: 10\n"+codexProviderKtav)
	t.Setenv(ConfigFileEnv, path)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if seeded, err := SeedCodexClientVersion(cfg); err != nil || !seeded {
		t.Fatalf("seed = %v, %v", seeded, err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "codex.client_version:: "+DefaultCodexClientVersion) {
		t.Fatalf("seeded config:\n%s", b)
	}
	if cfg, err = Load(); err != nil || cfg.CodexClientVersionSet != DefaultCodexClientVersion {
		t.Fatalf("reloaded %q, %v", cfg.CodexClientVersionSet, err)
	}
	if seeded, err := SeedCodexClientVersion(cfg); err != nil || seeded {
		t.Errorf("second seed = %v, %v; want a no-op", seeded, err)
	}

	from, changed, err := RaiseCodexClientVersion(path, "0.200.1")
	if err != nil || !changed || from != DefaultCodexClientVersion {
		t.Fatalf("raise = %q, %v, %v", from, changed, err)
	}
	if from, changed, err = RaiseCodexClientVersion(path, "0.170.0"); err != nil || changed || from != "0.200.1" {
		t.Errorf("lowering = %q, %v, %v; want no change", from, changed, err)
	}

	// No codex provider, or no config file: nothing is written.
	other := writeKtav(t, "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n]\n")
	t.Setenv(ConfigFileEnv, other)
	if cfg, err = Load(); err != nil {
		t.Fatal(err)
	}
	if seeded, err := SeedCodexClientVersion(cfg); err != nil || seeded {
		t.Errorf("seed without a codex provider = %v, %v", seeded, err)
	}
	if seeded, err := SeedCodexClientVersion(&Config{}); err != nil || seeded {
		t.Errorf("seed in env-only mode = %v, %v", seeded, err)
	}
}
