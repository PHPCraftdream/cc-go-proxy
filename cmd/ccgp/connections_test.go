package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
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
