package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
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
	fc, err := LoadFile(writeKtav(t, sampleKtav))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
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

func TestLoadFileClaudeProfile(t *testing.T) {
	fc, err := LoadFile(writeKtav(t, "providers: [\n    {\n        name: work\n        type: claude\n    }\n]\nprofiles: [\n    {\n        name: work\n        provider: work\n    }\n]\n"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(fc.Providers) != 1 || fc.Providers[0].Type != ProviderTypeClaude || len(fc.Profiles) != 1 {
		t.Errorf("config = %+v", fc)
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
		{"claude provider with a url", "providers: [\n    {\n        name: c\n        type: claude\n        base_url: https://x\n    }\n]\n", "takes no base_url"},
		{"claude profile with models", "providers: [\n    {\n        name: c\n        type: claude\n    }\n]\nprofiles: [\n    {\n        name: c\n        provider: c\n        models: [\n            {\n                match: *\n                model: m\n            }\n        ]\n    }\n]\n", "does not go through ccgp"},
		{"claude profile with a proxy", "providers: [\n    {\n        name: c\n        type: claude\n    }\n]\nprofiles: [\n    {\n        name: c\n        provider: c\n        proxy: none\n    }\n]\n", "does not go through ccgp"},
		{"bad pattern", provider + "profiles: [\n    {\n        name: w\n        provider: p\n        models: [\n            {\n                match: op[us\n                model: x\n            }\n        ]\n    }\n]\n", "bad pattern"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFile(writeKtav(t, tt.body))
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
	fc, err := LoadFile(filepath.Join(t.TempDir(), "absent.ktav"))
	if err != nil || fc != nil {
		t.Fatalf("LoadFile(missing) = %v, %v; want nil, nil", fc, err)
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
	fc, err := LoadFile(writeKtav(t, "providers: [\n    {\n        name: old\n        type: openai\n    }\n]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if fc.Providers[0].Type != ProviderTypeCodexAPI {
		t.Errorf("type = %q, want %q", fc.Providers[0].Type, ProviderTypeCodexAPI)
	}
	if _, err := LoadFile(writeKtav(t, "providers: [\n    {\n        name: x\n        type: nope\n    }\n]\n")); err == nil ||
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
	fc, err := LoadFile(writeKtav(t, cfg))
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
	if _, err := LoadFile(writeKtav(t, bad)); err == nil || !strings.Contains(err.Error(), "context_window") {
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
	fc, err := LoadFile(writeKtav(t, ok))
	if err != nil {
		t.Fatal(err)
	}
	if p := fc.Providers[0]; p.Type != ProviderTypeAnthropicCompat || p.EffortMap["medium"] != "high" || p.EffortMap["xhigh"] != "max" {
		t.Errorf("provider = %+v", p)
	}
	noURL := "providers: [\n    {\n        name: z\n        type: anthropic_compat\n    }\n]\n"
	if _, err := LoadFile(writeKtav(t, noURL)); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("anthropic_compat without base_url accepted: %v", err)
	}
}

func TestLoadRejectsBadPeakHours(t *testing.T) {
	for name, extra := range map[string]string{
		"bad time":     "        peak: [\n            {\n                from: 25:00\n                to: 26:00\n            }\n        ]\n",
		"bad day":      "        peak: [\n            {\n                days: funday\n                from: 14:00\n                to: 18:00\n            }\n        ]\n",
		"empty window": "        peak: [\n            {\n                from: 14:00\n                to: 14:00\n            }\n        ]\n",
		"zone alone":   "        peak_tz: Asia/Shanghai\n",
		"bad zone":     "        peak_tz: Mars/Base\n        peak: [\n            {\n                from: 14:00\n                to: 18:00\n            }\n        ]\n",
	} {
		text := strings.Replace(peakBase, "        base_url: https://api.z.ai/api/anthropic\n", "        base_url: https://api.z.ai/api/anthropic\n"+extra, 1)
		if _, err := LoadFile(writeKtav(t, text)); err == nil || !strings.Contains(err.Error(), `provider "zai"`) {
			t.Errorf("%s: LoadFile = %v, want an error naming the provider", name, err)
		}
	}
	claude := "providers: [\n    {\n        name: work\n        type: claude\n        peak: [\n            {\n                from: 14:00\n                to: 18:00\n            }\n        ]\n    }\n]\nprofiles: [\n    {\n        name: work\n        provider: work\n    }\n]\n"
	if _, err := LoadFile(writeKtav(t, claude)); err == nil {
		t.Error("a claude provider with peak hours was accepted")
	}
}

const peakBase = `## config
providers: [
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
        effort_map: {
            low: low
        }
    }
    {
        name: codex
        type: codex
    }
]
profiles: [
    {
        name: zai
        provider: zai
    }
    {
        name: codex
        provider: codex
    }
]
`

func TestLoadPeakHours(t *testing.T) {
	text := strings.Replace(peakBase, "        base_url: https://api.z.ai/api/anthropic\n", `        base_url: https://api.z.ai/api/anthropic
        peak_tz: Asia/Shanghai
        peak: [
            {
                days: mon-fri
                from: 14:00
                to: 18:00
            }
            {
                from: 20:00
                to: 24:00
            }
        ]
`, 1)
	fc := mustLoad(t, text)
	zai := providerPtr(fc, "zai")
	want := []peak.Window{{Days: "mon-fri", From: "14:00", To: "18:00"}, {From: "20:00", To: "24:00"}}
	if zai.PeakTZ != "Asia/Shanghai" || !reflect.DeepEqual(zai.Peak, want) {
		t.Errorf("zai peak = %+v in %q", zai.Peak, zai.PeakTZ)
	}
	if s, err := zai.PeakSchedule(); err != nil || s == nil {
		t.Errorf("Schedule = %v, %v", s, err)
	}
	if codex := providerPtr(fc, "codex"); len(codex.Peak) != 0 {
		t.Errorf("codex peak = %+v", codex.Peak)
	}
}

// mustLoad loads text through the real loader or fails the test. The text is
// written as-is (CRLF stays CRLF): the loader accepts CRLF files.
func mustLoad(t *testing.T, text string) *fileConfig {
	t.Helper()
	fc, err := LoadFile(writeKtav(t, text))
	if err != nil {
		t.Fatalf("result does not load: %v\n%s", err, text)
	}
	return fc
}

func providerPtr(fc *fileConfig, name string) *Provider {
	for i := range fc.Providers {
		if fc.Providers[i].Name == name {
			return &fc.Providers[i]
		}
	}
	return nil
}

func TestConnectionLimits(t *testing.T) {
	provider := "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n]\n"
	t.Setenv(ConfigFileEnv, writeKtav(t, "connections.active: 3\nconnections.total: 5\n"+provider))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a, n := cfg.ConnectionLimits(); a != 3 || n != 5 {
		t.Errorf("limits = %d/%d, want 3/5", a, n)
	}

	// Absent keys (an older config) fall back to the defaults.
	t.Setenv(ConfigFileEnv, writeKtav(t, provider))
	if cfg, err = Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a, n := cfg.ConnectionLimits(); a != DefaultConnectionsActive || n != DefaultConnectionsTotal {
		t.Errorf("default limits = %d/%d", a, n)
	}
	if a, n := (&Config{}).ConnectionLimits(); a != 7 || n != 10 {
		t.Errorf("zero Config limits = %d/%d, want 7/10", a, n)
	}
}

func TestConnectionsValidation(t *testing.T) {
	tests := []struct {
		active, total int64
		want          string
	}{
		{-1, 0, "connections.active must be"},
		{65, 70, "connections.active must be"},
		{0, -1, "connections.total must be"},
		{8, 6, "must not be below"},
		{12, 0, "must not be below"}, // the default total is 10
	}
	for _, tt := range tests {
		var fc fileConfig
		fc.Connections.Active, fc.Connections.Total = tt.active, tt.total
		if err := fc.validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%d/%d: err = %v, want %q", tt.active, tt.total, err, tt.want)
		}
	}
	var fc fileConfig
	fc.Connections.Active, fc.Connections.Total = 10, 10
	if err := fc.validate(); err != nil {
		t.Errorf("10/10: %v", err)
	}
}

func TestConnectionsProxy(t *testing.T) {
	provider := "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n]\n"
	t.Setenv(ConfigFileEnv, writeKtav(t, "connections.proxy: socks5://me:pw@127.0.0.1:1080\n"+provider))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if u := cfg.ProxyURL(); u == nil || u.Scheme != "socks5" || u.Host != "127.0.0.1:1080" || u.User.Username() != "me" {
		t.Errorf("ProxyURL = %v", u)
	}
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	if d := proxydial.Default(); d == nil || d.Host != "127.0.0.1:1080" {
		t.Errorf("Load did not make the proxy the process default: %v", d)
	}
	if (&Config{}).ProxyURL() != nil {
		t.Error("no proxy configured, but ProxyURL is set")
	}
	for _, bad := range []string{"ftp://proxy:21", "socks5://proxy", "http://proxy:3128/x"} {
		var fc fileConfig
		fc.Connections.Proxy = bad
		if err := fc.validate(); err == nil || !strings.Contains(err.Error(), "connections.proxy") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
}

const cascadeConfig = `connections.proxy: socks5://127.0.0.1:1080

providers: [
    {
        name: shared
        type: codex
    }
    {
        name: other
        type: codex_api
    }
]

profiles: [
    {
        name: inherits
        provider: shared
    }
    {
        name: direct
        provider: shared
        proxy: none
    }
    {
        name: own
        provider: other
        proxy: http://me:pw@proxy.example:3128
    }
]
`

func TestProxyCascade(t *testing.T) {
	t.Setenv(ConfigFileEnv, writeKtav(t, cascadeConfig))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	tests := []struct {
		profile, key string
	}{
		{"inherits", "proxy:socks5://127.0.0.1:1080"}, // no proxy of its own: the global one
		{"direct", "direct"},                          // none beats the global one
		{"own", "proxy:http://me:pw@proxy.example:3128"},
		{"missing", "proxy:socks5://127.0.0.1:1080"}, // unknown profile: the global one
	}
	for _, tt := range tests {
		if got := cfg.ProxyChoice(tt.profile).Key(); got != tt.key {
			t.Errorf("%s: choice %q, want %q", tt.profile, got, tt.key)
		}
	}
	// Without a global proxy the profiles still differ; "inherits" falls to the environment.
	cfg.ConnectionsProxy = ""
	if got := cfg.ProxyChoice("inherits").Key(); got != "env" {
		t.Errorf("no global proxy: inherits = %q, want env", got)
	}
	if got := cfg.ProxyChoice("direct").Key(); got != "direct" {
		t.Errorf("no global proxy: direct = %q", got)
	}
}

func TestProviderProxyChoice(t *testing.T) {
	t.Setenv(ConfigFileEnv, writeKtav(t, cascadeConfig))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	// "shared" has a profile that inherits and one that is direct: no common way, so the global proxy.
	if got := cfg.ProviderProxyChoice("shared").Key(); got != "proxy:socks5://127.0.0.1:1080" {
		t.Errorf("shared = %q, want the global proxy", got)
	}
	// "other" has one profile, with its own proxy.
	if got := cfg.ProviderProxyChoice("other").Key(); got != "proxy:http://me:pw@proxy.example:3128" {
		t.Errorf("other = %q", got)
	}
	// Profiles that agree give their common choice.
	cfg.Profiles["inherits"].Proxy = ProxyNone
	if got := cfg.ProviderProxyChoice("shared").Key(); got != "direct" {
		t.Errorf("both direct: shared = %q", got)
	}
	// A provider no profile uses falls back to the global proxy.
	if got := cfg.ProviderProxyChoice("nobody").Key(); got != "proxy:socks5://127.0.0.1:1080" {
		t.Errorf("unused provider = %q", got)
	}
}

func TestProfileProxyValidation(t *testing.T) {
	provider := "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n]\n"
	for _, bad := range []string{"off", "ftp://x:21", "socks5://host", "http://proxy:3128/path"} {
		body := provider + "profiles: [\n    {\n        name: w\n        provider: p\n        proxy: " + bad + "\n    }\n]\n"
		_, err := LoadFile(writeKtav(t, body))
		if err == nil || !strings.Contains(err.Error(), `profile "w": proxy`) {
			t.Errorf("proxy %q: err = %v", bad, err)
		}
	}
	for _, ok := range []string{"none", "socks5h://u:p@host:1080", "https://proxy:443"} {
		body := provider + "profiles: [\n    {\n        name: w\n        provider: p\n        proxy: " + ok + "\n    }\n]\n"
		if _, err := LoadFile(writeKtav(t, body)); err != nil {
			t.Errorf("proxy %q: %v", ok, err)
		}
	}
}

func TestDefaultTemplateIsValid(t *testing.T) {
	fc, err := LoadFile(writeKtav(t, string(DefaultTemplate())))
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
	if _, err := LoadFile(p); err != nil {
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
	if _, err := LoadFile(path); err != nil {
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
	if _, err := LoadFile(path); err != nil {
		t.Errorf("the config of a home does not load: %v", err)
	}
}

const levelsConfig = `connections.proxy: socks5://127.0.0.1:1080

providers: [
    {
        name: corp
        type: codex
        proxy: http://corp:3128
    }
    {
        name: lab
        type: codex_api
        proxy: none
    }
    {
        name: plain
        type: codex_api
    }
    {
        name: idle
        type: codex_api
        proxy: http://idle:3128
    }
]

profiles: [
    {
        name: c1
        provider: corp
    }
    {
        name: c2
        provider: corp
        proxy: http://own:8080
    }
    {
        name: c3
        provider: corp
        proxy: none
    }
    {
        name: l1
        provider: lab
    }
    {
        name: l2
        provider: lab
        proxy: http://lab-own:8080
    }
    {
        name: p1
        provider: plain
    }
]
`

func loadLevels(t *testing.T) *Config {
	t.Helper()
	t.Setenv(ConfigFileEnv, writeKtav(t, levelsConfig))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	return cfg
}

// Profile, then provider, then the global proxy: the nearest level that sets
// one replaces the ones above it whole, and none at a level is direct.
func TestProxyLevels(t *testing.T) {
	cfg := loadLevels(t)
	const global = "proxy:socks5://127.0.0.1:1080"
	tests := []struct {
		profile, key, level string
	}{
		{"c1", "proxy:http://corp:3128", ProxyLevelProvider},   // the provider's proxy replaces the global one
		{"c2", "proxy:http://own:8080", ProxyLevelProfile},     // the profile's beats the provider's
		{"c3", "direct", ProxyLevelProfile},                    // none of the profile beats the provider's proxy
		{"l1", "direct", ProxyLevelProvider},                   // none of the provider beats the global one
		{"l2", "proxy:http://lab-own:8080", ProxyLevelProfile}, // the profile beats the provider's none
		{"p1", global, ProxyLevelGlobal},                       // no override anywhere: the global one
		{"missing", global, ProxyLevelGlobal},                  // unknown profile: the global one
	}
	for _, tt := range tests {
		if got := cfg.ProxyChoice(tt.profile).Key(); got != tt.key {
			t.Errorf("%s: choice %q, want %q", tt.profile, got, tt.key)
		}
		if got := cfg.ProxyLevel(tt.profile); got != tt.level {
			t.Errorf("%s: level %q, want %q", tt.profile, got, tt.level)
		}
	}

	// Without a global proxy only the levels below it remain; the rest fall to the environment.
	cfg.ConnectionsProxy = ""
	if got := cfg.ProxyChoice("p1").Key(); got != "env" || cfg.ProxyLevel("p1") != "" {
		t.Errorf("no global proxy: p1 = %q (level %q), want env", got, cfg.ProxyLevel("p1"))
	}
	if got := cfg.ProxyChoice("c1").Key(); got != "proxy:http://corp:3128" {
		t.Errorf("no global proxy: c1 = %q", got)
	}
	if got := cfg.ProxyChoice("l1").Key(); got != "direct" {
		t.Errorf("no global proxy: l1 = %q", got)
	}
}

func TestProviderProxy(t *testing.T) {
	cfg := loadLevels(t)
	for name, want := range map[string]string{
		"corp":    "proxy:http://corp:3128",
		"lab":     "direct",
		"plain":   "proxy:socks5://127.0.0.1:1080",
		"missing": "proxy:socks5://127.0.0.1:1080",
	} {
		if got := cfg.ProviderProxy(name).Key(); got != want {
			t.Errorf("ProviderProxy(%s) = %q, want %q", name, got, want)
		}
	}
}

// What belongs to a provider (login, the catalog) goes the way its profiles
// agree on; when they differ, the provider's own proxy decides.
func TestProviderProxyChoiceWithProviderLevel(t *testing.T) {
	cfg := loadLevels(t)
	for name, want := range map[string]string{
		"corp":  "proxy:http://corp:3128",        // c1, c2, c3 differ: the provider's proxy
		"lab":   "direct",                        // l1 direct, l2 own: the provider's none
		"plain": "proxy:socks5://127.0.0.1:1080", // p1 has no override: the global one
		"idle":  "proxy:http://idle:3128",        // no profile: the provider's proxy
		"nope":  "proxy:socks5://127.0.0.1:1080", // unknown provider: the global one
	} {
		if got := cfg.ProviderProxyChoice(name).Key(); got != want {
			t.Errorf("ProviderProxyChoice(%s) = %q, want %q", name, got, want)
		}
	}
	// Profiles that agree give their common choice, the provider's proxy included.
	cfg.Profiles["c2"].Proxy, cfg.Profiles["c3"].Proxy = "", ""
	if got := cfg.ProviderProxyChoice("corp").Key(); got != "proxy:http://corp:3128" {
		t.Errorf("profiles agree: corp = %q", got)
	}
	cfg.Profiles["c1"].Proxy, cfg.Profiles["c2"].Proxy, cfg.Profiles["c3"].Proxy = ProxyNone, ProxyNone, ProxyNone
	if got := cfg.ProviderProxyChoice("corp").Key(); got != "direct" {
		t.Errorf("profiles agree on none: corp = %q", got)
	}
}

func TestProviderProxyValidation(t *testing.T) {
	for _, bad := range []string{"off", "ftp://x:21", "socks5://host", "http://proxy:3128/path"} {
		body := "providers: [\n    {\n        name: w\n        type: codex_api\n        proxy: " + bad + "\n    }\n]\n"
		_, err := LoadFile(writeKtav(t, body))
		if err == nil || !strings.Contains(err.Error(), `provider "w": proxy`) {
			t.Errorf("proxy %q: err = %v", bad, err)
		}
	}
	for _, ok := range []string{"none", "socks5h://u:p@host:1080", "https://proxy:443", "http://proxy:3128"} {
		body := "providers: [\n    {\n        name: w\n        type: codex_api\n        proxy: " + ok + "\n    }\n]\n"
		if _, err := LoadFile(writeKtav(t, body)); err != nil {
			t.Errorf("proxy %q: %v", ok, err)
		}
	}
	// The real Claude Code does not go through ccgp: nothing to proxy.
	_, err := LoadFile(writeKtav(t, "providers: [\n    {\n        name: c\n        type: claude\n        proxy: none\n    }\n]\n"))
	if err == nil || !strings.Contains(err.Error(), "takes no") {
		t.Errorf("claude provider with a proxy: %v", err)
	}
}
