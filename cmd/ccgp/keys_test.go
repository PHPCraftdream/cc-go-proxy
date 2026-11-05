package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

// testConfig plus a provider and a profile of the real Claude Code.
func claudeTestConfig() string {
	text := strings.Replace(testConfig, "providers: [\n", "providers: [\n    {\n        name: work\n        type: claude\n    }\n", 1)
	return strings.Replace(text, "profiles: [\n", "profiles: [\n    {\n        name: work\n        provider: work\n    }\n", 1)
}

func TestProfileSet(t *testing.T) {
	path, _ := writeTestConfig(t, claudeTestConfig())
	var buf bytes.Buffer
	run := func(unset bool, args ...string) error {
		buf.Reset()
		return profileSet(&buf, loadTestConfig(t), args, unset)
	}

	if err := run(false, "zai2", "default_model", "glm-5.3"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "config.ktav updated") || loadTestConfig(t).Profiles["zai2"].DefaultModel != "glm-5.3" {
		t.Errorf("default_model: %q, %+v", buf.String(), loadTestConfig(t).Profiles["zai2"])
	}
	if err := run(false, "zai2", "default_model", "glm-5.3"); err != nil || !strings.Contains(buf.String(), "already had this value") {
		t.Errorf("the same value again: %v %q", err, buf.String())
	}
	if err := run(false, "zai2", "context_window", "500000"); err != nil {
		t.Fatal(err)
	}
	if err := run(false, "zai2", "compact_percent", "70"); err != nil {
		t.Fatal(err)
	}
	if pf := loadTestConfig(t).Profiles["zai2"]; pf.ContextWindow != 500000 || pf.CompactPercent != 70 || pf.DefaultModel != "glm-5.3" {
		t.Errorf("profile zai2 = %+v", pf)
	}
	if err := run(true, "zai2", "compact_percent"); err != nil {
		t.Fatal(err)
	}
	if pf := loadTestConfig(t).Profiles["zai2"]; pf.CompactPercent != 0 || pf.ContextWindow != 500000 {
		t.Errorf("after unset: %+v", pf)
	}
	if err := run(false, "zai2", "provider", "codex"); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Profiles["zai2"].Provider; got != "codex" {
		t.Errorf("provider after the move = %q", got)
	}

	// Refused: nothing is written.
	before, _ := os.ReadFile(path)
	for name, call := range map[string]func() error{
		"unknown profile":     func() error { return run(false, "nope", "default_model", "m") },
		"unknown key":         func() error { return run(false, "zai", "models", "m") },
		"proxy has its own":   func() error { return run(false, "zai", "proxy", "none") },
		"not a number":        func() error { return run(false, "zai", "context_window", "big") },
		"negative":            func() error { return run(false, "zai", "compact_percent", "-5") },
		"over 100":            func() error { return run(false, "zai", "compact_percent", "150") },
		"unknown provider":    func() error { return run(false, "zai", "provider", "nope") },
		"a claude provider":   func() error { return run(false, "zai", "provider", "work") },
		"unset the provider":  func() error { return run(true, "zai", "provider") },
		"a claude profile":    func() error { return run(false, "work", "default_model", "m") },
		"too few words":       func() error { return run(false, "zai", "default_model") },
		"too many words":      func() error { return run(false, "zai", "default_model", "a", "b") },
		"unset with a value":  func() error { return run(true, "zai", "default_model", "a") },
		"unset without a key": func() error { return run(true, "zai") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("config changed by refused edits:\n%s", after)
	}
}

func TestProfileSetThroughTheDispatcher(t *testing.T) {
	writeTestConfig(t, testConfig)
	t.Setenv("CCGP_HOME", t.TempDir())
	if err := profileCmd(loadTestConfig(t), []string{"set", "zai", "default_model", "m"}); err != nil {
		t.Fatal(err)
	}
	if err := profileCmd(loadTestConfig(t), []string{"unset", "zai", "default_model"}); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Profiles["zai"].DefaultModel; got != "" {
		t.Errorf("default_model = %q after unset", got)
	}
}

func providerRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("CCGP_HOME", t.TempDir())
	var buf bytes.Buffer
	store := creds.Store{Dir: t.TempDir()}
	err := provider(&buf, loadTestConfig(t), store, config.NewPeakPauses(store.Dir), time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC), args)
	return buf.String(), err
}

func TestProviderListAndShow(t *testing.T) {
	text := strings.Replace(testConfig, "        api_key_env: ZAI_KEY_TEST\n",
		"        api_key_env: ZAI_KEY_TEST\n        proxy: http://corp:3128\n        peak: [\n            {\n                days: mon-fri\n                from: 14:00\n                to: 18:00\n            }\n        ]\n", 1)
	writeTestConfig(t, text)

	out, err := providerRun(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PROVIDER", "codex", "codex ", "zai", "anthropic_compat", "yes", "zai, zai2"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}

	out, err = providerRun(t, "zai")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type         anthropic_compat", "base_url     https://api.z.ai/api/anthropic", "api_key_env  ZAI_KEY_TEST",
		"proxy        http://corp:3128 (its own)", "effort_map   max=max medium=high", "peak hours   mon-fri 14:00-18:00", "profiles     zai, zai2"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lacks %q:\n%s", want, out)
		}
	}

	out, err = providerRun(t, "codex")
	if err != nil || !strings.Contains(out, "proxy        global (") || !strings.Contains(out, "peak hours   none") {
		t.Errorf("codex: %v\n%s", err, out)
	}

	out, err = providerRun(t, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got providerListData
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Providers) != 2 {
		t.Fatalf("json %s: %v", out, err)
	}
	if z := got.Providers[1]; z.Name != "zai" || z.Proxy != "http://corp:3128" || !z.ProxyOwn || z.APIKeyEnv != "ZAI_KEY_TEST" || len(z.Profiles) != 2 {
		t.Errorf("zai = %+v", z)
	}
	out, err = providerRun(t, "zai", "--json")
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || len(got.Providers) != 1 || got.Providers[0].Name != "zai" {
		t.Errorf("one provider as json: %v\n%s", err, out)
	}

	for _, args := range [][]string{{"nope"}, {"zai", "codex"}, {"--bogus"}} {
		if _, err := providerRun(t, args...); err == nil {
			t.Errorf("%q: accepted", args)
		}
	}
}

func TestProviderSet(t *testing.T) {
	path, _ := writeTestConfig(t, claudeTestConfig())
	out, err := providerRun(t, "set", "zai", "base_url", "https://example.internal/api")
	if err != nil || !strings.Contains(out, "config.ktav updated") {
		t.Fatalf("set base_url: %v %q", err, out)
	}
	if got := loadTestConfig(t).Providers["zai"].BaseURL; got != "https://example.internal/api" {
		t.Errorf("base_url = %q", got)
	}
	if out, err = providerRun(t, "set", "zai", "base_url", "https://example.internal/api"); err != nil || !strings.Contains(out, "already had this value") {
		t.Errorf("the same value again: %v %q", err, out)
	}
	if _, err = providerRun(t, "unset", "zai", "api_key_env"); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Providers["zai"].APIKeyEnv; got != "" {
		t.Errorf("api_key_env = %q after unset", got)
	}
	if _, err = providerRun(t, "set", "zai", "api_key_env", "MY_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := loadTestConfig(t).Providers["zai"].APIKeyEnv; got != "MY_KEY" {
		t.Errorf("api_key_env = %q", got)
	}

	before, _ := os.ReadFile(path)
	for name, args := range map[string][]string{
		"unknown provider":     {"set", "nope", "base_url", "x"},
		"unknown key":          {"set", "zai", "type", "codex"},
		"proxy has its own":    {"set", "zai", "proxy", "none"},
		"codex has no key env": {"set", "codex", "api_key_env", "K"},
		"a claude provider":    {"set", "work", "base_url", "x"},
		"base_url is required": {"unset", "zai", "base_url"},
		"too few words":        {"set", "zai", "base_url"},
		"unset with a value":   {"unset", "zai", "base_url", "x"},
		"set without anything": {"set"},
		"unset without a key":  {"unset", "zai"},
		"two lines in a value": {"set", "zai", "base_url", "a\nb"},
	} {
		if _, err := providerRun(t, args...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("config changed by refused edits:\n%s", after)
	}
}

func TestProviderNeedsAConfigFile(t *testing.T) {
	cfg := &config.Config{}
	if err := provider(&bytes.Buffer{}, cfg, creds.Store{}, config.NewPeakPauses(""), time.Now(), nil); err == nil {
		t.Error("env-only mode accepted")
	}
}
