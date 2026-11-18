package cfgedit

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

const speedBase = "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: one\n        provider: codex\n    }\n]\n"

func speedRulesOf(t *testing.T, text string) map[string]string {
	t.Helper()
	fc, err := config.LoadFile(writeKtav(t, text))
	if err != nil {
		t.Fatalf("config does not load: %v\n%s", err, text)
	}
	return fc.Profiles[0].SpeedRules
}

func TestSetProfileSpeedTextReplacesAValue(t *testing.T) {
	text, err := SetProfileSpeedText(speedBase, "one", "astra", config.SpeedFast)
	if err != nil {
		t.Fatal(err)
	}
	if text, err = SetProfileSpeedText(text, "one", "astra", config.SpeedStandard); err != nil {
		t.Fatal(err)
	}
	rules := speedRulesOf(t, text)
	if len(rules) != 1 || rules["astra"] != config.SpeedStandard || strings.Count(text, "astra:") != 1 {
		t.Errorf("rules %v\n%s", rules, text)
	}
}

func TestSetProfileSpeedTextKeepsLineEndings(t *testing.T) {
	crlf := strings.ReplaceAll(speedBase, "\n", "\r\n")
	text, err := SetProfileSpeedText(crlf, "one", "sol", config.SpeedFast)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(text, "\n") != strings.Count(text, "\r\n") {
		t.Errorf("a bare LF was written:\n%q", text)
	}
	if rules := speedRulesOf(t, text); rules["sol"] != config.SpeedFast {
		t.Errorf("rules %v", rules)
	}
	text, err = SetProfileSpeedText(text, "one", "sol", "")
	if err != nil || strings.Contains(text, "speed_rules") || strings.Count(text, "\n") != strings.Count(text, "\r\n") {
		t.Errorf("removal: %q, %v", text, err)
	}
}

func TestSetProfileSpeedTextRemovalKeepsOthersAndComments(t *testing.T) {
	text := speedBase
	for _, family := range []string{"astra", "sol"} {
		var err error
		if text, err = SetProfileSpeedText(text, "one", family, config.SpeedFast); err != nil {
			t.Fatal(err)
		}
	}
	text, err := SetProfileSpeedText(text, "one", "astra", "")
	if err != nil {
		t.Fatal(err)
	}
	if rules := speedRulesOf(t, text); len(rules) != 1 || rules["sol"] != config.SpeedFast {
		t.Fatalf("rules %v\n%s", rules, text)
	}
	// A comment inside the block outlives the block when the last rule goes.
	text = strings.Replace(text, "sol: fast", "## keep me\n            sol: fast", 1)
	text, err = SetProfileSpeedText(text, "one", "sol", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "## keep me") || strings.Contains(text, "speed_rules") {
		t.Errorf("after the last removal:\n%s", text)
	}
	if rules := speedRulesOf(t, text); len(rules) != 0 {
		t.Errorf("rules %v", rules)
	}
}

func TestSetProfileSpeedTextRefusals(t *testing.T) {
	inline := strings.Replace(speedBase, "        provider: codex\n    }\n]\n", "        provider: codex\n        speed_rules: {}\n    }\n]\n", 1)
	if _, err := SetProfileSpeedText(inline, "one", "astra", config.SpeedFast); err == nil || !strings.Contains(err.Error(), "multiline object") {
		t.Errorf("inline block: %v", err)
	}
	if _, err := SetProfileSpeedText(speedBase, "missing", "astra", config.SpeedFast); err == nil {
		t.Error("unknown profile accepted")
	}
	for _, family := range []string{"", "Astra", "as-tra", "astra1", "a b"} {
		if _, err := SetProfileSpeedText(speedBase, "one", family, config.SpeedFast); err == nil {
			t.Errorf("family %q accepted", family)
		}
	}
	if out, err := SetProfileSpeedText(speedBase, "one", "astra", ""); err != nil || out != speedBase {
		t.Errorf("removing a rule that is not there changed the text: %v", err)
	}
}

func TestSpeedRuleLoadValidation(t *testing.T) {
	for name, rule := range map[string]string{"upper-case family": "Astra: fast", "unknown speed": "astra: turbo"} {
		text := strings.Replace(speedBase, "        provider: codex\n    }\n]\n", "        provider: codex\n        speed_rules: {\n            "+rule+"\n        }\n    }\n]\n", 1)
		if _, err := config.LoadFile(writeKtav(t, text)); err == nil || !strings.Contains(err.Error(), "invalid speed rule") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSpeedRulesNilProfile(t *testing.T) {
	var p *config.Profile
	if p.SpeedFor("astra") != "" || !p.EffectiveFast("astra", true) || p.EffectiveFast("astra", false) {
		t.Error("a missing profile must inherit the client's request")
	}
}

func TestSpeedRulesRequireCodex(t *testing.T) {
	for _, provider := range []string{config.ProviderTypeCodexAPI, config.ProviderTypeAnthropicCompat, config.ProviderTypeClaude, config.ProviderTypeClaudeAPI} {
		t.Run(provider, func(t *testing.T) {
			baseURL := ""
			if provider == config.ProviderTypeAnthropicCompat {
				baseURL = "        base_url: https://example.com\n"
			}
			text := "providers: [\n    {\n        name: p\n        type: " + provider + "\n" + baseURL + "    }\n]\nprofiles: [\n    {\n        name: p\n        provider: p\n    }\n]\n"
			p := filepath.Join(t.TempDir(), "config.ktav")
			if err := os.WriteFile(p, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.LoadFile(p); err != nil {
				t.Fatal(err)
			}
			_, err := EditFile(p, func(s string) (string, error) { return SetProfileSpeedText(s, "p", "astra", config.SpeedFast) })
			if err == nil || !strings.Contains(err.Error(), "speed_rules require a codex provider") {
				t.Fatalf("unexpected error: %v", err)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != text {
				t.Fatal("invalid edit changed file")
			}
			invalid, err := SetProfileSpeedText(text, "p", "astra", config.SpeedFast)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(invalid), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.LoadFile(p); err == nil || !strings.Contains(err.Error(), "speed_rules require a codex provider") {
				t.Fatalf("unexpected load error: %v", err)
			}
		})
	}
}

func TestProfileSpeedRules(t *testing.T) {
	base := "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: one\n        provider: codex\n        ## ccgp:managed-models begin\n        models: [\n        ]\n        ## ccgp:managed-models end\n    }\n    {\n        name: two\n        provider: codex\n    }\n]\n"
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	edit := func(profile, family, speed string) string {
		t.Helper()
		backup, err := EditFile(p, func(s string) (string, error) { return SetProfileSpeedText(s, profile, family, speed) })
		if err != nil {
			t.Fatal(err)
		}
		return backup
	}
	if edit("one", "astra", "fast") == "" {
		t.Fatal("missing backup")
	}
	edit("one", "sol", "standard")
	edit("two", "astra", "standard")
	fc, err := config.LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Profiles[0].SpeedFor("astra") != config.SpeedFast || fc.Profiles[0].SpeedFor("sol") != config.SpeedStandard || fc.Profiles[1].SpeedFor("astra") != config.SpeedStandard || fc.Profiles[1].SpeedFor("sol") != "" {
		t.Fatal("incorrect profile rules")
	}
	if !fc.Profiles[0].EffectiveFast("astra", false) || fc.Profiles[0].EffectiveFast("sol", true) || !fc.Profiles[1].EffectiveFast("sol", true) || fc.Profiles[1].EffectiveFast("sol", false) {
		t.Fatal("incorrect effective speed")
	}
	edit("one", "astra", "")
	if backup := edit("one", "astra", ""); backup != "" {
		t.Fatal("no-op made backup")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "## ccgp:managed-models begin\n        models: [\n        ]\n        ## ccgp:managed-models end") {
		t.Fatal("managed block changed")
	}
	fc, err = config.LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Profiles[0].SpeedFor("sol") != config.SpeedStandard || fc.Profiles[1].SpeedFor("astra") != config.SpeedStandard {
		t.Fatal("unrelated rule changed")
	}
	edit("one", "sol", "")
	if _, err := config.LoadFile(p); err != nil {
		t.Fatalf("last rule removal produced invalid config: %v", err)
	}
	if backup := edit("one", "sol", ""); backup != "" {
		t.Fatal("repeated last-rule unset made backup")
	}
	if _, err := SetProfileSpeedText(base, "one", "astra", "turbo"); err == nil {
		t.Fatal("accepted invalid speed")
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
	t.Setenv(config.ConfigFileEnv, path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if seeded, err := SeedCodexClientVersion(cfg); err != nil || !seeded {
		t.Fatalf("seed = %v, %v", seeded, err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "codex.client_version:: "+config.DefaultCodexClientVersion) {
		t.Fatalf("seeded config:\n%s", b)
	}
	if cfg, err = config.Load(); err != nil || cfg.CodexClientVersionSet != config.DefaultCodexClientVersion {
		t.Fatalf("reloaded %q, %v", cfg.CodexClientVersionSet, err)
	}
	if seeded, err := SeedCodexClientVersion(cfg); err != nil || seeded {
		t.Errorf("second seed = %v, %v; want a no-op", seeded, err)
	}

	from, changed, err := RaiseCodexClientVersion(path, "0.200.1")
	if err != nil || !changed || from != config.DefaultCodexClientVersion {
		t.Fatalf("raise = %q, %v, %v", from, changed, err)
	}
	if from, changed, err = RaiseCodexClientVersion(path, "0.170.0"); err != nil || changed || from != "0.200.1" {
		t.Errorf("lowering = %q, %v, %v; want no change", from, changed, err)
	}

	// No codex provider, or no config file: nothing is written.
	other := writeKtav(t, "providers: [\n    {\n        name: p\n        type: codex_api\n    }\n]\n")
	t.Setenv(config.ConfigFileEnv, other)
	if cfg, err = config.Load(); err != nil {
		t.Fatal(err)
	}
	if seeded, err := SeedCodexClientVersion(cfg); err != nil || seeded {
		t.Errorf("seed without a codex provider = %v, %v", seeded, err)
	}
	if seeded, err := SeedCodexClientVersion(&config.Config{}); err != nil || seeded {
		t.Errorf("seed in env-only mode = %v, %v", seeded, err)
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

func TestEditFileRemovesProfileProxyAfterBlankLine(t *testing.T) {
	const body = `connections.proxy: socks5://127.0.0.1:1080
providers: [
    {
        name: p
        type: codex_api
    }
]
profiles: [
    {

        name: w
        provider: p
        proxy: none
    }
]
`
	path := writeKtav(t, body)
	backup, err := EditFile(path, func(text string) (string, error) {
		return SetProfileProxyText(text, "w", "")
	})
	if err != nil || backup == "" {
		t.Fatalf("EditFile remove profile proxy = %q, %v", backup, err)
	}

	t.Setenv(config.ConfigFileEnv, path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load edited config: %v", err)
	}
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	if got := cfg.Profiles["w"].Proxy; got != "" {
		t.Errorf("profile proxy = %q, want unset", got)
	}
	if got := cfg.ProxyChoice("w").Key(); got != "proxy:socks5://127.0.0.1:1080" {
		t.Errorf("inherited proxy = %q, want global proxy", got)
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
	none, err := SetProviderProxyText(got, "zai", config.ProxyNone)
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
	prov := &ProviderSpec{Name: "zai9", Type: config.ProviderTypeAnthropicCompat, BaseURL: "https://api.z.ai/api/anthropic", Proxy: "http://corp:3128"}
	text, err := AddProfileText(peakBase, prov, ProfileSpec{Name: "zai9", Provider: "zai9"})
	if err != nil {
		t.Fatal(err)
	}
	if pr := providerPtr(mustLoad(t, text), "zai9"); pr == nil || pr.Proxy != "http://corp:3128" {
		t.Errorf("provider after add: %+v\n%s", pr, text)
	}
}

func TestSetProviderPeakText(t *testing.T) {
	windows := []peak.Window{{Days: "mon-fri", From: "14:00", To: "18:00"}, {From: "20:00", To: "23:00"}}
	got, err := SetProviderPeakText(peakBase, "zai", windows, "Asia/Shanghai")
	want := strings.Replace(peakBase, "        base_url: https://api.z.ai/api/anthropic\n", `        base_url: https://api.z.ai/api/anthropic
        peak_tz: Asia/Shanghai
        peak: [
            {
                days: mon-fri
                from: 14:00
                to: 18:00
            }
            {
                from: 20:00
                to: 23:00
            }
        ]
`, 1)
	if err != nil || got != want {
		t.Fatalf("set: %v\n%s\nwant\n%s", err, got, want)
	}
	fc := mustLoad(t, got)
	if zai := providerPtr(fc, "zai"); !reflect.DeepEqual(zai.Peak, windows) || zai.PeakTZ != "Asia/Shanghai" {
		t.Errorf("loaded back: %+v in %q", zai.Peak, zai.PeakTZ)
	}

	// A new list replaces the old one, and a missing zone removes the old zone.
	one := []peak.Window{{From: "01:00", To: "02:00"}}
	again, err := SetProviderPeakText(got, "zai", one, "")
	if err != nil || strings.Contains(again, "peak_tz") || strings.Contains(again, "14:00") || !strings.Contains(again, "from: 01:00") {
		t.Fatalf("replace: %v\n%s", err, again)
	}
	mustLoad(t, again)

	// Clearing gives the original text back; clearing nothing changes nothing.
	if back, err := SetProviderPeakText(got, "zai", nil, ""); err != nil || back != peakBase {
		t.Errorf("clear: %v\n%s", err, back)
	}
	if same, err := SetProviderPeakText(peakBase, "zai", nil, ""); err != nil || same != peakBase {
		t.Errorf("clear of nothing: %v\n%s", err, same)
	}

	// Another provider stays as it was.
	if !strings.Contains(got, "    {\n        name: codex\n        type: codex\n    }\n") {
		t.Errorf("codex was touched:\n%s", got)
	}
	if _, err := SetProviderPeakText(peakBase, "nope", windows, ""); err == nil {
		t.Error("unknown provider accepted")
	}
	if _, err := SetProviderPeakText(peakBase, "zai", []peak.Window{{From: "9", To: "10"}}, ""); err == nil {
		t.Error("bad window accepted")
	}

	// CRLF files keep their endings.
	crlf := strings.ReplaceAll(peakBase, "\n", "\r\n")
	got3, err := SetProviderPeakText(crlf, "codex", one, "+08:00")
	if err != nil || strings.Contains(strings.ReplaceAll(got3, "\r\n", ""), "\n") || !strings.Contains(got3, "        type: codex\r\n        peak_tz: +08:00\r\n        peak: [\r\n") {
		t.Errorf("CRLF: %v\n%q", err, got3)
	}
	mustLoad(t, got3)
}

// A provider made by profile add carries its peak hours, and the text it is
// written as loads back.
func TestAddProfileTextWritesPeakHours(t *testing.T) {
	windows := []peak.Window{{Days: "mon-fri", From: "14:00", To: "18:00"}, {From: "20:00", To: "23:00"}}
	prov := &ProviderSpec{Name: "zai9", Type: config.ProviderTypeAnthropicCompat, BaseURL: "https://api.z.ai/api/anthropic", Peak: windows, PeakTZ: "Asia/Shanghai"}
	text, err := AddProfileText(peakBase, prov, ProfileSpec{Name: "zai9", Provider: "zai9"})
	if err != nil {
		t.Fatal(err)
	}
	pr := providerPtr(mustLoad(t, text), "zai9")
	if pr == nil || !reflect.DeepEqual(pr.Peak, windows) || pr.PeakTZ != "Asia/Shanghai" {
		t.Errorf("provider after the add: %+v", pr)
	}
	if other := providerPtr(mustLoad(t, text), "zai"); len(other.Peak) != 0 {
		t.Errorf("another provider got peak hours: %+v", other.Peak)
	}
}

func TestSetProviderPeakTextKeepsComments(t *testing.T) {
	base := strings.Replace(peakBase, "        type: anthropic_compat\n",
		"        type: anthropic_compat\n        ## my note about peak hours\n        ## peak: [ the old way\n", 1)
	windows := []peak.Window{{From: "14:00", To: "18:00"}}
	set, err := SetProviderPeakText(base, "zai", windows, "UTC")
	if err != nil || !strings.Contains(set, "## my note about peak hours\n        ## peak: [ the old way\n") {
		t.Fatalf("set: %v\n%s", err, set)
	}
	if pr := providerPtr(mustLoad(t, set), "zai"); !reflect.DeepEqual(pr.Peak, windows) {
		t.Errorf("loaded back: %+v", pr.Peak)
	}
	if back, err := SetProviderPeakText(set, "zai", nil, ""); err != nil || back != base {
		t.Errorf("clear: %v\n%s", err, back)
	}
}

func TestSetProviderPeakTextNeedsAProvidersList(t *testing.T) {
	if _, err := SetProviderPeakText("listen.port: 60093\n", "zai", []peak.Window{{From: "14:00", To: "18:00"}}, ""); err == nil {
		t.Error("a config without providers was edited")
	}
}

// Keys at an indent of their own: the object's first key sets the indent of the
// new lines, and with none of the scalar keys there the lines go to the end of
// the object.
func TestSetProviderPeakTextWithUnevenIndent(t *testing.T) {
	const odd = "providers: [\n    {\n            effort_map: {\n                low: low\n            }\n        name: zai\n        type: anthropic_compat\n        base_url: https://api.z.ai/api/anthropic\n    }\n]\nprofiles: [\n    {\n        name: zai\n        provider: zai\n    }\n]\n"
	windows := []peak.Window{{Days: "sat", From: "10:00", To: "12:00"}}
	set, err := SetProviderPeakText(odd, "zai", windows, "")
	if err != nil || !strings.HasSuffix(strings.SplitN(set, "profiles:", 2)[0], "            peak: [\n                {\n                    days: sat\n                    from: 10:00\n                    to: 12:00\n                }\n            ]\n    }\n]\n") {
		t.Fatalf("set: %v\n%s", err, set)
	}
	if pr := providerPtr(mustLoad(t, set), "zai"); !reflect.DeepEqual(pr.Peak, windows) {
		t.Errorf("loaded back: %+v", pr.Peak)
	}
	again, err := SetProviderPeakText(set, "zai", []peak.Window{{From: "01:00", To: "02:00"}}, "")
	if err != nil || strings.Contains(again, "10:00") || strings.Count(again, "peak: [") != 1 {
		t.Errorf("replace: %v\n%s", err, again)
	}
	if back, err := SetProviderPeakText(set, "zai", nil, ""); err != nil || back != odd {
		t.Errorf("clear: %v\n%s", err, back)
	}
}
