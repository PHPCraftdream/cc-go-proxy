package config

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

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

func TestSetConnectionsText(t *testing.T) {
	tests := []struct {
		name, in, key string
		value         int64
		want          string
	}{
		{"replace", "listen.port: 1\nconnections.active: 7 ## note\nx: y\n", "active", 3,
			"listen.port: 1\nconnections.active: 3\nx: y\n"},
		{"after listen", "## head\nlisten.host: h\nlisten.port: 1\n\nproviders: []\n", "total", 12,
			"## head\nlisten.host: h\nlisten.port: 1\nconnections.total: 12\n\nproviders: []\n"},
		{"after the other one", "listen.port: 1\nconnections.active: 3\n\nx: y\n", "total", 5,
			"listen.port: 1\nconnections.active: 3\nconnections.total: 5\n\nx: y\n"},
		{"after comments", "## a\n## b\nproviders: []\n", "active", 2,
			"## a\n## b\nconnections.active: 2\nproviders: []\n"},
		{"crlf", "listen.port: 1\r\nconnections.total: 10\r\n", "total", 4,
			"listen.port: 1\r\nconnections.total: 4\r\n"},
		{"crlf insert", "listen.port: 1\r\nx: y\r\n", "active", 4,
			"listen.port: 1\r\nconnections.active: 4\r\nx: y\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SetConnectionsText(tt.in, tt.key, tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q\nwant %q", got, tt.want)
			}
		})
	}
	if _, err := SetConnectionsText("connections: {\n    active: 7\n}\n", "active", 3); err == nil {
		t.Error("a connections block was edited")
	}
	if _, err := SetConnectionsText("", "lanes", 3); err == nil {
		t.Error("unknown key accepted")
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

func TestSetConnectionsProxyText(t *testing.T) {
	in := "listen.port: 1\nconnections.active: 3\n\nx: y\n"
	got, err := SetConnectionsProxyText(in, "http://proxy:3128")
	if err != nil || got != "listen.port: 1\nconnections.active: 3\nconnections.proxy: http://proxy:3128\n\nx: y\n" {
		t.Fatalf("set: %q, %v", got, err)
	}
	got, err = SetConnectionsProxyText(got, "socks5://127.0.0.1:1080")
	if err != nil || !strings.Contains(got, "connections.proxy: socks5://127.0.0.1:1080\n") || strings.Contains(got, "3128") {
		t.Fatalf("replace: %q, %v", got, err)
	}
	got, err = SetConnectionsProxyText(got, "")
	if err != nil || got != in {
		t.Fatalf("remove: %q, %v", got, err)
	}
	if _, err := SetConnectionsProxyText(in, "ftp://x:1"); err == nil {
		t.Error("bad proxy accepted")
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
		_, err := loadFile(writeKtav(t, body))
		if err == nil || !strings.Contains(err.Error(), `profile "w": proxy`) {
			t.Errorf("proxy %q: err = %v", bad, err)
		}
	}
	for _, ok := range []string{"none", "socks5h://u:p@host:1080", "https://proxy:443"} {
		body := provider + "profiles: [\n    {\n        name: w\n        provider: p\n        proxy: " + ok + "\n    }\n]\n"
		if _, err := loadFile(writeKtav(t, body)); err != nil {
			t.Errorf("proxy %q: %v", ok, err)
		}
	}
}

func TestSetProfileProxyText(t *testing.T) {
	const base = "profiles: [\n    {\n        name: a\n        provider: p\n        models: [\n            {\n                match: *\n                model: m\n            }\n        ]\n    }\n    {\n        name: b\n        provider: p\n        proxy: none\n    }\n]\n"
	got, err := SetProfileProxyText(base, "a", "socks5://127.0.0.1:1080")
	want := strings.Replace(base, "        name: a\n        provider: p\n", "        name: a\n        provider: p\n        proxy: socks5://127.0.0.1:1080\n", 1)
	if err != nil || got != want {
		t.Fatalf("add: %q\nwant %q\n%v", got, want, err)
	}
	got2, err := SetProfileProxyText(got, "a", "none")
	if err != nil || !strings.Contains(got2, "name: a\n        provider: p\n        proxy: none\n        models: [") || strings.Contains(got2, "socks5") {
		t.Fatalf("replace: %q, %v", got2, err)
	}
	if back, err := SetProfileProxyText(got, "a", ""); err != nil || back != base {
		t.Fatalf("remove: %q, %v", back, err)
	}
	// Only the named profile changes; b keeps its line.
	if !strings.Contains(got, "name: b\n        provider: p\n        proxy: none\n") {
		t.Errorf("profile b was touched: %q", got)
	}
	if same, err := SetProfileProxyText(base, "a", ""); err != nil || same != base {
		t.Errorf("removing a missing line changed the text: %q, %v", same, err)
	}
	if _, err := SetProfileProxyText(base, "nope", "none"); err == nil {
		t.Error("unknown profile accepted")
	}
	if _, err := SetProfileProxyText(base, "a", "ftp://x:1"); err == nil {
		t.Error("bad proxy accepted")
	}
	crlf := strings.ReplaceAll(base, "\n", "\r\n")
	got3, err := SetProfileProxyText(crlf, "a", "none")
	if err != nil || !strings.Contains(got3, "        provider: p\r\n        proxy: none\r\n") || strings.Contains(strings.ReplaceAll(got3, "\r\n", ""), "\n") {
		t.Errorf("CRLF: %q, %v", got3, err)
	}
	if again, err := SetProfileProxyText(got3, "a", "http://h:1"); err != nil || !strings.Contains(again, "        proxy: http://h:1\r\n") {
		t.Errorf("CRLF replace: %q, %v", again, err)
	}
}
