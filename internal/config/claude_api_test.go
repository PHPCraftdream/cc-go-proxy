package config

import (
	"strings"
	"testing"
)

func claudeAPIConfig(provider string) string {
	return "providers: [\n    {\n        name: api\n        type: claude_api\n" + provider + "    }\n]\nprofiles: [\n    {\n        name: api\n        provider: api\n    }\n]\n"
}

func TestLoadFileClaudeAPI(t *testing.T) {
	for _, tt := range []struct{ name, extra, url string }{
		{"the official server by default", "", OfficialAnthropicURL},
		{"another server", "        base_url: https://gw.example/anthropic\n", "https://gw.example/anthropic"},
		{"a key from the environment", "        api_key_env: MY_ANTHROPIC_KEY\n", OfficialAnthropicURL},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fc, err := loadFile(writeKtav(t, claudeAPIConfig(tt.extra)))
			if err != nil {
				t.Fatalf("loadFile: %v", err)
			}
			pr := fc.Providers[0]
			if pr.Type != ProviderTypeClaudeAPI || pr.EffectiveBaseURL() != tt.url {
				t.Errorf("provider = %+v, server %q, want %q", pr, pr.EffectiveBaseURL(), tt.url)
			}
		})
	}
}

func TestLoadFileClaudeAPIErrors(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"effort map", claudeAPIConfig("        effort_map: {\n            medium: high\n        }\n"), "takes no effort_map or proxy"},
		{"proxy", claudeAPIConfig("        proxy: none\n"), "takes no effort_map or proxy"},
		{"server without a scheme", claudeAPIConfig("        base_url: gw.example\n"), "base_url must be an http(s) URL"},
		{"server of another kind", claudeAPIConfig("        base_url: ftp://gw.example\n"), "base_url must be an http(s) URL"},
		{"peak hours", claudeAPIConfig("        peak_tz: UTC\n"), "takes no peak hours"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadFile(writeKtav(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}

	profile := "providers: [\n    {\n        name: api\n        type: claude_api\n    }\n]\nprofiles: [\n    {\n        name: api\n        provider: api\n%s    }\n]\n"
	for name, extra := range map[string]string{
		"models":         "        models: [\n            {\n                match: *\n                model: m\n            }\n        ]\n",
		"default_model":  "        default_model: m\n",
		"context_window": "        context_window: 1000\n",
		"proxy":          "        proxy: none\n",
	} {
		t.Run("profile "+name, func(t *testing.T) {
			_, err := loadFile(writeKtav(t, strings.Replace(profile, "%s", extra, 1)))
			if err == nil || !strings.Contains(err.Error(), "does not go through ccgp") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestIsOwnType(t *testing.T) {
	for typ, want := range map[string]bool{
		ProviderTypeClaude: true, ProviderTypeClaudeAPI: true,
		ProviderTypeCodex: false, ProviderTypeCodexAPI: false, ProviderTypeAnthropicCompat: false, "": false,
	} {
		if got := IsOwnType(typ); got != want {
			t.Errorf("IsOwnType(%q) = %v, want %v", typ, got, want)
		}
	}
	// Only claude_api has a default server.
	for _, typ := range []string{ProviderTypeClaude, ProviderTypeCodexAPI, ProviderTypeAnthropicCompat} {
		if got := (&Provider{Type: typ}).EffectiveBaseURL(); got != "" {
			t.Errorf("%s without a base_url has the server %q", typ, got)
		}
	}
}

// A claude_api profile has no URL token, no rules and no route: the server
// config for it is never asked for, but the loader must still hand it over whole.
func TestClaudeAPIKeepsItsBaseURLInTheConfig(t *testing.T) {
	t.Setenv(ConfigFileEnv, writeKtav(t, claudeAPIConfig("        base_url: https://gw.example\n")))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if pr := cfg.Providers["api"]; pr == nil || pr.BaseURL != "https://gw.example" {
		t.Errorf("provider = %+v", pr)
	}
	t.Setenv(ConfigFileEnv, writeKtav(t, claudeAPIConfig("")))
	if cfg, err = Load(); err != nil {
		t.Fatal(err)
	}
	if pr := cfg.Providers["api"]; pr == nil || pr.BaseURL != "" {
		t.Errorf("a claude_api without a base_url must keep it empty in the config, got %+v", pr)
	}
}
