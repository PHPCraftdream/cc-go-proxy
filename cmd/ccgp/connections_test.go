package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

func connectionsTestConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	body := "listen.port: 60093\n\nproviders: [\n    {\n        name: p\n        type: codex\n    }\n]\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, p)
	return p
}

func TestConnectionsShowsDefaults(t *testing.T) {
	connectionsTestConfig(t)
	var buf bytes.Buffer
	if err := connections(&buf, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "active: 7") || !strings.Contains(out, "total:  10") || strings.Count(out, "(default)") != 2 {
		t.Errorf("output:\n%s", out)
	}
}

func TestConnectionsSetsConfig(t *testing.T) {
	p := connectionsTestConfig(t)
	var buf bytes.Buffer
	if err := connections(&buf, []string{"active", "3", "total", "12"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "connections.active: 3\nconnections.total: 12\n") {
		t.Errorf("config:\n%s", data)
	}
	if !strings.Contains(buf.String(), "config.ktav updated") {
		t.Errorf("output:\n%s", buf.String())
	}

	buf.Reset()
	if err := connections(&buf, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Active, Total int
		ActiveDefault bool `json:"active_default"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || got.Active != 3 || got.Total != 12 || got.ActiveDefault {
		t.Errorf("json %s: %+v %v", buf.String(), got, err)
	}
}

func TestConnectionsRejectsBadInput(t *testing.T) {
	p := connectionsTestConfig(t)
	before, _ := os.ReadFile(p)
	for _, args := range [][]string{{"active"}, {"active", "x"}, {"active", "-1"}, {"lanes", "3"}, {"active", "12"}} {
		if err := connections(&bytes.Buffer{}, args); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	after, _ := os.ReadFile(p)
	if string(after) != string(before) {
		t.Errorf("config changed by refused edits:\n%s", after)
	}
}

func TestConnectionsProxy(t *testing.T) {
	p := connectionsTestConfig(t)
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	var buf bytes.Buffer
	if err := connections(&buf, []string{"proxy", "socks5://me:s3cret@127.0.0.1:1080"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "connections.proxy: socks5://me:s3cret@127.0.0.1:1080\n") {
		t.Errorf("config:\n%s", data)
	}
	if out := buf.String(); !strings.Contains(out, "proxy:  socks5://me:***@127.0.0.1:1080") || strings.Contains(out, "s3cret") {
		t.Errorf("output shows the password or no proxy:\n%s", out)
	}
	buf.Reset()
	if err := connections(&buf, []string{"--json"}); err != nil || strings.Contains(buf.String(), "s3cret") || !strings.Contains(buf.String(), `"proxy": "socks5://me:***@127.0.0.1:1080"`) {
		t.Errorf("json %s, %v", buf.String(), err)
	}
	buf.Reset()
	if err := connections(&buf, []string{"proxy", "off"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(p); strings.Contains(string(data), "connections.proxy") || !strings.Contains(buf.String(), "proxy:  none") {
		t.Errorf("proxy off kept the line:\n%s\n%s", data, buf.String())
	}
	if err := connections(&bytes.Buffer{}, []string{"proxy", "ftp://x:21"}); err == nil {
		t.Error("ftp proxy accepted")
	}
	if err := connections(&bytes.Buffer{}, []string{"proxy"}); err == nil {
		t.Error("proxy without a value accepted")
	}
}

func profileProxyTestConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	body := "connections.proxy: socks5://me:s3cret@127.0.0.1:1080\n\nproviders: [\n    {\n        name: p\n        type: codex\n    }\n]\n\n" +
		"profiles: [\n    {\n        name: a\n        provider: p\n    }\n    {\n        name: b\n        provider: p\n    }\n]\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, p)
	t.Cleanup(func() { proxydial.SetDefault(nil) })
	return p
}

func TestConnectionsProfileProxy(t *testing.T) {
	p := profileProxyTestConfig(t)
	var buf bytes.Buffer
	// An own proxy for profile a.
	if err := connections(&buf, []string{"proxy", "http://other:3128", "profile", "a"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "name: a\n        provider: p\n        proxy: http://other:3128\n") {
		t.Fatalf("config:\n%s", data)
	}
	if !strings.Contains(string(data), "connections.proxy: socks5://me:s3cret@127.0.0.1:1080\n") {
		t.Errorf("the global proxy changed:\n%s", data)
	}
	if out := buf.String(); !strings.Contains(out, "profile a: proxy http://other:3128") || strings.Contains(out, "profile b") {
		t.Errorf("output:\n%s", out)
	}

	// none: direct, the global proxy is not inherited.
	buf.Reset()
	if err := connections(&buf, []string{"proxy", "none", "profile", "b"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p)
	if !strings.Contains(string(data), "name: b\n        provider: p\n        proxy: none\n") {
		t.Fatalf("config:\n%s", data)
	}
	if !strings.Contains(buf.String(), "profile b: proxy none") {
		t.Errorf("output:\n%s", buf.String())
	}

	// JSON: effective proxy of every profile, and whether it is the profile's own.
	buf.Reset()
	if err := connections(&buf, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Proxy    string
		Profiles map[string]struct {
			Proxy string
			Own   bool
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || strings.Contains(buf.String(), "s3cret") {
		t.Fatalf("json %s: %v", buf.String(), err)
	}
	if got.Profiles["a"].Proxy != "http://other:3128" || !got.Profiles["a"].Own || got.Profiles["b"].Proxy != "none" || !got.Profiles["b"].Own {
		t.Errorf("profiles = %+v", got.Profiles)
	}

	// inherit removes the override: back to the global proxy.
	buf.Reset()
	if err := connections(&buf, []string{"proxy", "inherit", "profile", "a"}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p)
	if strings.Contains(string(data), "http://other:3128") || !strings.Contains(string(data), "proxy: none") {
		t.Errorf("config after inherit:\n%s", data)
	}
	buf.Reset()
	if err := connections(&buf, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || got.Profiles["a"].Own || got.Profiles["a"].Proxy != "socks5://me:***@127.0.0.1:1080" {
		t.Errorf("profile a after inherit = %+v (%v)", got.Profiles["a"], err)
	}

	// The global form still works next to the profile ones; "off" removes it.
	if err := connections(&bytes.Buffer{}, []string{"proxy", "off"}); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(p); strings.Contains(string(data), "connections.proxy") || !strings.Contains(string(data), "proxy: none") {
		t.Errorf("config after global off:\n%s", data)
	}
}

func TestConnectionsProfileProxyErrors(t *testing.T) {
	p := profileProxyTestConfig(t)
	before, _ := os.ReadFile(p)
	for _, args := range [][]string{
		{"proxy", "none", "profile", "nope"},
		{"proxy", "ftp://x:21", "profile", "a"},
		{"proxy", "ftp://x:21"},
		{"proxy", "none", "profile"},
	} {
		if err := connections(&bytes.Buffer{}, args); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Errorf("config changed by refused edits:\n%s", after)
	}
}
