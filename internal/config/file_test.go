package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKtav(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const sampleKtav = `
listen.host: 127.0.0.2
listen.port: 61000

providers: [
    {
        name: codex
        type: codex
    }
    {
        name: oai
        type: openai
        api_key_env: MY_OPENAI_KEY
    }
]

profiles: [
    {
        name: work
        provider: codex
        default_model: gpt-5
        models: [
            {
                match: *haiku*
                model: gpt-5-mini
            }
        ]
    }
    {
        name: agents
        provider: oai
    }
]
`

func TestLoadFileKtav(t *testing.T) {
	fc, err := loadFile(writeKtav(t, sampleKtav))
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}
	if fc.Listen.Host != "127.0.0.2" || fc.Listen.Port != 61000 {
		t.Errorf("listen = %+v", fc.Listen)
	}
	if len(fc.Providers) != 2 || fc.Providers[1].APIKeyEnv != "MY_OPENAI_KEY" {
		t.Errorf("providers = %+v", fc.Providers)
	}
	if len(fc.Profiles) != 2 || fc.Profiles[0].Models[0].Match != "*haiku*" {
		t.Errorf("profiles = %+v", fc.Profiles)
	}
}

func TestLoadFileErrors(t *testing.T) {
	provider := "providers: [\n    {\n        name: p\n        type: openai\n    }\n]\n"
	tests := []struct{ name, body, want string }{
		{"bad provider type", "providers: [\n    {\n        name: p\n        type: foo\n    }\n]\n", "type must be"},
		{"duplicate provider", "providers: [\n    {\n        name: p\n        type: openai\n    }\n    {\n        name: p\n        type: codex\n    }\n]\n", "duplicate"},
		{"unknown provider", provider + "profiles: [\n    {\n        name: w\n        provider: nope\n    }\n]\n", "unknown provider"},
		{"rule without model", provider + "profiles: [\n    {\n        name: w\n        provider: p\n        models: [\n            {\n                match: *opus*\n            }\n        ]\n    }\n]\n", "required"},
		{"bad pattern", provider + "profiles: [\n    {\n        name: w\n        provider: p\n        models: [\n            {\n                match: op[us\n                model: x\n            }\n        ]\n    }\n]\n", "bad pattern"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadFile(writeKtav(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadFileMissing(t *testing.T) {
	fc, err := loadFile(filepath.Join(t.TempDir(), "absent.ktav"))
	if err != nil || fc != nil {
		t.Fatalf("loadFile(missing) = %v, %v; want nil, nil", fc, err)
	}
}

func TestResolveModel(t *testing.T) {
	cfg := &Config{
		ModelRules: []ModelRule{
			{Match: "*haiku*", Model: "small"},
			{Match: "claude-opus-4*", Model: "big"},
			{Match: "*opus*", Model: "never"},
		},
	}
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"claude-haiku-4-5", "small", true},
		{"Claude-Opus-4-1", "big", true}, // case-insensitive, first match wins
		{"claude-opus-3", "never", true},
		{"claude-sonnet-4", "", false},
	}
	for _, tt := range tests {
		got, ok := cfg.ResolveModel(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ResolveModel(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}

	cfg.DefaultModel = "fallback"
	if got, ok := cfg.ResolveModel("claude-sonnet-4"); got != "fallback" || !ok {
		t.Errorf("default_model fallback = %q, %v", got, ok)
	}
}

func TestLoadFileMode(t *testing.T) {
	t.Setenv(ConfigFileEnv, writeKtav(t, sampleKtav))
	t.Setenv("PORT", "")
	t.Setenv("OPENAI_API_KEY", "") // not required in file mode

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IsEnvMode() || cfg.Port != "61000" {
		t.Errorf("env mode = %v, port = %q", cfg.IsEnvMode(), cfg.Port)
	}
	if cfg.Providers["oai"].BaseURL != "https://api.openai.com/v1" {
		t.Errorf("default base_url = %q", cfg.Providers["oai"].BaseURL)
	}

	w, err := cfg.ForProfile("work", "")
	if err != nil {
		t.Fatal(err)
	}
	if w.ProviderType != ProviderTypeCodex || w.DefaultModel != "gpt-5" || len(w.ModelRules) != 1 {
		t.Errorf("work = %+v", w)
	}
	a, err := cfg.ForProfile("agents", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if a.ProviderType != ProviderTypeOpenAI || a.OpenAIAPIKey != "sk-test" || a.OpenAIBaseURL != "https://api.openai.com/v1" {
		t.Errorf("agents = %+v", a)
	}
	if _, err := cfg.ForProfile("nope", ""); err == nil {
		t.Error("ForProfile(unknown) succeeded")
	}
}

func TestLoadEnvMode(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IsEnvMode() || cfg.Profile != EnvProfile || cfg.Profiles[EnvProfile] == nil {
		t.Errorf("env mode cfg = %+v", cfg)
	}
}

func TestLoadExplicitMissingFile(t *testing.T) {
	t.Setenv(ConfigFileEnv, filepath.Join(t.TempDir(), "absent.ktav"))
	t.Setenv("OPENAI_API_KEY", "test-key")
	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded with missing explicit config")
	}
}
