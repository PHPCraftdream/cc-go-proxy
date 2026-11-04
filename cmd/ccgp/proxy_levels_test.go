package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

// A global proxy, a codex provider p with profiles a and b, and a Claude one.
func providerProxyTestConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	body := "connections.proxy: socks5://me:s3cret@127.0.0.1:1080\n\n" +
		"providers: [\n    {\n        name: p\n        type: codex\n    }\n    {\n        name: w\n        type: claude\n    }\n]\n\n" +
		"profiles: [\n    {\n        name: a\n        provider: p\n    }\n    {\n        name: b\n        provider: p\n    }\n    {\n        name: w\n        provider: w\n    }\n]\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, p)
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	return p
}

func TestConnectionsProviderProxy(t *testing.T) {
	p := providerProxyTestConfig(t)
	var buf bytes.Buffer
	// A proxy for the provider: every profile of it goes through it.
	if err := connections(&buf, []string{"proxy", "http://corp:3128", "provider", "p"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "name: p\n        type: codex\n        proxy: http://corp:3128\n") {
		t.Fatalf("config:\n%s", data)
	}
	if !strings.Contains(string(data), "connections.proxy: socks5://me:s3cret@127.0.0.1:1080\n") {
		t.Errorf("the global proxy changed:\n%s", data)
	}
	if out := buf.String(); !strings.Contains(out, "provider p: proxy http://corp:3128") || strings.Contains(out, "profile a") {
		t.Errorf("output:\n%s", out)
	}

	// A profile's own proxy beats the provider's; the output lists both.
	buf.Reset()
	if err := connections(&buf, []string{"proxy", "none", "profile", "a"}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "provider p: proxy http://corp:3128") || !strings.Contains(out, "profile a: proxy none") || strings.Contains(out, "profile b") {
		t.Errorf("output:\n%s", out)
	}

	// JSON: what is in force, whether the object sets it, and the level it comes from.
	buf.Reset()
	if err := connections(&buf, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Providers map[string]struct {
			Proxy string
			Own   bool
		}
		Profiles map[string]struct {
			Proxy string
			Own   bool
			Level string
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || strings.Contains(buf.String(), "s3cret") {
		t.Fatalf("json %s: %v", buf.String(), err)
	}
	if pr := got.Providers["p"]; pr.Proxy != "http://corp:3128" || !pr.Own {
		t.Errorf("provider p = %+v", pr)
	}
	if w := got.Providers["w"]; w.Own {
		t.Errorf("provider w = %+v", w)
	}
	if a := got.Profiles["a"]; a.Proxy != "none" || !a.Own || a.Level != "profile" {
		t.Errorf("profile a = %+v", a)
	}
	if b := got.Profiles["b"]; b.Proxy != "http://corp:3128" || b.Own || b.Level != "provider" {
		t.Errorf("profile b = %+v", b)
	}

	// none for the provider: direct, the global proxy is not inherited.
	buf.Reset()
	if err := connections(&buf, []string{"proxy", "none", "provider", "p"}); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(p); !strings.Contains(string(data), "name: p\n        type: codex\n        proxy: none\n") || strings.Contains(string(data), "corp:3128") {
		t.Errorf("config after none:\n%s", data)
	}
	if !strings.Contains(buf.String(), "provider p: proxy none") {
		t.Errorf("output:\n%s", buf.String())
	}

	// inherit removes the override: the global proxy applies again.
	buf.Reset()
	if err := connections(&buf, []string{"proxy", "inherit", "provider", "p"}); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(p); strings.Contains(string(data), "name: p\n        type: codex\n        proxy:") {
		t.Errorf("config after inherit:\n%s", data)
	}
	if strings.Contains(buf.String(), "provider p") {
		t.Errorf("output after inherit:\n%s", buf.String())
	}
	buf.Reset()
	if err := connections(&buf, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || got.Providers["p"].Own || got.Profiles["b"].Proxy != "socks5://me:***@127.0.0.1:1080" || got.Profiles["b"].Level != "global" {
		t.Errorf("after inherit: provider p %+v, profile b %+v (%v)", got.Providers["p"], got.Profiles["b"], err)
	}
}

func TestConnectionsProviderProxyErrors(t *testing.T) {
	p := providerProxyTestConfig(t)
	before, _ := os.ReadFile(p)
	for _, args := range [][]string{
		{"proxy", "none", "provider", "nope"},
		{"proxy", "ftp://x:21", "provider", "p"},
		{"proxy", "none", "provider", "w"}, // the real Claude Code does not go through ccgp
		{"proxy", "none", "provider"},
	} {
		if err := connections(&bytes.Buffer{}, args); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Errorf("config changed by refused edits:\n%s", after)
	}
}

// The new provider of --like has the proxy of the source: it belongs to the
// service, not to the login, and without it the copy would go another way.
func TestProfileAddLikeCarriesProviderProxy(t *testing.T) {
	text := strings.Replace(testConfig, "        api_key_env: ZAI_KEY_TEST\n",
		"        api_key_env: ZAI_KEY_TEST\n        proxy: http://corp:3128\n", 1)
	path, cfg := writeTestConfig(t, text)
	if err := profileAdd(&bytes.Buffer{}, profileAddOpts{Like: "zai"}, path, cfg, creds.Store{}); err != nil {
		t.Fatal(err)
	}
	pr := loadTestConfig(t).Providers["zai3"]
	if pr == nil || pr.Proxy != "http://corp:3128" {
		t.Errorf("provider zai3 = %+v, want the proxy of zai", pr)
	}
}
