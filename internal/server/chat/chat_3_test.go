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

// syncBuf is a log sink the stream-writer goroutine and the test share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

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

func TestOpenAIUpstreamErrorsPreserveStatusAndSafeHeaders(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, tc := range []struct {
			status int
			typ    string
		}{
			{http.StatusTooManyRequests, "rate_limit_error"},
			{http.StatusServiceUnavailable, "api_error"},
			{http.StatusBadRequest, "invalid_request_error"},
		} {
			t.Run(strings.Join([]string{http.StatusText(tc.status), map[bool]string{true: "stream", false: "nonstream"}[streaming]}, "/"), func(t *testing.T) {
				const upstreamMessage = "upstream rejected the request"
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "60")
					w.Header().Set("X-RateLimit-Remaining-Requests", "0")
					w.Header().Set("X-Private-Upstream", "do-not-forward")
					w.Header().Set("Authorization", "Bearer upstream-secret")
					w.Header().Set("Content-Type", "text/plain")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":{"message":"`+upstreamMessage+`","type":"upstream_error"}}`)
				}))
				defer upstream.Close()

				oldTransport := rt.OpenAITransport
				rt.OpenAITransport = connpool.NewRouter(0, 1, 1, nil)
				t.Cleanup(func() { rt.OpenAITransport = oldTransport })
				cfg := &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "openai-secret", AnthropicAPIKey: "anthropic-secret"}
				app := fiber.New()
				app.Post("/v1/messages", func(c *fiber.Ctx) error { return HandleMessages(c, cfg) })
				requestBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":` + map[bool]string{true: "true", false: "false"}[streaming] + `}`
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-api-key", "anthropic-secret")
				resp, err := app.Test(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != tc.status {
					t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tc.status, body)
				}
				var payload struct {
					Type  string `json:"type"`
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("response is not JSON: %v: %s", err, body)
				}
				if payload.Type != "error" || payload.Error.Type != tc.typ || payload.Error.Message != upstreamMessage {
					t.Errorf("error payload = %#v, want type=%q message=%q", payload, tc.typ, upstreamMessage)
				}
				if resp.Header.Get("Retry-After") != "60" || resp.Header.Get("X-RateLimit-Remaining-Requests") != "0" {
					t.Errorf("rate headers = Retry-After %q, remaining %q", resp.Header.Get("Retry-After"), resp.Header.Get("X-RateLimit-Remaining-Requests"))
				}
				for _, key := range []string{"X-Private-Upstream", "Authorization", "Set-Cookie"} {
					if got := resp.Header.Get(key); got != "" {
						t.Errorf("sensitive/arbitrary header %s leaked: %q", key, got)
					}
				}
				if got := resp.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("response content type = %q, want application/json", got)
				}
			})
		}
	}
}

func TestOpenAIUpstreamErrorRedactsConfiguredSecretsAndCallToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad openai-secret anthropic-secret /call/secret-token/messages","type":"invalid_request_error"}}`)
	}))
	defer upstream.Close()
	oldTransport := rt.OpenAITransport
	rt.OpenAITransport = connpool.NewRouter(0, 1, 1, nil)
	t.Cleanup(func() { rt.OpenAITransport = oldTransport })
	cfg := &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "openai-secret", AnthropicAPIKey: "anthropic-secret"}
	app := fiber.New()
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return HandleMessages(c, cfg) })
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "anthropic-secret")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"openai-secret", "anthropic-secret", "secret-token"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("response leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(string(body), "/call/***/messages") {
		t.Errorf("call token was not masked: %s", body)
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

func TestUpstreamErrorParts(t *testing.T) {
	tests := []struct {
		name, body, label, msg string
	}{
		{"anthropic envelope", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "rate_limit_error", "slow down"},
		{"z.ai: string code in error", `{"error":{"code":"1302","message":"Rate limit reached for requests"}}`, "1302", "Rate limit reached for requests"},
		{"type and code together", `{"error":{"type":"overloaded_error","code":529,"message":"busy"}}`, "overloaded_error 529", "busy"},
		{"numeric code, msg at the top", `{"code":1113,"msg":"insufficient balance","success":false}`, "1113", "insufficient balance"},
		{"message at the top", `{"message":"nope"}`, "", "nope"},
		{"error as a string", `{"error":"forbidden here"}`, "", "forbidden here"},
		{"detail", `{"detail":"model not found"}`, "", "model not found"},
		{"envelope only", `{"type":"error"}`, "", `{"type":"error"}`},
		{"html page", `<html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>`, "", "html error page: Attention Required! | Cloudflare"},
		{"plain text", "upstream connect error", "", "upstream connect error"},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label, msg := upstreamErrorParts([]byte(tt.body))
			if label != tt.label || msg != tt.msg {
				t.Errorf("label = %q, message = %q; want %q, %q", label, msg, tt.label, tt.msg)
			}
		})
	}
}

func TestUpstreamErrorText(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	h.Set("X-Ratelimit-Remaining-Requests", "0")
	h.Set("X-Request-Id", "req-1")
	body := []byte(`{"error":{"code":"1302","message":"Rate limit reached"}}`)

	got := upstreamErrorText(429, h, body)
	for _, want := range []string{"1302: Rate limit reached", "retry-after=30", "x-ratelimit-remaining-requests=0", "x-request-id=req-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("429 text lacks %q: %q", want, got)
		}
	}
	// The limit headers say something about a limit only; a bad request gets the reason and the id.
	got = upstreamErrorText(400, h, []byte(`{"error":{"type":"invalid_request_error","message":"bad field"}}`))
	if !strings.Contains(got, "invalid_request_error: bad field") || !strings.Contains(got, "x-request-id=req-1") {
		t.Errorf("400 text = %q", got)
	}
	if strings.Contains(got, "retry-after") || strings.Contains(got, "ratelimit") {
		t.Errorf("a 400 carries limit headers: %q", got)
	}
	if got = upstreamErrorText(503, h, []byte(`{"error":{"message":"overloaded"}}`)); !strings.Contains(got, "retry-after=30") {
		t.Errorf("503 text lacks Retry-After: %q", got)
	}
	// Nothing to say is nothing.
	if got = upstreamErrorText(500, http.Header{}, nil); got != "" {
		t.Errorf("empty reply gave %q", got)
	}
}

// P0: whatever the endpoint echoes of the credential, the log does not keep it.
func TestUpstreamErrorTextHidesKeys(t *testing.T) {
	for _, msg := range []string{
		"Incorrect API key provided: sk-proj-abcdef1234567890XYZ",
		"invalid Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
		"bad key 0123456789abcdef0123456789abcdef.AbCdEfGh12345678 for this account",
		`request failed: {"api_key": "supersecretvalue99"}`,
		"token=abcdef123456 rejected",
	} {
		body := []byte(`{"error":{"message":` + jsonString(msg) + `}}`)
		got := upstreamErrorText(401, http.Header{}, body)
		for _, secret := range []string{"sk-proj-abcdef1234567890XYZ", "eyJhbGciOiJIUzI1NiJ9", "0123456789abcdef0123456789abcdef", "AbCdEfGh12345678", "supersecretvalue99", "abcdef123456"} {
			if strings.Contains(got, secret) {
				t.Errorf("%q leaked %q: %q", msg, secret, got)
			}
		}
		if !strings.Contains(got, "***") {
			t.Errorf("nothing was hidden in %q: %q", msg, got)
		}
	}
	// Ordinary words are left as they are.
	plain := "model glm-5.3-flash is not available, token limit reached, try again"
	if got := redactSecrets(plain); got != plain {
		t.Errorf("redactSecrets changed %q to %q", plain, got)
	}
}

func jsonString(s string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// P0: the log line is one line of a bounded size, however the endpoint answers.
func TestUpstreamErrorTextIsOneBoundedLine(t *testing.T) {
	long := strings.Repeat("a long and rambling reason\n\t", 200)
	got := upstreamErrorText(500, http.Header{}, []byte(`{"error":{"message":`+jsonString(long)+`}}`))
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("not one line: %q", got)
	}
	if len(got) > maxUpstreamMsg+len("…")+16 {
		t.Errorf("%d bytes: %q", len(got), got)
	}

	html := "<html><head><title>Blocked</title></head><body>" + strings.Repeat("<p>markup</p>\n", 500) + "</body></html>"
	got = upstreamErrorText(403, http.Header{}, []byte(html))
	if strings.Contains(got, "<p>") || strings.Contains(got, "\n") || !strings.Contains(got, "Blocked") {
		t.Errorf("an HTML page is shown as markup: %q", got)
	}
}

// P0: the reason reaches the request's line for a refusal, and the client still gets the reply as it came.
func TestZaiRefusalLineGivesTheReason(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "42")
		w.Header().Set("X-Request-Id", "req-zai-1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"1302","message":"Rate limit reached for requests, key sk-live-abcdef123456"}}`)
	}))
	defer refusing.Close()

	lines := zaiLines(t, refusing.URL, 1)
	line := lines[0]
	for _, want := range []string{" 429 - ", "claude-sonnet-5-5 -> glm-5.3-flash: 1302: Rate limit reached for requests", "retry-after=42", "x-request-id=req-zai-1", " | conn new (dns "} {
		if !strings.Contains(line, want) {
			t.Errorf("the line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "sk-live-abcdef123456") {
		t.Errorf("a key is in the log:\n%s", line)
	}

	// The client's reply is untouched.
	resp, err := compatApp(compatConfig(refusing.URL, nil)).Test(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(sampleRequest)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "42" || !strings.Contains(string(body), "sk-live-abcdef123456") {
		t.Errorf("the client's reply changed: %d %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

// A successful request has no reason on its line, and a refused count_tokens (answered with an
// estimate, status 200) neither.
func TestZaiLinesWithoutARefusalHaveNoReason(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	defer ok.Close()
	if line := zaiLines(t, ok.URL, 1)[0]; strings.Contains(line, "glm-5.3-flash:") {
		t.Errorf("a reason on a good line: %q", line)
	}
}
