package config

import (
	"strings"
	"testing"
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
