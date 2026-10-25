package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

func whichTestConfig() *config.Config {
	return &config.Config{
		ConfigFile: "config.ktav", // file mode
		Providers: map[string]*config.Provider{
			"codex": {Name: "codex", Type: config.ProviderTypeCodex},
		},
		Profiles: map[string]*config.Profile{
			"codex": {
				Name:     "codex",
				Provider: "codex",
				Models: []config.ModelRule{
					{Match: "*haiku*", Model: "mini", ContextWindow: 272000},
					{Match: "claude-opus-*", Model: "big", ContextWindow: 0},
				},
			},
			"empty": {Name: "empty", Provider: "codex"},
		},
	}
}

func TestWhichRouteTiers(t *testing.T) {
	cfg := whichTestConfig()
	for _, tc := range []struct{ model, backend, matchedBy string }{
		{"claude-3-5-haiku-20241022", "mini", "rule *haiku*"},
		{"claude-opus-4-1", "big", "rule claude-opus-*"},
		{"claude-sonnet-4", "gpt-5", "tier setting"},
		{"gpt-4o", "gpt-4o", "passthrough"},
	} {
		ri, err := whichRoute(cfg, "codex", tc.model)
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		if ri.BackendModel != tc.backend {
			t.Errorf("%s: backend = %q, want %q", tc.model, ri.BackendModel, tc.backend)
		}
		if ri.MatchedBy != tc.matchedBy {
			t.Errorf("%s: matchedBy = %q, want %q", tc.model, ri.MatchedBy, tc.matchedBy)
		}
	}
}

func TestWhichRouteDefaultModel(t *testing.T) {
	cfg := whichTestConfig()
	cfg.Profiles["codex"].DefaultModel = "fallback-model"
	ri, err := whichRoute(cfg, "codex", "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if ri.BackendModel != "fallback-model" || ri.MatchedBy != "default_model" {
		t.Errorf("got %q / %q, want fallback-model / default_model", ri.BackendModel, ri.MatchedBy)
	}
}

func TestWhichRouteContextWindow(t *testing.T) {
	cfg := whichTestConfig()
	if ri, _ := whichRoute(cfg, "codex", "claude-3-5-haiku-20241022"); ri.ContextWindow != "272000" {
		t.Errorf("haiku window = %q, want 272000", ri.ContextWindow)
	}
	// A rule without a window falls back to the smallest window of the profile.
	if ri, _ := whichRoute(cfg, "codex", "claude-opus-4-1"); ri.ContextWindow != "272000" {
		t.Errorf("opus window = %q, want the profile fallback 272000", ri.ContextWindow)
	}
	// No window anywhere in the profile: unknown.
	if ri, err := whichRoute(cfg, "empty", "gpt-4o"); err != nil || ri.ContextWindow != "unknown" {
		t.Errorf("empty profile window = %q (%v), want unknown", ri.ContextWindow, err)
	}
}

func TestWhichRouteUnknownProfile(t *testing.T) {
	cfg := whichTestConfig()
	_, err := whichRoute(cfg, "nope", "claude-sonnet-4")
	if err == nil {
		t.Fatal("expected error")
	}
	want := `unknown profile "nope" (configured: codex, empty)`
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

func TestWhichRouteJSONShape(t *testing.T) {
	cfg := whichTestConfig()
	ri, err := whichRoute(cfg, "codex", "claude-3-5-haiku-20241022")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, ri); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"profile":       "codex",
		"provider":      "codex",
		"providerType":  "codex",
		"requested":     "claude-3-5-haiku-20241022",
		"backendModel":  "mini",
		"contextWindow": "272000",
		"matchedBy":     "rule *haiku*",
	}
	for k, v := range want {
		if doc[k] != v {
			t.Errorf("%s = %v, want %q", k, doc[k], v)
		}
	}
	for k := range doc {
		if !strings.Contains(strings.Join([]string{"profile", "provider", "providerType", "requested", "backendModel", "contextWindow", "matchedBy"}, " "), k) {
			t.Errorf("unexpected JSON field %q", k)
		}
	}
}

func TestWhichRouteTextOutput(t *testing.T) {
	cfg := whichTestConfig()
	ri, err := whichRoute(cfg, "empty", "gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := renderRouteText(&buf, ri); err != nil {
		t.Fatal(err)
	}
	// Values start in one column, after the longest label ("context window:").
	row := func(label, value string) string {
		return label + strings.Repeat(" ", 16-len(label)) + value + "\n"
	}
	want := row("profile:", "empty") +
		row("provider:", "codex (codex)") +
		row("requested:", "gpt-4o") +
		row("backend model:", "gpt-4o") +
		row("context window:", "unknown") +
		row("matched by:", "passthrough")
	if buf.String() != want {
		t.Errorf("output:\n%q\nwant:\n%q", buf.String(), want)
	}
}

func TestParseWhichArgs(t *testing.T) {
	p, m, j, err := parseWhichArgs([]string{"codex", "--json", "claude-opus-4-1"})
	if err != nil || p != "codex" || m != "claude-opus-4-1" || !j {
		t.Errorf("got %q %q %v %v", p, m, j, err)
	}
	p, m, j, err = parseWhichArgs([]string{"codex", "x"})
	if err != nil || p != "codex" || m != "x" || j {
		t.Errorf("got %q %q %v %v", p, m, j, err)
	}
}

// Env-mode tier overrides apply in env mode only; file mode ignores them.
func TestWhichRouteEnvModeTierOverrides(t *testing.T) {
	env := &config.Config{
		ProviderName: "env", ProviderType: config.ProviderTypeCodexAPI,
		OpusModel: "my-opus", HaikuModel: "my-haiku", SonnetModel: "my-sonnet",
		Profiles: map[string]*config.Profile{"default": {Name: "default"}},
	}
	for model, want := range map[string]string{
		"claude-opus-4-1": "my-opus", "claude-haiku-4": "my-haiku", "claude-sonnet-4": "my-sonnet",
	} {
		ri, err := whichRoute(env, "default", model)
		if err != nil || ri.BackendModel != want {
			t.Errorf("%s: backend %q (%v), want %q", model, ri.BackendModel, err, want)
		}
	}
	// File mode (the tier fallback above is the converter default, not an override).
	ri, err := whichRoute(whichTestConfig(), "empty", "claude-opus-4-1")
	if err != nil || ri.BackendModel != "gpt-5" {
		t.Errorf("file mode opus: %q (%v), want gpt-5", ri.BackendModel, err)
	}
}

func TestWhichCmdUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"onlyone"},
		{"a", "b", "c"},
		{"--json"},
	} {
		if err := whichCmd(args); err == nil || !strings.Contains(err.Error(), "usage: ccgp --which <profile> <model> [--json]") {
			t.Errorf("whichCmd(%v) = %v, want usage error", args, err)
		}
	}
}
