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
        type: codex_api
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
	provider := "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n]\n"
	tests := []struct{ name, body, want string }{
		{"bad provider type", "providers: [\n    {\n        name: p\n        type: foo\n    }\n]\n", "type must be"},
		{"duplicate provider", "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n    {\n        name: p\n        type: codex\n    }\n]\n", "duplicate"},
		{"unknown provider", provider + "profiles: [\n    {\n        name: w\n        provider: nope\n    }\n]\n", "unknown provider"},
		{"rule without model", provider + "profiles: [\n    {\n        name: w\n        provider: p\n        models: [\n            {\n                match: *opus*\n            }\n        ]\n    }\n]\n", "required"},
		{"claude is a send endpoint", provider + "profiles: [\n    {\n        name: claude\n        provider: p\n    }\n]\n", "reserved profile name"},
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

func TestMaxBodyMB(t *testing.T) {
	provider := "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n]\n"
	t.Setenv(ConfigFileEnv, writeKtav(t, "listen.max_body_mb: 64\n"+provider))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxBodyMB != 64 || cfg.MaxBodyBytes() != 64<<20 {
		t.Errorf("MaxBodyMB = %d, bytes = %d; want 64 MiB", cfg.MaxBodyMB, cfg.MaxBodyBytes())
	}

	// Absent key (an older config) and zero both fall back to the default.
	t.Setenv(ConfigFileEnv, writeKtav(t, provider))
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxBodyBytes() != DefaultMaxBodyMB<<20 {
		t.Errorf("default bytes = %d, want %d MiB", cfg.MaxBodyBytes(), DefaultMaxBodyMB)
	}
	if got := (&Config{}).MaxBodyBytes(); got != DefaultMaxBodyMB<<20 {
		t.Errorf("zero Config bytes = %d, want default", got)
	}

	for _, bad := range []int64{-1, 2048} {
		var fc fileConfig
		fc.Listen.MaxBodyMB = bad
		if err := fc.validate(); err == nil || !strings.Contains(err.Error(), "listen.max_body_mb") {
			t.Errorf("max_body_mb %d: err = %v, want a max_body_mb error", bad, err)
		}
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
	if a.ProviderType != ProviderTypeCodexAPI || a.OpenAIAPIKey != "sk-test" || a.OpenAIBaseURL != "https://api.openai.com/v1" {
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

func TestLegacyOpenAIProviderType(t *testing.T) {
	fc, err := loadFile(writeKtav(t, "providers: [\n    {\n        name: old\n        type: openai\n    }\n]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if fc.Providers[0].Type != ProviderTypeCodexAPI {
		t.Errorf("type = %q, want %q", fc.Providers[0].Type, ProviderTypeCodexAPI)
	}
	if _, err := loadFile(writeKtav(t, "providers: [\n    {\n        name: x\n        type: nope\n    }\n]\n")); err == nil ||
		!strings.Contains(err.Error(), ProviderTypeCodexAPI) {
		t.Errorf("unknown type error = %v", err)
	}
}

func TestRuleContextWindowAndEffective(t *testing.T) {
	cfg := "providers: [\n    {\n        name: p\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: w\n        provider: p\n        models: [\n" +
		"            {\n                match: *a*\n                model: m1\n                context_window: 500000\n            }\n" +
		"            {\n                match: *b*\n                model: m2\n                context_window: 272000\n            }\n" +
		"            {\n                match: *\n                model: m3\n            }\n" +
		"        ]\n    }\n]\n"
	fc, err := loadFile(writeKtav(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	pf := fc.Profiles[0]
	if pf.Models[0].ContextWindow != 500000 || pf.Models[2].ContextWindow != 0 {
		t.Errorf("rule windows = %+v", pf.Models)
	}
	// Smallest known rule window; rules without one are ignored.
	if got := pf.EffectiveContextWindow(); got != 272000 {
		t.Errorf("EffectiveContextWindow = %d, want 272000", got)
	}
	// A profile-level value overrides the derived one.
	pf.ContextWindow = 123000
	if got := pf.EffectiveContextWindow(); got != 123000 {
		t.Errorf("override: EffectiveContextWindow = %d", got)
	}
	var none *Profile
	if none.EffectiveContextWindow() != 0 || (&Profile{}).EffectiveContextWindow() != 0 {
		t.Error("empty profile must have no window")
	}

	bad := strings.Replace(cfg, "context_window: 500000", "context_window: -1", 1)
	if _, err := loadFile(writeKtav(t, bad)); err == nil || !strings.Contains(err.Error(), "context_window") {
		t.Errorf("negative rule window accepted: %v", err)
	}
}

func TestContextWindowFor(t *testing.T) {
	pf := &Profile{Models: []ModelRule{
		{Match: "*opus*", Model: "sol", ContextWindow: 872000},
		{Match: "*haiku*", Model: "old", ContextWindow: 272000},
		{Match: "*", Model: "luna"}, // catch-all without a window
	}}
	tests := map[string]int64{
		"opus":              872000,
		"Claude-Opus-5-5":   872000, // case-insensitive
		"haiku":             272000,
		"something-else":    272000, // matched rule has no window: smallest known
		"claude-sonnet-5-5": 272000,
	}
	for model, want := range tests {
		if got := pf.ContextWindowFor(model); got != want {
			t.Errorf("ContextWindowFor(%q) = %d, want %d", model, got, want)
		}
	}
	pf.ContextWindow = 123000
	if got := pf.ContextWindowFor("opus"); got != 123000 {
		t.Errorf("override ignored: %d", got)
	}
	var none *Profile
	if none.ContextWindowFor("x") != 0 {
		t.Error("nil profile")
	}
}

func TestAnthropicCompatProvider(t *testing.T) {
	ok := "providers: [\n    {\n        name: z\n        type: anthropic_compat\n        base_url: https://api.z.ai/api/anthropic\n        effort_map: {\n            medium: high\n            xhigh: max\n        }\n    }\n]\n"
	fc, err := loadFile(writeKtav(t, ok))
	if err != nil {
		t.Fatal(err)
	}
	if p := fc.Providers[0]; p.Type != ProviderTypeAnthropicCompat || p.EffortMap["medium"] != "high" || p.EffortMap["xhigh"] != "max" {
		t.Errorf("provider = %+v", p)
	}
	noURL := "providers: [\n    {\n        name: z\n        type: anthropic_compat\n    }\n]\n"
	if _, err := loadFile(writeKtav(t, noURL)); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("anthropic_compat without base_url accepted: %v", err)
	}
}
