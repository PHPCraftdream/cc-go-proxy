package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/gofiber/fiber/v2"
)

// TestMaxTokensParameterErrorDetection tests the error detection logic for parameter errors
func TestMaxTokensParameterErrorDetection(t *testing.T) {
	tests := []struct {
		name         string
		errorMsg     string
		shouldDetect bool
		description  string
	}{
		{
			name:         "max_completion_tokens unsupported",
			errorMsg:     "Error: unsupported parameter 'max_completion_tokens'",
			shouldDetect: true,
			description:  "Should detect unsupported max_completion_tokens",
		},
		{
			name:         "max_tokens invalid",
			errorMsg:     "invalid parameter: max_tokens not supported for this model",
			shouldDetect: true,
			description:  "Should detect invalid max_tokens",
		},
		{
			name:         "parameter error lowercase",
			errorMsg:     "parameter 'max_completion_tokens' is not supported",
			shouldDetect: true,
			description:  "Should handle lowercase parameter errors",
		},
		{
			name:         "OpenWebUI style error",
			errorMsg:     "500: 'max_completion_tokens' is not a valid OpenAI parameter",
			shouldDetect: true,
			description:  "Should detect OpenWebUI style errors",
		},
		{
			name:         "unrelated error",
			errorMsg:     "Authentication failed",
			shouldDetect: false,
			description:  "Should not detect unrelated errors",
		},
		{
			name:         "timeout error",
			errorMsg:     "request timeout",
			shouldDetect: false,
			description:  "Should not detect timeout errors",
		},
		{
			name:         "empty error message",
			errorMsg:     "",
			shouldDetect: false,
			description:  "Should handle empty error messages",
		},
		{
			name:         "wrong parameter name",
			errorMsg:     "unsupported parameter 'max_input_tokens'",
			shouldDetect: false,
			description:  "Should not detect errors for wrong parameters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isMaxTokensParameterError(tt.errorMsg)
			if result != tt.shouldDetect {
				t.Errorf("%s: got %v, want %v", tt.description, result, tt.shouldDetect)
			}
		})
	}
}

func TestModelCapabilityCacheStructure(t *testing.T) {
	scope := t.Name() + "/" + t.TempDir()
	key := config.CacheKey{
		BaseURL: "https://api.openai.com/v1/" + scope,
		Model:   "gpt-5",
	}
	reconstructed := config.CacheKey{
		BaseURL: "https://api.openai.com/v1/" + scope,
		Model:   "gpt-5",
	}
	otherProvider := config.CacheKey{
		BaseURL: "https://openrouter.ai/api/v1/" + scope,
		Model:   "gpt-5",
	}

	want := &config.ModelCapabilities{UsesMaxCompletionTokens: true}
	config.SetModelCapabilities(key, want)
	got := config.GetModelCapabilities(reconstructed)
	if got == nil {
		t.Fatal("same provider and model should retrieve the stored cache entry")
	}
	if !got.UsesMaxCompletionTokens {
		t.Fatal("retrieved cache entry has the wrong capability")
	}
	if got := config.GetModelCapabilities(otherProvider); got != nil {
		t.Fatalf("different provider unexpectedly retrieved cache entry: %+v", got)
	}
}

func TestCacheHitAndMiss(t *testing.T) {
	scope := t.Name() + "/" + t.TempDir()
	key := config.CacheKey{
		BaseURL: "https://api.openai.com/v1/" + scope,
		Model:   "gpt-5",
	}

	if caps := config.GetModelCapabilities(key); caps != nil {
		t.Fatalf("new cache key unexpectedly hit: %+v", caps)
	}

	want := &config.ModelCapabilities{UsesMaxCompletionTokens: true}
	config.SetModelCapabilities(key, want)

	got := config.GetModelCapabilities(key)
	if got == nil {
		t.Fatal("cache should hit after setting capabilities")
	}
	if !got.UsesMaxCompletionTokens {
		t.Fatalf("cached capability = %v, want true", got.UsesMaxCompletionTokens)
	}
}

func TestPerModelPerProviderScoping(t *testing.T) {
	scope := t.Name() + "/" + t.TempDir()
	providerA := "https://api.openai.com/v1/" + scope
	providerB := "https://openrouter.ai/api/v1/" + scope
	entries := []struct {
		key   config.CacheKey
		value bool
	}{
		{config.CacheKey{BaseURL: providerA, Model: "shared-model"}, true},
		{config.CacheKey{BaseURL: providerB, Model: "shared-model"}, false},
		{config.CacheKey{BaseURL: providerA, Model: "different-model"}, false},
	}

	for _, entry := range entries {
		config.SetModelCapabilities(entry.key, &config.ModelCapabilities{
			UsesMaxCompletionTokens: entry.value,
		})
	}
	for _, entry := range entries {
		got := config.GetModelCapabilities(entry.key)
		if got == nil {
			t.Errorf("cache miss for provider %q, model %q", entry.key.BaseURL, entry.key.Model)
			continue
		}
		if got.UsesMaxCompletionTokens != entry.value {
			t.Errorf("provider %q, model %q capability = %v, want %v",
				entry.key.BaseURL, entry.key.Model, got.UsesMaxCompletionTokens, entry.value)
		}
	}
}

func TestCacheConcurrency(t *testing.T) {
	const workers, iterations = 10, 100
	scope := t.Name() + "/" + t.TempDir()
	keys := make([]config.CacheKey, workers)
	wants := make([]bool, workers)
	for i := range keys {
		keys[i] = config.CacheKey{
			BaseURL: "https://api.openai.com/v1/" + scope,
			Model:   fmt.Sprintf("model-%d", i),
		}
		wants[i] = i%2 == 0
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errors := make(chan string, workers*iterations)
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				// Each call publishes a fresh immutable value.
				config.SetModelCapabilities(keys[id], &config.ModelCapabilities{
					UsesMaxCompletionTokens: wants[id],
				})
			}
		}(i)
		go func(id int) {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				got := config.GetModelCapabilities(keys[id])
				if got == nil {
					continue // The corresponding writer may not have published yet.
				}
				if got.UsesMaxCompletionTokens != wants[id] {
					errors <- fmt.Sprintf("model %q capability = %v, want %v",
						keys[id].Model, got.UsesMaxCompletionTokens, wants[id])
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}

	for i, key := range keys {
		got := config.GetModelCapabilities(key)
		if got == nil {
			t.Errorf("no final cache value for model %q", key.Model)
			continue
		}
		if got.UsesMaxCompletionTokens != wants[i] {
			t.Errorf("final model %q capability = %v, want %v", key.Model, got.UsesMaxCompletionTokens, wants[i])
		}
	}
}

func TestCacheTimestampTracking(t *testing.T) {
	scope := t.Name() + "/" + t.TempDir()
	key := config.CacheKey{
		BaseURL: "https://api.openai.com/v1/" + scope,
		Model:   "gpt-5",
	}
	ancient := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

	config.SetModelCapabilities(key, &config.ModelCapabilities{
		UsesMaxCompletionTokens: true,
		LastChecked:             ancient,
	})
	previous := config.GetModelCapabilities(key)
	if previous == nil {
		t.Fatal("cache entry missing after initial set")
	}
	previousChecked := previous.LastChecked

	before := time.Now()
	config.SetModelCapabilities(key, &config.ModelCapabilities{
		UsesMaxCompletionTokens: false,
		LastChecked:             ancient,
	})
	after := time.Now()
	got := config.GetModelCapabilities(key)
	if got == nil {
		t.Fatal("cache entry missing after replacement")
	}
	if got.UsesMaxCompletionTokens {
		t.Fatal("replacement did not store the new capability value")
	}
	if got.LastChecked.Before(before) || got.LastChecked.After(after) {
		t.Errorf("replacement LastChecked = %v, want between %v and %v", got.LastChecked, before, after)
	}
	if got.LastChecked.Before(previousChecked) {
		t.Errorf("replacement LastChecked = %v, want not earlier than prior value %v", got.LastChecked, previousChecked)
	}
}

// TestErrorDetectionBroadMatching tests broad keyword matching for various error formats
func TestErrorDetectionBroadMatching(t *testing.T) {
	// These should ALL match: require both parameter indicator AND parameter name
	shouldMatch := []string{
		"parameter 'max_completion_tokens'",
		"Parameter 'max_completion_tokens'",
		"parameter max_tokens",
		"invalid parameter max_completion_tokens",
		"unsupported parameter max_tokens",
		"Parameter 'MAX_COMPLETION_TOKENS' is invalid",
		"UNSUPPORTED parameter max_tokens",
		"OpenAI error: parameter max_completion_tokens unsupported",
		"OpenWebUI: parameter 'max_completion_tokens' not supported",
		"LiteLLM error: invalid parameter max_tokens",
		"Invalid max_tokens parameter",
		"Unsupported max_completion_tokens",
	}

	for _, pattern := range shouldMatch {
		if !isMaxTokensParameterError(pattern) {
			t.Errorf("Should detect parameter error: %s", pattern)
		}
	}

	// These should NOT match: missing either indicator or parameter name
	shouldNotMatch := []string{
		"parameter 'max_input_tokens'", // wrong parameter
		"parameter mismatch",           // no token param
		"max_tokens is great",          // no error indicator
		"invalid request",              // no token param
		"unsupported feature",          // no token param
		"",                             // empty
	}

	for _, pattern := range shouldNotMatch {
		if isMaxTokensParameterError(pattern) {
			t.Errorf("Should NOT detect parameter error: %s", pattern)
		}
	}
}

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
			app.Post("/v1/messages", func(c *fiber.Ctx) error { return HandleMessages(c, cfg) })

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
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return HandleMessages(c, cfg) })

	body := `{"model":"claude-x","messages":"` + strings.Repeat("secret prompt ", 200) + `"}`
	old := shared.LogOut
	var buf syncBuf
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })

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

// A rejected request carries its reason in the access line, token masked.
func TestMessagesParseFailureReasonInAccessLog(t *testing.T) {
	var buf bytes.Buffer
	app := fiber.New()
	app.Use(rt.AccessLog(&buf))
	app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error { return HandleMessages(c, &config.Config{}) })

	old := shared.LogOut
	var logBuf syncBuf
	shared.LogOut = &logBuf
	t.Cleanup(func() { shared.LogOut = old })

	req := httptest.NewRequest(http.MethodPost, "/call/secret/v1/messages", strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	got := buf.String()
	for _, want := range []string{" 400 - ", "POST /call/***/v1/messages: Invalid request body: "} {
		if !strings.Contains(got, want) {
			t.Errorf("access log %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("token leaked: %q", got)
	}
}

// In debug mode the raw body is still printed.
func TestMessagesParseFailureLogsBodyInDebug(t *testing.T) {
	app := fiber.New()
	cfg := &config.Config{Debug: true}
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return HandleMessages(c, cfg) })

	body := `{"model":"claude-x","messages":"not an array"}`
	old := shared.LogOut
	var buf syncBuf
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })

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

const authTestToken = "0123456789abcdef0123456789abcdef"

func TestModelRoute(t *testing.T) {
	cfg := &config.Config{
		Profile:      "work",
		DefaultModel: "backend-default",
		ModelRules:   []config.ModelRule{{Match: "*opus*", Model: "backend-opus"}},
	}
	app := fiber.New()
	app.Get(shared.CallPrefix+":token/model", shared.RequireProfile(creds.StaticIndex("work", authTestToken),
		func(string) (*config.Config, error) { return cfg, nil }), ModelRoute)
	for _, tt := range []struct {
		query string
		want  string
	}{
		{"claude-opus-5-5", "backend-opus"},
		{"claude-sonnet-5-5", "backend-default"},
	} {
		resp, err := app.Test(httptest.NewRequest("GET", "/call/"+authTestToken+"/model?requested="+tt.query, nil))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]string
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusOK || len(result) != 3 || result["kind"] != "ccgp:model-route:v1" ||
			result["requestedModel"] != tt.query || result["backendModel"] != tt.want {
			t.Fatalf("unexpected route response: %s", raw)
		}
		if strings.Contains(string(raw), authTestToken) || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("route metadata must not expose auth or allow HTTP caching")
		}
	}
	for _, path := range []string{"/call/wrong/model?requested=opus", "/call/" + authTestToken + "/model"} {
		resp, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == fiber.StatusOK {
			t.Fatalf("unexpected success for %s", path)
		}
	}
}

// routeApp mounts modelRoute behind the real auth middleware.
func routeApp(build func(string) (*config.Config, error)) *fiber.App {
	app := fiber.New()
	app.Get(shared.CallPrefix+":token/model", shared.RequireProfile(creds.StaticIndex("work", authTestToken), build), ModelRoute)
	return app
}

func routeGet(t *testing.T, app *fiber.App, path string) (int, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func TestModelRouteStatuses(t *testing.T) {
	app := routeApp(func(string) (*config.Config, error) { return &config.Config{}, nil })
	ok := "/call/" + authTestToken + "/model?requested="
	for _, tt := range []struct {
		name string
		path string
		want int
	}{
		{"unknown token", "/call/wrong/model?requested=opus", fiber.StatusNotFound},
		{"missing query", "/call/" + authTestToken + "/model", fiber.StatusBadRequest},
		{"empty value", ok, fiber.StatusBadRequest},
		{"too long", ok + strings.Repeat("a", 257), fiber.StatusBadRequest},
		{"max length", ok + strings.Repeat("a", 256), fiber.StatusOK},
		{"newline", ok + "a%0Ab", fiber.StatusBadRequest},
		{"carriage return", ok + "a%0Db", fiber.StatusBadRequest},
		{"nul", ok + "a%00b", fiber.StatusBadRequest},
	} {
		if got, body := routeGet(t, app, tt.path); got != tt.want {
			t.Errorf("%s: status %d, want %d (%s)", tt.name, got, tt.want, body)
		}
	}
}

func TestModelRouteMapping(t *testing.T) {
	app := routeApp(func(string) (*config.Config, error) {
		return &config.Config{OpusModel: "my-opus", ModelRules: []config.ModelRule{{Match: "special-*", Model: "ruled"}}}, nil
	})
	for _, tt := range []struct{ requested, want string }{
		{"Claude-OPUS-5", "my-opus"},   // tier match ignores case
		{"special-1", "ruled"},         // rule wins
		{"gpt-4o-mini", "gpt-4o-mini"}, // non-Claude passes through
		{"gpt%204%2Fx", "gpt 4/x"},     // query is decoded and echoed verbatim
		{"claude-haiku-5-5", converter.DefaultHaikuModel},
	} {
		status, body := routeGet(t, app, "/call/"+authTestToken+"/model?requested="+tt.requested)
		var got map[string]string
		if err := json.Unmarshal([]byte(body), &got); err != nil || status != fiber.StatusOK {
			t.Fatalf("%s: status %d body %q err %v", tt.requested, status, body, err)
		}
		if got["backendModel"] != tt.want {
			t.Errorf("%s: backendModel %q, want %q", tt.requested, got["backendModel"], tt.want)
		}
	}
}

func TestModelRouteFollowsReloadedConfig(t *testing.T) {
	cfg := &config.Config{DefaultModel: "before"}
	app := routeApp(func(string) (*config.Config, error) { return cfg, nil })
	path := "/call/" + authTestToken + "/model?requested=claude-sonnet-5-5"
	if _, body := routeGet(t, app, path); !strings.Contains(body, `"backendModel":"before"`) {
		t.Fatalf("before reload: %s", body)
	}
	cfg = &config.Config{DefaultModel: "after"}
	if _, body := routeGet(t, app, path); !strings.Contains(body, `"backendModel":"after"`) {
		t.Fatalf("after reload: %s", body)
	}
}

func TestModelRouteConfigErrorIs503(t *testing.T) {
	app := routeApp(func(string) (*config.Config, error) { return nil, errors.New("config broken") })
	status, body := routeGet(t, app, "/call/"+authTestToken+"/model?requested=opus")
	if status != fiber.StatusServiceUnavailable || strings.Contains(body, "backendModel") {
		t.Fatalf("status %d body %s", status, body)
	}
}

func TestDoOpenAIRequestDeadlineCoversBodyRead(t *testing.T) {
	requestSeen := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(requestSeen)
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	client := &http.Client{Transport: connpool.NewRouter(0, 1, 1, nil)}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.DoOpenAIRequest(context.Background(), client, req, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("doOpenAIRequest() error before headers = %v", err)
	}
	defer resp.Body.Close()
	<-requestSeen

	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("body read error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("body read did not stop at the request deadline")
	}
	select {
	case <-requestCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request context was not canceled at the deadline")
	}
}

func TestDoOpenAIRequestBodyCloseCancelsContext(t *testing.T) {
	requestSeen := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(requestSeen)
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	client := &http.Client{Transport: connpool.NewRouter(0, 1, 1, nil)}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.DoOpenAIRequest(context.Background(), client, req, 5*time.Second)
	if err != nil {
		t.Fatalf("doOpenAIRequest() error = %v", err)
	}
	defer resp.Body.Close()
	defer func() {
		if !errors.Is(resp.Request.Context().Err(), context.Canceled) {
			t.Errorf("request context error after body close = %v, want context.Canceled", resp.Request.Context().Err())
		}
	}()
	<-requestSeen
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("response body close error = %v", err)
	}
	select {
	case <-requestCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("closing response body did not cancel the upstream request")
	}
}
