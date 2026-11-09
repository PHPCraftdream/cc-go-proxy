package config

import (
	"os"
	"path/filepath"
	"testing"
)

func setupCodexTransportLoad(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(ConfigFileEnv, "")
	t.Setenv("PROXY_TOKEN", testToken)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("CODEX_TRANSPORT", "")
	return dir
}

func TestCodexTransportDefault(t *testing.T) {
	setupCodexTransportLoad(t)
	for _, tt := range []struct {
		name  string
		unset bool
	}{{"absent", true}, {"empty", false}} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.unset {
				os.Unsetenv("CODEX_TRANSPORT")
			} else {
				t.Setenv("CODEX_TRANSPORT", "")
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CodexTransport != "http" || cfg.CodexWebsocket() {
				t.Errorf("transport = %q, websocket = %v; want http, false", cfg.CodexTransport, cfg.CodexWebsocket())
			}
		})
	}
}

func TestCodexTransportAcceptedValues(t *testing.T) {
	setupCodexTransportLoad(t)
	for _, tt := range []struct {
		value string
		want  string
	}{{"http", "http"}, {"HTTP", "http"}, {"websocket", "websocket"}, {"WEBSOCKET", "websocket"}, {"ws", "websocket"}, {"Ws", "websocket"}} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("CODEX_TRANSPORT", tt.value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CodexTransport != tt.want || cfg.CodexWebsocket() != (tt.want == "websocket") {
				t.Errorf("transport = %q, websocket = %v; want %q", cfg.CodexTransport, cfg.CodexWebsocket(), tt.want)
			}
		})
	}
}

func TestCodexTransportInvalidValue(t *testing.T) {
	setupCodexTransportLoad(t)
	for _, value := range []string{"not-a-transport", " ws ", " websocket ", "   "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CODEX_TRANSPORT", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CodexTransport != "http" || cfg.CodexWebsocket() {
				t.Errorf("transport = %q, websocket = %v; want http, false", cfg.CodexTransport, cfg.CodexWebsocket())
			}
		})
	}
}

func TestCodexTransportReload(t *testing.T) {
	setupCodexTransportLoad(t)
	for _, value := range []string{"ws", "http", "WEBsocket"} {
		t.Setenv("CODEX_TRANSPORT", value)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load with CODEX_TRANSPORT=%q: %v", value, err)
		}
		want := value != "http"
		if cfg.CodexWebsocket() != want {
			t.Errorf("CODEX_TRANSPORT=%q: CodexWebsocket() = %v, want %v", value, cfg.CodexWebsocket(), want)
		}
	}
}

func TestCodexTransportEnvFiles(t *testing.T) {
	for _, tt := range []struct {
		name string
		path func(string) string
	}{
		{"local env", func(string) string { return ".env" }},
		{"proxy env", func(dir string) string { return filepath.Join(dir, ".claude", "proxy.env") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupCodexTransportLoad(t)
			file := tt.path(dir)
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("CODEX_TRANSPORT=ws\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CodexTransport != "websocket" || !cfg.CodexWebsocket() {
				t.Errorf("transport = %q, websocket = %v; want websocket, true", cfg.CodexTransport, cfg.CodexWebsocket())
			}
		})
	}
}

func TestCodexTransportKtavMode(t *testing.T) {
	setupCodexTransportLoad(t)
	t.Setenv("CCGP_CONFIG", writeKtav(t, "providers: [\n    {\n        name: work\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: work\n        provider: work\n    }\n]\n"))
	t.Setenv("CODEX_TRANSPORT", "websocket")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	profile, err := cfg.ForProfile("work", "")
	if err != nil {
		t.Fatalf("ForProfile: %v", err)
	}
	if profile.CodexTransport != "websocket" || !profile.CodexWebsocket() {
		t.Errorf("profile transport = %q, websocket = %v; want websocket, true", profile.CodexTransport, profile.CodexWebsocket())
	}
}
