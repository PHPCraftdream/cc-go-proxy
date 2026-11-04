package config

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

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
		_, err := loadFile(writeKtav(t, body))
		if err == nil || !strings.Contains(err.Error(), `provider "w": proxy`) {
			t.Errorf("proxy %q: err = %v", bad, err)
		}
	}
	for _, ok := range []string{"none", "socks5h://u:p@host:1080", "https://proxy:443", "http://proxy:3128"} {
		body := "providers: [\n    {\n        name: w\n        type: codex_api\n        proxy: " + ok + "\n    }\n]\n"
		if _, err := loadFile(writeKtav(t, body)); err != nil {
			t.Errorf("proxy %q: %v", ok, err)
		}
	}
	// The real Claude Code does not go through ccgp: nothing to proxy.
	_, err := loadFile(writeKtav(t, "providers: [\n    {\n        name: c\n        type: claude\n        proxy: none\n    }\n]\n"))
	if err == nil || !strings.Contains(err.Error(), "takes no") {
		t.Errorf("claude provider with a proxy: %v", err)
	}
}

func TestSetProviderProxyText(t *testing.T) {
	got, err := SetProviderProxyText(peakBase, "zai", "socks5://127.0.0.1:1080")
	want := strings.Replace(peakBase, "        base_url: https://api.z.ai/api/anthropic\n", "        base_url: https://api.z.ai/api/anthropic\n        proxy: socks5://127.0.0.1:1080\n", 1)
	if err != nil || got != want {
		t.Fatalf("add: %v\n%s\nwant\n%s", err, got, want)
	}
	if pr := providerPtr(mustLoad(t, got), "zai"); pr.Proxy != "socks5://127.0.0.1:1080" || pr.EffortMap["low"] != "low" {
		t.Errorf("loaded back: %+v", pr)
	}
	// Replaced in place, none included; the effort_map and the profiles stay as they are.
	none, err := SetProviderProxyText(got, "zai", ProxyNone)
	if err != nil || !strings.Contains(none, "        base_url: https://api.z.ai/api/anthropic\n        proxy: none\n") || strings.Contains(none, "socks5") || strings.Count(none, "proxy:") != 1 {
		t.Fatalf("replace: %v\n%s", err, none)
	}
	if back, err := SetProviderProxyText(got, "zai", ""); err != nil || back != peakBase {
		t.Errorf("remove: %v\n%s", err, back)
	}
	if same, err := SetProviderProxyText(peakBase, "zai", ""); err != nil || same != peakBase {
		t.Errorf("removing a line that is not there changed the text: %v\n%s", err, same)
	}
	// A provider with only name and type gets the line after its type.
	codex, err := SetProviderProxyText(peakBase, "codex", "http://corp:3128")
	if err != nil || !strings.Contains(codex, "        name: codex\n        type: codex\n        proxy: http://corp:3128\n    }\n") {
		t.Errorf("codex: %v\n%s", err, codex)
	}
	// The profile of the same name is not the provider.
	if strings.Contains(got, "        name: zai\n        provider: zai\n        proxy") {
		t.Errorf("the profile was touched:\n%s", got)
	}
	if _, err := SetProviderProxyText(peakBase, "nope", "none"); err == nil {
		t.Error("unknown provider accepted")
	}
	if _, err := SetProviderProxyText(peakBase, "zai", "ftp://x:1"); err == nil {
		t.Error("bad proxy accepted")
	}
	if _, err := SetProviderKeyText(peakBase, "zai", "proxy", "none"); err == nil {
		t.Error("proxy went through the key editor, which has no URL check")
	}

	crlf := strings.ReplaceAll(peakBase, "\n", "\r\n")
	set, err := SetProviderProxyText(crlf, "codex", "none")
	if err != nil || !strings.Contains(set, "        type: codex\r\n        proxy: none\r\n") || strings.Contains(strings.ReplaceAll(set, "\r\n", ""), "\n") {
		t.Fatalf("CRLF add: %v\n%q", err, set)
	}
	repl, err := SetProviderProxyText(set, "codex", "http://h:1")
	if err != nil || !strings.Contains(repl, "        proxy: http://h:1\r\n") {
		t.Errorf("CRLF replace: %v\n%q", err, repl)
	}
	mustLoad(t, repl)
}

// A provider made by profile add carries its proxy.
func TestAddProfileTextWritesProviderProxy(t *testing.T) {
	prov := &ProviderSpec{Name: "zai9", Type: ProviderTypeAnthropicCompat, BaseURL: "https://api.z.ai/api/anthropic", Proxy: "http://corp:3128"}
	text, err := AddProfileText(peakBase, prov, ProfileSpec{Name: "zai9", Provider: "zai9"})
	if err != nil {
		t.Fatal(err)
	}
	if pr := providerPtr(mustLoad(t, text), "zai9"); pr == nil || pr.Proxy != "http://corp:3128" {
		t.Errorf("provider after add: %+v\n%s", pr, text)
	}
}
