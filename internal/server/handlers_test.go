package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

func TestAPIKeyValidation(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Errorf("upstream request = %s %s, want POST /chat/completions", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	tests := []struct {
		name          string
		configuredKey string
		requestKey    string
		wantStatus    int
		wantUpstream  bool
	}{
		{name: "matching key", configuredKey: "test-key", requestKey: "test-key", wantStatus: http.StatusOK, wantUpstream: true},
		{name: "mismatched key", configuredKey: "test-key", requestKey: "wrong-key", wantStatus: http.StatusUnauthorized},
		{name: "no key configured", requestKey: "any-key", wantStatus: http.StatusOK, wantUpstream: true},
		{name: "missing key", configuredKey: "test-key", wantStatus: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				AnthropicAPIKey: tt.configuredKey,
				OpenAIBaseURL:   upstream.URL,
			}
			app := fiber.New()
			app.Post("/v1/messages", func(c *fiber.Ctx) error { return handleMessages(c, cfg) })

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-4o-mini","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			if tt.requestKey != "" {
				req.Header.Set("x-api-key", tt.requestKey)
			}
			previousHits := upstreamHits.Load()
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			hitDelta := upstreamHits.Load() - previousHits
			wantHitDelta := int64(0)
			if tt.wantUpstream {
				wantHitDelta = 1
			}
			if hitDelta != wantHitDelta {
				t.Errorf("upstream hit delta = %d, want %d", hitDelta, wantHitDelta)
			}
			if tt.wantUpstream {
				var body map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("decode accepted response: %v", err)
				}
				if body["type"] != "message" {
					t.Errorf("response type = %v, want message", body["type"])
				}
			}
		})
	}
}

// TestOpenRouterConfiguration tests OpenRouter-specific configuration
func TestOpenRouterConfiguration(t *testing.T) {
	cfg := &config.Config{
		OpenRouterAppName: "Claude-Code-Proxy",
		OpenRouterAppURL:  "https://github.com/example/repo",
		OpenAIBaseURL:     "https://openrouter.ai/api/v1",
	}

	if cfg.OpenRouterAppName != "Claude-Code-Proxy" {
		t.Errorf("Expected app name 'Claude-Code-Proxy'")
	}

	if cfg.OpenRouterAppURL != "https://github.com/example/repo" {
		t.Errorf("Expected app URL 'https://github.com/example/repo'")
	}

	if cfg.DetectProvider() != config.ProviderOpenRouter {
		t.Errorf("Expected OpenRouter provider")
	}
}

// TestProviderDetectionForHandlers tests provider detection in handler context
func TestProviderDetectionForHandlers(t *testing.T) {
	tests := []struct {
		name             string
		baseURL          string
		expectedProvider config.ProviderType
	}{
		{
			name:             "OpenRouter",
			baseURL:          "https://openrouter.ai/api/v1",
			expectedProvider: config.ProviderOpenRouter,
		},
		{
			name:             "OpenAI",
			baseURL:          "https://api.openai.com/v1",
			expectedProvider: config.ProviderOpenAI,
		},
		{
			name:             "Ollama",
			baseURL:          "http://localhost:11434/v1",
			expectedProvider: config.ProviderOllama,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				OpenAIBaseURL: tt.baseURL,
			}

			provider := cfg.DetectProvider()
			if provider != tt.expectedProvider {
				t.Errorf("Expected provider %v, got %v", tt.expectedProvider, provider)
			}
		})
	}
}

// captureStdout returns what fn prints to os.Stdout. fn must not call
// t.Fatal: the writer is restored only after it returns.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	return <-done
}

// A body that cannot be parsed is logged with its size, never in full: it can
// hold a whole conversation.
func TestMessagesParseFailureLogsErrorAndSizeNotBody(t *testing.T) {
	app := fiber.New()
	cfg := &config.Config{}
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return handleMessages(c, cfg) })

	body := `{"model":"claude-x","messages":"` + strings.Repeat("secret prompt ", 200) + `"}`
	old := logOut
	var buf syncBuf
	logOut = &buf
	t.Cleanup(func() { logOut = old })

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	log := buf.String()
	if !strings.Contains(log, "Failed to parse request body") || !strings.Contains(log, fmt.Sprintf("(body: %d bytes)", len(body))) {
		t.Errorf("log lacks the error and the size: %q", log)
	}
	if strings.Contains(log, "secret prompt") {
		t.Errorf("the body was logged: %q", log)
	}
}

// In debug mode the raw body is still printed.
func TestMessagesParseFailureLogsBodyInDebug(t *testing.T) {
	app := fiber.New()
	cfg := &config.Config{Debug: true}
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return handleMessages(c, cfg) })

	body := `{"model":"claude-x","messages":"not an array"}`
	old := logOut
	var buf syncBuf
	logOut = &buf
	t.Cleanup(func() { logOut = old })

	var callErr error
	var status int
	out := captureStdout(t, func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		callErr = err
		if err == nil {
			status = resp.StatusCode
		}
	})
	if callErr != nil {
		t.Fatal(callErr)
	}
	if status != 400 {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.Contains(out, "Raw body") || !strings.Contains(out, body) {
		t.Errorf("debug output lacks the raw body: %q", out)
	}
	if !strings.Contains(buf.String(), "(body: ") {
		t.Errorf("log lacks the body size: %q", buf.String())
	}
}
