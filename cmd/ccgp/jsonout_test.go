package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/daemon"
	"github.com/PHPCraftdream/cc-go-proxy/internal/preset"
)

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, map[string]int{"b": 2, "a": 1}); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": 1,\n  \"b\": 2\n}\n"
	if buf.String() != want {
		t.Errorf("writeJSON = %q, want %q", buf.String(), want)
	}
}

func TestRenderStatusText(t *testing.T) {
	up := statusServer{Running: true, Version: "ccgp v9", PID: 42, URL: "http://127.0.0.1:1"}
	tests := []struct {
		name string
		data statusData
		want string
	}{
		{
			name: "down, file mode",
			data: statusData{
				Server:    statusServer{URL: "http://127.0.0.1:1"},
				LogFile:   "x.log",
				Config:    statusConfig{Mode: "file", Path: "x.ktav"},
				CredsDir:  "creds",
				Providers: []statusProvider{{Name: "p1", Type: "codex", Login: "not logged in"}},
				Profiles:  []statusProfile{{Name: "alpha", Provider: "p1"}},
			},
			want: "server:  not running at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  x.ktav\n" +
				"creds:   creds\n" +
				"  provider p1           codex   not logged in\n" +
				"  profile  alpha        -> p1\n",
		},
		{
			name: "up, env mode",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "env"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  env-only mode (no config.ktav)\n",
		},
		{
			name: "error mode",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "error", Error: "parse: line 3"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  error: parse: line 3\n",
		},
		{
			name: "missing mode",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "missing", Path: "x.ktav"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  x.ktav (not created yet; created on first run)\n",
		},
		{
			name: "file mode without creds dir",
			data: statusData{
				Server:  up,
				LogFile: "x.log",
				Config:  statusConfig{Mode: "file", Path: "x.ktav"},
			},
			want: "server:  ccgp v9 running (pid 42) at http://127.0.0.1:1\n" +
				"log:     x.log\n" +
				"config:  x.ktav\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderStatusText(&buf, tt.data); err != nil {
				t.Fatal(err)
			}
			if buf.String() != tt.want {
				t.Errorf("renderStatusText =\n%q\nwant\n%q", buf.String(), tt.want)
			}
		})
	}
}

// The fake secret must never reach the JSON payload; status only carries
// non-secret fields (paths, names, login descriptions).
const testSecretToken = "tok-SUPERSECRET-123"

func TestStatusJSON(t *testing.T) {
	t.Setenv("CCGP_TEST_SECRET", "sk-FAKEKEY-not-real-000")
	cfg := &config.Config{
		ConfigFile: filepath.Join(t.TempDir(), "x.ktav"),
		Providers: map[string]*config.Provider{
			"p1": {Name: "p1", Type: config.ProviderTypeAnthropicCompat, BaseURL: "http://127.0.0.1:9/" + testSecretToken, APIKeyEnv: "CCGP_TEST_SECRET"},
		},
		Profiles: map[string]*config.Profile{
			"alpha": {Name: "alpha", Provider: "p1"},
		},
	}
	providers := []statusProvider{{Name: "p1", Type: config.ProviderTypeAnthropicCompat, Login: "key from $CCGP_TEST_SECRET"}}
	profiles := []statusProfile{{Name: "alpha", Provider: "p1"}}
	st := buildStatus("http://127.0.0.1:1", "x.log", &daemon.Health{Version: "ccgp v9", PID: 42}, nil, cfg, nil, filepath.Join(t.TempDir(), "creds"), nil, providers, profiles)

	var buf bytes.Buffer
	if err := writeJSON(&buf, st); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, testSecretToken) {
		t.Error("JSON output contains the planted base URL token")
	}
	if strings.Contains(out, "sk-FAKEKEY-not-real-000") {
		t.Error("JSON output contains the API key env value")
	}

	var got statusData
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Server.Running || got.Server.PID != 42 || got.Server.URL != "http://127.0.0.1:1" {
		t.Errorf("server = %+v", got.Server)
	}
	if got.Config.Mode != "file" || got.Config.Path != cfg.ConfigFile {
		t.Errorf("config = %+v", got.Config)
	}
	if len(got.Providers) != 1 || got.Providers[0].Name != "p1" {
		t.Errorf("providers = %+v", got.Providers)
	}
	if len(got.Profiles) != 1 || got.Profiles[0].Provider != "p1" {
		t.Errorf("profiles = %+v", got.Profiles)
	}
}

func TestBuildProfileRowsAndText(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]*config.Provider{
			"p1": {Name: "p1", Type: config.ProviderTypeCodex, BaseURL: "http://127.0.0.1:9/" + testSecretToken},
			"p2": {Name: "p2", Type: config.ProviderTypeAnthropicCompat, APIKeyEnv: "CCGP_TEST_SECRET"},
		},
		Profiles: map[string]*config.Profile{
			"beta":  {Name: "beta", Provider: "p2"},
			"alpha": {Name: "alpha", Provider: "p1", Models: []config.ModelRule{{Match: "sonnet*", Model: "gpt-x"}}},
		},
	}
	store := creds.Store{Dir: filepath.Join(t.TempDir(), "creds")}
	t.Setenv("CCGP_TEST_SECRET", "sk-FAKEKEY-not-real-000")
	claudeBase := t.TempDir()
	if err := os.Mkdir(filepath.Join(claudeBase, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}

	rows := buildProfileRows(cfg, store, claudeBase)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Name != "alpha" || rows[1].Name != "beta" {
		t.Errorf("row order = %s, %s", rows[0].Name, rows[1].Name)
	}
	if rows[0].MainModel != "gpt-x" || rows[1].MainModel != "-" {
		t.Errorf("main models = %q, %q", rows[0].MainModel, rows[1].MainModel)
	}
	if !rows[0].ClaudeDir || rows[1].ClaudeDir {
		t.Errorf("claudeDir = %v, %v", rows[0].ClaudeDir, rows[1].ClaudeDir)
	}
	if rows[0].Login != "not logged in (ccgp --login p1)" {
		t.Errorf("codex login = %q", rows[0].Login)
	}
	if rows[1].Login != "key from $CCGP_TEST_SECRET" {
		t.Errorf("compat login = %q", rows[1].Login)
	}

	var buf bytes.Buffer
	if err := profileList(&buf, rows); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, testSecretToken) || strings.Contains(out, "sk-FAKEKEY-not-real-000") {
		t.Error("text output contains a planted secret")
	}
	var want bytes.Buffer
	fmt.Fprintf(&want, "%-16s %-16s %-17s %-38s %-16s %s\n", "PROFILE", "PROVIDER", "TYPE", "LOGIN", "MAIN MODEL", "CLAUDE DIR")
	for _, r := range rows {
		dir := "no"
		if r.ClaudeDir {
			dir = "yes"
		}
		fmt.Fprintf(&want, "%-16s %-16s %-17s %-38s %-16s %s\n", r.Name, r.Provider, r.Type, r.Login, r.MainModel, dir)
	}
	if out != want.String() {
		t.Errorf("profileList output mismatch:\n%q\nwant\n%q", out, want.String())
	}
}

func TestProfileListJSONNoSecrets(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]*config.Provider{
			"p1": {Name: "p1", Type: config.ProviderTypeAnthropicCompat, BaseURL: "http://127.0.0.1:9/" + testSecretToken},
		},
		Profiles: map[string]*config.Profile{
			"alpha": {Name: "alpha", Provider: "p1", DefaultModel: "m"},
		},
	}
	rows := buildProfileRows(cfg, creds.Store{Dir: t.TempDir()}, t.TempDir())
	var buf bytes.Buffer
	if err := writeJSON(&buf, profileListData{Profiles: rows}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, testSecretToken) {
		t.Error("JSON output contains the planted base URL token")
	}
	var got profileListData
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Profiles) != 1 || got.Profiles[0].Name != "alpha" || got.Profiles[0].Provider != "p1" {
		t.Errorf("profiles = %+v", got.Profiles)
	}
}

func TestModelsJSON(t *testing.T) {
	sel := codex.Selection{
		Fable: "f", FableWindow: 1000,
		Opus: "o", OpusWindow: 2000,
		Sonnet: "s", SonnetWindow: 3000,
		Haiku: "h", HaikuWindow: 4000,
		Notes: []string{"picked by generation"},
	}
	mp := catalogModelsProfile("alpha", "p1", sel)
	if len(mp.Tiers) != 5 || mp.Tiers[4].Tier != "other" || mp.Tiers[4].Model != "s" || mp.Tiers[4].ContextWindow != 3000 {
		t.Errorf("catalog tiers = %+v", mp.Tiers)
	}
	if len(mp.Notes) != 1 || mp.Notes[0] != "picked by generation" {
		t.Errorf("notes = %+v", mp.Notes)
	}
	pp := presetModelsProfile("beta", "p2", preset.Preset{
		Name: "zai",
		Rules: []config.ModelRule{
			{Match: "*fable*", Model: "f-model", ContextWindow: 1000},
			{Match: "*", Model: "s-model", ContextWindow: 3000},
		},
	})
	if pp.Preset != "zai" || len(pp.Tiers) != 2 || pp.Tiers[0].Tier != "*fable*" {
		t.Errorf("preset profile = %+v", pp)
	}

	var buf bytes.Buffer
	if err := writeJSON(&buf, modelsData{Profiles: []modelsProfile{mp, pp}}); err != nil {
		t.Fatal(err)
	}
	var got modelsData
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(got.Profiles))
	}
	if got.Profiles[0].Tiers[2].Tier != "sonnet" || got.Profiles[0].Tiers[2].Model != "s" {
		t.Errorf("sonnet tier = %+v", got.Profiles[0].Tiers[2])
	}
	if got.Profiles[0].Notes[0] != "picked by generation" {
		t.Errorf("notes = %+v", got.Profiles[0].Notes)
	}
	if got.Profiles[1].Preset != "zai" || got.Profiles[1].Tiers[1].Model != "s-model" {
		t.Errorf("preset profile = %+v", got.Profiles[1])
	}

	// --models apply --json is rejected before any catalog fetch.
	err := models(&config.Config{
		ConfigFile: "x.ktav",
		Providers:  map[string]*config.Provider{},
		Profiles:   map[string]*config.Profile{},
	}, []string{"apply", "--json"})
	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("models(apply, --json) error = %v, want a --json usage error", err)
	}
}

func TestStatusUnknownFlag(t *testing.T) {
	if err := status([]string{"--bogus"}); err == nil {
		t.Error("status(--bogus) = nil error, want unknown-flag error")
	}
}
