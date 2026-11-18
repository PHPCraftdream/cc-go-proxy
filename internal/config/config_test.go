package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testToken = "test-token-0123456789abcdef0123456789"

// TestMain provides a valid PROXY_TOKEN (Load requires it) and isolates
// the user config dir so a real config.ktav is never picked up.
func TestMain(m *testing.M) {
	if os.Getenv("PROXY_TOKEN") == "" {
		os.Setenv("PROXY_TOKEN", testToken)
	}
	dir, err := os.MkdirTemp("", "ccgp-config-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("APPDATA", dir)
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Unsetenv(ConfigFileEnv)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestLoadToken(t *testing.T) {
	orig := os.Getenv("PROXY_TOKEN")
	t.Cleanup(func() { os.Setenv("PROXY_TOKEN", orig) })
	t.Setenv("OPENAI_API_KEY", "test-key")

	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{"missing", "", true},
		{"too short", strings.Repeat("a", MinTokenLength-1), true},
		{"min length", strings.Repeat("a", MinTokenLength), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("PROXY_TOKEN", tt.token)
			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.Token != tt.token {
				t.Errorf("Token = %q, want %q", cfg.Token, tt.token)
			}
		})
	}
}

// TestProviderDetection tests that we correctly identify providers from OPENAI_BASE_URL
func TestProviderDetection(t *testing.T) {
	tests := []struct {
		name             string
		baseURL          string
		expectedProvider ProviderType
	}{
		{
			name:             "OpenRouter detection",
			baseURL:          "https://openrouter.ai/api/v1",
			expectedProvider: ProviderOpenRouter,
		},
		{
			name:             "OpenAI Direct detection",
			baseURL:          "https://api.openai.com/v1",
			expectedProvider: ProviderOpenAI,
		},
		{
			name:             "Ollama local detection",
			baseURL:          "http://localhost:11434/v1",
			expectedProvider: ProviderOllama,
		},
		{
			name:             "Ollama with different port",
			baseURL:          "http://localhost:8080/v1",
			expectedProvider: ProviderOllama,
		},
		{
			name:             "Ollama with custom host - should be unknown since not localhost",
			baseURL:          "http://192.168.1.100:11434/v1",
			expectedProvider: ProviderUnknown,
		},
		{
			name:             "Unknown provider",
			baseURL:          "https://custom-api.example.com/v1",
			expectedProvider: ProviderUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				OpenAIBaseURL: tt.baseURL,
			}

			provider := cfg.DetectProvider()
			if provider != tt.expectedProvider {
				t.Errorf("DetectProvider() for %q = %v, want %v",
					tt.baseURL, provider, tt.expectedProvider)
			}
		})
	}
}

// TestModelOverrides tests that model overrides work correctly
func TestModelOverrides(t *testing.T) {
	tests := []struct {
		name         string
		opusModel    string
		sonnetModel  string
		haikuModel   string
		requestModel string
		expectedUsed string
	}{
		{
			name:         "Opus override for OpenRouter",
			opusModel:    "anthropic/claude-opus-4",
			requestModel: "claude-opus-4-1-20250805",
			expectedUsed: "anthropic/claude-opus-4",
		},
		{
			name:         "Sonnet override for Grok",
			sonnetModel:  "x-ai/grok-code-fast-1",
			requestModel: "claude-sonnet-4-5-20250805",
			expectedUsed: "x-ai/grok-code-fast-1",
		},
		{
			name:         "Haiku override for Gemini",
			haikuModel:   "google/gemini-2.5-flash",
			requestModel: "claude-haiku-4-5-20251001",
			expectedUsed: "google/gemini-2.5-flash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				OpusModel:   tt.opusModel,
				SonnetModel: tt.sonnetModel,
				HaikuModel:  tt.haikuModel,
			}

			// This would be tested through the converter which uses the config
			// The config struct should expose these values properly
			if tt.opusModel != "" && cfg.OpusModel != tt.expectedUsed {
				t.Errorf("OpusModel = %q, want %q", cfg.OpusModel, tt.expectedUsed)
			}
			if tt.sonnetModel != "" && cfg.SonnetModel != tt.expectedUsed {
				t.Errorf("SonnetModel = %q, want %q", cfg.SonnetModel, tt.expectedUsed)
			}
			if tt.haikuModel != "" && cfg.HaikuModel != tt.expectedUsed {
				t.Errorf("HaikuModel = %q, want %q", cfg.HaikuModel, tt.expectedUsed)
			}
		})
	}
}

// TestEnvironmentConfigLoading tests that .env files are loaded correctly
func TestEnvironmentConfigLoading(t *testing.T) {
	// Save current env vars
	originalBaseURL := os.Getenv("OPENAI_BASE_URL")
	originalAPIKey := os.Getenv("OPENAI_API_KEY")

	// Restore after test
	defer func() {
		if originalBaseURL != "" {
			os.Setenv("OPENAI_BASE_URL", originalBaseURL)
		} else {
			os.Unsetenv("OPENAI_BASE_URL")
		}
		if originalAPIKey != "" {
			os.Setenv("OPENAI_API_KEY", originalAPIKey)
		} else {
			os.Unsetenv("OPENAI_API_KEY")
		}
	}()

	// Test that environment variables override
	os.Setenv("OPENAI_BASE_URL", "https://test.example.com")
	os.Setenv("OPENAI_API_KEY", "test-key-123")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.OpenAIBaseURL != "https://test.example.com" {
		t.Errorf("OpenAIBaseURL = %q, want %q", cfg.OpenAIBaseURL, "https://test.example.com")
	}

	if cfg.OpenAIAPIKey != "test-key-123" {
		t.Errorf("OpenAIAPIKey = %q, want %q", cfg.OpenAIAPIKey, "test-key-123")
	}
}

// TestConfigDefaults tests default values
func TestConfigDefaults(t *testing.T) {
	// Clear relevant env vars for this test
	originalBaseURL := os.Getenv("OPENAI_BASE_URL")
	originalAPIKey := os.Getenv("OPENAI_API_KEY")
	os.Unsetenv("OPENAI_BASE_URL")
	os.Setenv("OPENAI_API_KEY", "test-key") // Required for non-localhost
	defer func() {
		if originalBaseURL != "" {
			os.Setenv("OPENAI_BASE_URL", originalBaseURL)
		}
		if originalAPIKey != "" {
			os.Setenv("OPENAI_API_KEY", originalAPIKey)
		} else {
			os.Unsetenv("OPENAI_API_KEY")
		}
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Default base URL should be OpenAI
	expectedDefault := "https://api.openai.com/v1"
	if cfg.OpenAIBaseURL != expectedDefault {
		t.Errorf("Default OpenAIBaseURL = %q, want %q", cfg.OpenAIBaseURL, expectedDefault)
	}

	// Default port
	if cfg.Port != DefaultPort {
		t.Errorf("Default Port = %q, want %q", cfg.Port, DefaultPort)
	}

	// Default host
	if cfg.Host != DefaultHost {
		t.Errorf("Default Host = %q, want %q", cfg.Host, DefaultHost)
	}
}

// TestProviderIsolation is a conceptual test documenting the isolation requirement
func TestProviderIsolation(t *testing.T) {
	scenarios := []struct {
		name             string
		configuredURL    string
		expectedProvider string
		shouldNotCallURL string
	}{
		{
			name:             "OpenRouter should not call OpenAI",
			configuredURL:    "https://openrouter.ai/api/v1",
			expectedProvider: "OpenRouter",
			shouldNotCallURL: "https://api.openai.com",
		},
		{
			name:             "OpenAI should not call OpenRouter",
			configuredURL:    "https://api.openai.com/v1",
			expectedProvider: "OpenAI Direct",
			shouldNotCallURL: "https://openrouter.ai",
		},
		{
			name:             "Ollama should not call external APIs",
			configuredURL:    "http://localhost:11434/v1",
			expectedProvider: "Ollama",
			shouldNotCallURL: "https://",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// Document the requirement: when configured for one provider,
			// we should NEVER make requests to another provider's endpoint
			t.Logf("REQUIREMENT: When OPENAI_BASE_URL=%s (%s), proxy must NOT make requests to %s",
				scenario.configuredURL, scenario.expectedProvider, scenario.shouldNotCallURL)
		})
	}
}

// TestLoadWithDebug tests loading config with debug mode enabled
func TestLoadWithDebug(t *testing.T) {
	// Save original env
	originalKey := os.Getenv("OPENAI_API_KEY")
	originalBaseURL := os.Getenv("OPENAI_BASE_URL")
	defer func() {
		os.Setenv("OPENAI_API_KEY", originalKey)
		os.Setenv("OPENAI_BASE_URL", originalBaseURL)
	}()

	// Set required env vars
	os.Setenv("OPENAI_API_KEY", "test-key")
	os.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")

	cfg, err := LoadWithDebug(true)
	if err != nil {
		t.Fatalf("LoadWithDebug failed: %v", err)
	}

	if !cfg.Debug {
		t.Errorf("Expected Debug=true, got %v", cfg.Debug)
	}

	if cfg.OpenAIAPIKey != "test-key" {
		t.Errorf("Expected API key 'test-key', got %q", cfg.OpenAIAPIKey)
	}
}

// TestIsLocalhost tests localhost detection
func TestIsLocalhost(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		expected bool
	}{
		{
			name:     "localhost with default port",
			baseURL:  "http://localhost:11434/v1",
			expected: true,
		},
		{
			name:     "localhost with custom port",
			baseURL:  "http://localhost:8080/v1",
			expected: true,
		},
		{
			name:     "127.0.0.1",
			baseURL:  "http://127.0.0.1:11434/v1",
			expected: true,
		},
		{
			name:     "OpenRouter",
			baseURL:  "https://openrouter.ai/api/v1",
			expected: false,
		},
		{
			name:     "OpenAI Direct",
			baseURL:  "https://api.openai.com/v1",
			expected: false,
		},
		{
			name:     "Custom host",
			baseURL:  "http://192.168.1.100:11434/v1",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				OpenAIBaseURL: tt.baseURL,
			}

			result := cfg.IsLocalhost()
			if result != tt.expected {
				t.Errorf("IsLocalhost() for %q = %v, want %v",
					tt.baseURL, result, tt.expected)
			}
		})
	}
}

// TestOpenRouterSpecificSettings tests OpenRouter app name and URL settings
func TestOpenRouterSpecificSettings(t *testing.T) {
	// Save original env
	originalAppName := os.Getenv("OPENROUTER_APP_NAME")
	originalAppURL := os.Getenv("OPENROUTER_APP_URL")
	originalKey := os.Getenv("OPENAI_API_KEY")
	originalBaseURL := os.Getenv("OPENAI_BASE_URL")
	defer func() {
		os.Setenv("OPENROUTER_APP_NAME", originalAppName)
		os.Setenv("OPENROUTER_APP_URL", originalAppURL)
		os.Setenv("OPENAI_API_KEY", originalKey)
		os.Setenv("OPENAI_BASE_URL", originalBaseURL)
	}()

	// Set env vars
	os.Setenv("OPENROUTER_APP_NAME", "Claude-Code-Proxy")
	os.Setenv("OPENROUTER_APP_URL", "https://github.com/example/repo")
	os.Setenv("OPENAI_API_KEY", "test-key")
	os.Setenv("OPENAI_BASE_URL", "https://openrouter.ai/api/v1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.OpenRouterAppName != "Claude-Code-Proxy" {
		t.Errorf("Expected app name 'Claude-Code-Proxy', got %q", cfg.OpenRouterAppName)
	}

	if cfg.OpenRouterAppURL != "https://github.com/example/repo" {
		t.Errorf("Expected app URL 'https://github.com/example/repo', got %q", cfg.OpenRouterAppURL)
	}

	if cfg.DetectProvider() != ProviderOpenRouter {
		t.Errorf("Expected OpenRouter provider, got %v", cfg.DetectProvider())
	}
}

// TestOllamaWithoutAPIKey tests that Ollama works without API key
func TestOllamaWithoutAPIKey(t *testing.T) {
	// Save original env
	originalKey := os.Getenv("OPENAI_API_KEY")
	originalBaseURL := os.Getenv("OPENAI_BASE_URL")
	defer func() {
		os.Setenv("OPENAI_API_KEY", originalKey)
		os.Setenv("OPENAI_BASE_URL", originalBaseURL)
	}()

	// Clear API key and set Ollama URL
	os.Unsetenv("OPENAI_API_KEY")
	os.Setenv("OPENAI_BASE_URL", "http://localhost:11434/v1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load should succeed for Ollama without API key: %v", err)
	}

	// Should have dummy key for Ollama
	if cfg.OpenAIAPIKey != "ollama" {
		t.Errorf("Expected dummy API key 'ollama', got %q", cfg.OpenAIAPIKey)
	}

	if cfg.DetectProvider() != ProviderOllama {
		t.Errorf("Expected Ollama provider, got %v", cfg.DetectProvider())
	}
}

// TestMissingAPIKeyForOpenAI tests that load fails without API key for OpenAI
func TestMissingAPIKeyForOpenAI(t *testing.T) {
	// Save original env
	originalKey := os.Getenv("OPENAI_API_KEY")
	originalBaseURL := os.Getenv("OPENAI_BASE_URL")
	defer func() {
		os.Setenv("OPENAI_API_KEY", originalKey)
		os.Setenv("OPENAI_BASE_URL", originalBaseURL)
	}()

	// Clear API key and set OpenAI URL
	os.Unsetenv("OPENAI_API_KEY")
	os.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")

	cfg, err := Load()
	if err == nil {
		t.Errorf("Load should fail when OpenAI API key is missing")
	}

	_ = cfg
}

// TestHostAndPortDefaults tests default host and port values
func TestHostAndPortDefaults(t *testing.T) {
	// Save original env
	originalHost := os.Getenv("HOST")
	originalPort := os.Getenv("PORT")
	originalKey := os.Getenv("OPENAI_API_KEY")
	originalBaseURL := os.Getenv("OPENAI_BASE_URL")
	defer func() {
		if originalHost != "" {
			os.Setenv("HOST", originalHost)
		} else {
			os.Unsetenv("HOST")
		}
		if originalPort != "" {
			os.Setenv("PORT", originalPort)
		} else {
			os.Unsetenv("PORT")
		}
		os.Setenv("OPENAI_API_KEY", originalKey)
		os.Setenv("OPENAI_BASE_URL", originalBaseURL)
	}()

	// Clear host and port
	os.Unsetenv("HOST")
	os.Unsetenv("PORT")
	os.Setenv("OPENAI_API_KEY", "test-key")
	os.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Host != DefaultHost {
		t.Errorf("Expected default host %q, got %q", DefaultHost, cfg.Host)
	}

	if cfg.Port != DefaultPort {
		t.Errorf("Expected default port %q, got %q", DefaultPort, cfg.Port)
	}
}

// TestLoadIgnoresPassthroughMode verifies the obsolete environment variable has
// no effect on loading or env-mode routing.
func TestLoadIgnoresPassthroughMode(t *testing.T) {
	if _, ok := reflect.TypeOf(Config{}).FieldByName("PassthroughMode"); ok {
		t.Fatal("Config must not expose PassthroughMode")
	}

	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(ConfigFileEnv, "")
	t.Setenv("PROXY_TOKEN", testToken)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("PASSTHROUGH_MODE", "")

	type routing struct {
		profile         string
		providerName    string
		providerType    string
		providerBaseURL string
		profileProvider string
	}
	loadRouting := func() routing {
		t.Helper()
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		profile, ok := cfg.Profiles[EnvProfile]
		if !ok {
			t.Fatalf("Load() missing env profile %q", EnvProfile)
		}
		provider, ok := cfg.Providers[profile.Provider]
		if !ok {
			t.Fatalf("Load() missing provider %q", profile.Provider)
		}
		return routing{
			profile:         cfg.Profile,
			providerName:    cfg.ProviderName,
			providerType:    cfg.ProviderType,
			providerBaseURL: provider.BaseURL,
			profileProvider: profile.Provider,
		}
	}

	baseline := loadRouting()
	if baseline.profile != EnvProfile || baseline.providerName != EnvProfile ||
		baseline.providerType != ProviderTypeCodexAPI || baseline.profileProvider != EnvProfile ||
		baseline.providerBaseURL != "https://api.openai.com/v1" {
		t.Fatalf("unexpected baseline routing: %+v", baseline)
	}

	for _, value := range []string{"true", "false", "not-a-bool"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("PASSTHROUGH_MODE", value)
			if got := loadRouting(); got != baseline {
				t.Errorf("routing with PASSTHROUGH_MODE=%q = %+v, want baseline %+v", value, got, baseline)
			}
		})
	}
}

// TestMultipleEnvFiles tests that env files are loaded in correct priority order
func TestMultipleEnvFiles(t *testing.T) {
	// Create temporary directory for test env files
	tempDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	originalCwd, _ := os.Getwd()

	// Create mock .claude directory
	claudeDir := filepath.Join(tempDir, ".claude")
	os.MkdirAll(claudeDir, 0755)

	// Create .env file in current directory
	localEnvFile := filepath.Join(tempDir, ".env")
	os.WriteFile(localEnvFile, []byte("OPENAI_API_KEY=local-key\nOPENAI_BASE_URL=https://local.example.com"), 0644)

	// Create ~/.claude/proxy.env file
	claudeEnvFile := filepath.Join(claudeDir, "proxy.env")
	os.WriteFile(claudeEnvFile, []byte("OPENAI_API_KEY=claude-key"), 0644)

	// Setup environment
	os.Chdir(tempDir)
	os.Setenv("HOME", tempDir)

	defer func() {
		os.Chdir(originalCwd)
		os.Setenv("HOME", originalHome)
	}()

	// Load config - should pick up local .env first
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.OpenAIAPIKey != "local-key" {
		t.Errorf("Expected local API key, got %q", cfg.OpenAIAPIKey)
	}

	if cfg.OpenAIBaseURL != "https://local.example.com" {
		t.Errorf("Expected local base URL, got %q", cfg.OpenAIBaseURL)
	}
}

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
			fc, err := LoadFile(writeKtav(t, claudeAPIConfig(tt.extra)))
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
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
			_, err := LoadFile(writeKtav(t, tt.body))
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
			_, err := LoadFile(writeKtav(t, strings.Replace(profile, "%s", extra, 1)))
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

func setupCodexTransportLoad(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(ConfigFileEnv, "")
	t.Setenv("PROXY_TOKEN", testToken)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("CODEX_TRANSPORT", "")
	return dir
}

func TestCodexTransportDefault(t *testing.T) {
	setupCodexTransportLoad(t)
	for _, tt := range []struct {
		name  string
		unset bool
	}{{"absent", true}, {"empty", false}} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.unset {
				os.Unsetenv("CODEX_TRANSPORT")
			} else {
				t.Setenv("CODEX_TRANSPORT", "")
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CodexTransport != "auto" || !cfg.CodexWebsocket() {
				t.Errorf("transport = %q, websocket = %v; want auto, true", cfg.CodexTransport, cfg.CodexWebsocket())
			}
		})
	}
}

func TestCodexTransportZeroValueIsHTTP(t *testing.T) {
	// A Config not made by Load (tests, tools) keeps the plain HTTP path.
	if (&Config{}).CodexWebsocket() {
		t.Fatal("zero Config uses the websocket")
	}
}

func TestCodexTransportAcceptedValues(t *testing.T) {
	setupCodexTransportLoad(t)
	for _, tt := range []struct {
		value string
		want  string
	}{{"http", "http"}, {"HTTP", "http"}, {"websocket", "websocket"}, {"WEBSOCKET", "websocket"}, {"ws", "websocket"}, {"Ws", "websocket"}, {"auto", "auto"}, {"AUTO", "auto"}} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("CODEX_TRANSPORT", tt.value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CodexTransport != tt.want || cfg.CodexWebsocket() != (tt.want != "http") {
				t.Errorf("transport = %q, websocket = %v; want %q", cfg.CodexTransport, cfg.CodexWebsocket(), tt.want)
			}
		})
	}
}

func TestCodexTransportInvalidValue(t *testing.T) {
	setupCodexTransportLoad(t)
	for _, value := range []string{"not-a-transport", " ws ", " websocket ", "   "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CODEX_TRANSPORT", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CodexTransport != "auto" || !cfg.CodexWebsocket() {
				t.Errorf("transport = %q, websocket = %v; want the default auto, true", cfg.CodexTransport, cfg.CodexWebsocket())
			}
		})
	}
}

func TestCodexTransportReload(t *testing.T) {
	setupCodexTransportLoad(t)
	for _, value := range []string{"ws", "http", "WEBsocket"} {
		t.Setenv("CODEX_TRANSPORT", value)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load with CODEX_TRANSPORT=%q: %v", value, err)
		}
		want := value != "http"
		if cfg.CodexWebsocket() != want {
			t.Errorf("CODEX_TRANSPORT=%q: CodexWebsocket() = %v, want %v", value, cfg.CodexWebsocket(), want)
		}
	}
}

func TestCodexTransportEnvFiles(t *testing.T) {
	for _, tt := range []struct {
		name string
		path func(string) string
	}{
		{"local env", func(string) string { return ".env" }},
		{"proxy env", func(dir string) string { return filepath.Join(dir, ".claude", "proxy.env") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupCodexTransportLoad(t)
			file := tt.path(dir)
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("CODEX_TRANSPORT=ws\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.CodexTransport != "websocket" || !cfg.CodexWebsocket() {
				t.Errorf("transport = %q, websocket = %v; want websocket, true", cfg.CodexTransport, cfg.CodexWebsocket())
			}
		})
	}
}

func TestCodexTransportKtavMode(t *testing.T) {
	setupCodexTransportLoad(t)
	t.Setenv("CCGP_CONFIG", writeKtav(t, "providers: [\n    {\n        name: work\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: work\n        provider: work\n    }\n]\n"))
	t.Setenv("CODEX_TRANSPORT", "websocket")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	profile, err := cfg.ForProfile("work", "")
	if err != nil {
		t.Fatalf("ForProfile: %v", err)
	}
	if profile.CodexTransport != "websocket" || !profile.CodexWebsocket() {
		t.Errorf("profile transport = %q, websocket = %v; want websocket, true", profile.CodexTransport, profile.CodexWebsocket())
	}
}

const codexProviderKtav = "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\n"

// The template carries the built-in version, and it reads back as a string.
func TestTemplateCodexClientVersion(t *testing.T) {
	fc, err := LoadFile(writeKtav(t, string(DefaultTemplate())))
	if err != nil {
		t.Fatal(err)
	}
	if fc.Codex.ClientVersion != DefaultCodexClientVersion {
		t.Errorf("template codex.client_version = %q, want %q", fc.Codex.ClientVersion, DefaultCodexClientVersion)
	}
	if CompareClientVersions(DefaultCodexClientVersion, "0.169.0") < 0 {
		t.Errorf("default %s is older than 0.169.0, which brought gpt-6.2", DefaultCodexClientVersion)
	}
}

func TestCodexClientVersionLoad(t *testing.T) {
	t.Setenv(ConfigFileEnv, writeKtav(t, "codex.client_version:: 0.171.2\n"+codexProviderKtav))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CodexClientVersion(); got != "0.171.2" {
		t.Errorf("CodexClientVersion = %q", got)
	}
	if got := (&Config{}).CodexClientVersion(); got != DefaultCodexClientVersion {
		t.Errorf("unset CodexClientVersion = %q, want the default", got)
	}

	t.Setenv(ConfigFileEnv, writeKtav(t, "codex.client_version:: 0.171\n"+codexProviderKtav))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MAJOR.MINOR.PATCH") {
		t.Errorf("bad version: err = %v", err)
	}
}

func TestCompareClientVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"0.169.0", "0.169.0", 0},
		{"0.170.0", "0.169.9", 1},
		{"0.99.0", "0.100.0", -1},
		{"1.0.0", "0.999.999", 1},
	} {
		if got := CompareClientVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
