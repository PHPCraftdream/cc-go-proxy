package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

func TestSpeedTUIUnsetParityAndRepeat(t *testing.T) {
	h := newTUI(t, parityConfig)
	if err := speed(&bytes.Buffer{}, []string{"codex", "astra", "fast"}); err != nil {
		t.Fatal(err)
	}
	if err := h.a.reload(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(h.cfgPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	h.open("speed", "profile:codex", "family:astra")
	if !strings.Contains(h.view(), "fast (profile rule)") {
		t.Fatalf("inspection: %s", h.view())
	}
	inspected, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInspect, err := filepath.Glob(h.cfgPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, inspected) || len(backups) != len(afterInspect) {
		t.Fatal("inspection wrote config or backup")
	}
	h.open("unset")
	if h.a.errMsg != "" {
		t.Fatal(h.a.errMsg)
	}
	viaTUI, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	backups, err = filepath.Glob(h.cfgPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	h.open("unset")
	if h.a.errMsg != "" {
		t.Fatal(h.a.errMsg)
	}
	if !strings.Contains(h.a.msg, "unchanged") {
		t.Fatalf("repeated unset: %q", h.a.msg)
	}
	repeated, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	afterRepeat, err := filepath.Glob(h.cfgPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaTUI, repeated) || len(backups) != len(afterRepeat) {
		t.Fatal("repeated TUI unset wrote config or backup")
	}
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	if err := speed(&bytes.Buffer{}, []string{"codex", "astra", "unset"}); err != nil {
		t.Fatal(err)
	}
	viaCLI, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaTUI, viaCLI) {
		t.Fatal("TUI and CLI unset produced different configs")
	}
	backups, err = filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := speed(&bytes.Buffer{}, []string{"codex", "astra", "unset"}); err != nil {
		t.Fatal(err)
	}
	repeated, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterRepeat, err = filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaCLI, repeated) || len(backups) != len(afterRepeat) {
		t.Fatal("repeated CLI unset wrote config or backup")
	}
}

func TestSpeedProfileIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ktav")
	body := strings.Replace(parityConfig, "name: zai2\n        provider: zai", "name: zai2\n        provider: codex", 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	if err := speed(&bytes.Buffer{}, []string{"codex", "astra", "fast"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := speed(&out, []string{"zai2", "astra"}); err != nil || !strings.Contains(out.String(), "inherit (client request)") {
		t.Fatalf("other profile: %q %v", out.String(), err)
	}
}

func TestSpeedRulesCommand(t *testing.T) {
	path := peakTestConfig(t)
	run := func(args ...string) (string, error) {
		var b bytes.Buffer
		err := speed(&b, args)
		return b.String(), err
	}
	for _, args := range [][]string{{"pp"}, {"work", "astra"}, {"missing", "astra"}, {"pp", "astra-1", "fast"}, {"pp", "astra", "turbo"}} {
		if _, err := run(args...); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if out, err := run("pp", "astra"); err != nil || !strings.Contains(out, "inherit (client request)") {
		t.Fatalf("inspect: %q %v", out, err)
	}
	if out, err := run("pp", "astra", "fast"); err != nil || !strings.Contains(out, "fast (profile rule)") {
		t.Fatalf("set: %q %v", out, err)
	}
	if out, err := run(); err != nil || !strings.Contains(out, "pp astra: fast") {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err := run("pp", "sol", "standard"); err != nil || !strings.Contains(out, "standard") {
		t.Fatalf("set standard: %q %v", out, err)
	}
	if _, err := run("pp", "astra", "inherit"); err != nil {
		t.Fatal(err)
	}
	if out, err := run("pp", "astra"); err != nil || !strings.Contains(out, "inherit") {
		t.Fatalf("inspect unset: %q %v", out, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := run("pp", "astra", "unset"); err != nil || !strings.Contains(out, "unchanged") {
		t.Fatalf("repeat unset: %q %v", out, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || len(backups) != len(next) {
		t.Fatal("repeat unset wrote config or backup")
	}
	if out, err := run(); err != nil || strings.Contains(out, "astra") || !strings.Contains(out, "pp sol: standard") {
		t.Fatalf("list after unset: %q %v", out, err)
	}
}
