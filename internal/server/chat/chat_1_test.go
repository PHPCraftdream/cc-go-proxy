package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
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

func compatConfig(upstreamURL string, efforts map[string]string) *config.Config {
	return &config.Config{
		Profile: "zai", ProviderName: "zai", ProviderType: config.ProviderTypeAnthropicCompat,
		OpenAIAPIKey: "zai-key",
		Providers: map[string]*config.Provider{
			"zai": {Name: "zai", Type: config.ProviderTypeAnthropicCompat, BaseURL: upstreamURL, EffortMap: efforts},
		},
		ModelRules: []config.ModelRule{
			{Match: "*fable*", Model: "glm-5.3", ContextWindow: 1000000},
			{Match: "*", Model: "glm-5.3-flash", ContextWindow: 1000000},
		},
	}
}

func compatApp(cfg *config.Config) *fiber.App {
	app := fiber.New()
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return HandleAnthropicCompat(c, cfg) })
	app.Post("/v1/messages/count_tokens", func(c *fiber.Ctx) error { return HandleAnthropicCountTokens(c, cfg) })
	return app
}

const sampleRequest = `{
	"model": "claude-sonnet-5-5",
	"max_tokens": 128000,
	"stream": true,
	"temperature": 0.25,
	"system": [{"type":"text","text":"You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5. <b>keep</b>"}],
	"messages": [
		{"role":"system","content":[{"type":"text","text":"ctx You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5."}]},
		{"role":"user","content":"hello & goodbye"}
	],
	"thinking": {"type":"adaptive","display":"omitted"},
	"output_config": {"effort": "xhigh"},
	"tools": [{"name":"Read","input_schema":{"type":"object"}}]
}`

var zaiEfforts = map[string]string{"low": "low", "medium": "high", "high": "max", "xhigh": "max", "max": "max"}

func TestRewriteAnthropicBody(t *testing.T) {
	out, requested, backend, err := rewriteAnthropicBody([]byte(sampleRequest), compatConfig("http://x", nil), zaiEfforts)
	if err != nil {
		t.Fatal(err)
	}
	if requested != "claude-sonnet-5-5" || backend != "glm-5.3-flash" {
		t.Errorf("requested=%q backend=%q", requested, backend)
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if m["model"] != "glm-5.3-flash" {
		t.Errorf("model = %v", m["model"])
	}
	if eff := m["output_config"].(map[string]any)["effort"]; eff != "max" {
		t.Errorf("effort = %v, want max (xhigh -> max)", eff)
	}
	// Untouched fields keep their value and number type.
	if m["max_tokens"].(json.Number).String() != "128000" || m["temperature"].(json.Number).String() != "0.25" || m["stream"] != true {
		t.Errorf("scalar fields changed: %v", m)
	}
	if m["thinking"].(map[string]any)["type"] != "adaptive" || len(m["tools"].([]any)) != 1 {
		t.Errorf("thinking/tools changed: %v %v", m["thinking"], m["tools"])
	}
	// Identity corrected in system and in the system-role message; the user text
	// and HTML-ish characters are not mangled.
	s := string(out)
	if strings.Contains(s, "Sonnet 5.5. The exact") || strings.Count(s, "named glm-5.3-flash") != 2 {
		t.Errorf("identity not corrected: %s", s)
	}
	if !strings.Contains(s, "<b>keep</b>") || !strings.Contains(s, "hello & goodbye") {
		t.Errorf("text was escaped or lost: %s", s)
	}

	// Fable goes to the full model.
	fable := strings.Replace(sampleRequest, "claude-sonnet-5-5\",\n\t\"max_tokens", "claude-fable-5-1\",\n\t\"max_tokens", 1)
	if _, _, b, err := rewriteAnthropicBody([]byte(fable), compatConfig("http://x", nil), nil); err != nil || b != "glm-5.3" {
		t.Errorf("fable -> %q, %v", b, err)
	}
}

func TestMapEffort(t *testing.T) {
	for in, want := range map[string]string{"low": "low", "medium": "high", "high": "max", "xhigh": "max", "max": "max", "MEDIUM": "high", "weird": "weird"} {
		m := map[string]any{"output_config": map[string]any{"effort": in}}
		mapEffort(m, zaiEfforts)
		if got := m["output_config"].(map[string]any)["effort"]; got != want {
			t.Errorf("effort %q -> %v, want %q", in, got, want)
		}
	}
	// Without a map nothing changes; odd shapes do not panic.
	m := map[string]any{"output_config": map[string]any{"effort": "medium"}}
	mapEffort(m, nil)
	if m["output_config"].(map[string]any)["effort"] != "medium" {
		t.Error("effort changed without a map")
	}
	mapEffort(map[string]any{}, zaiEfforts)
	mapEffort(map[string]any{"output_config": map[string]any{"effort": 3}}, zaiEfforts)
	mapEffort(map[string]any{"output_config": "x"}, zaiEfforts)
}

func TestEffortMapFor(t *testing.T) {
	own := &config.Provider{BaseURL: "https://api.z.ai/api/anthropic", EffortMap: map[string]string{"medium": "low"}}
	if got := effortMapFor(own); got["medium"] != "low" {
		t.Errorf("own map ignored: %v", got)
	}
	// z.ai without its own map: the preset's.
	if got := effortMapFor(&config.Provider{BaseURL: "https://api.z.ai/api/anthropic"}); got["xhigh"] != "max" || got["medium"] != "high" {
		t.Errorf("preset map = %v", got)
	}
	if got := effortMapFor(&config.Provider{BaseURL: "https://example.com"}); got != nil {
		t.Errorf("unknown provider has a map: %v", got)
	}
}

func TestForwardAnthropicStreamsAndAuthenticates(t *testing.T) {
	old := shared.LogOut
	var buf syncBuf
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })

	var mu sync.Mutex
	var gotAuth, gotVersion, gotBeta, gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth, gotVersion, gotBeta, gotPath = r.Header.Get("Authorization"), r.Header.Get("anthropic-version"), r.Header.Get("anthropic-beta"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req-1")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	app := compatApp(compatConfig(upstream.URL+"/api/anthropic/", map[string]string{"xhigh": "max"}))
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(sampleRequest))
	req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(buf.String(), "stream failed") {
		t.Errorf("a clean stream was logged: %q", buf.String())
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.Header.Get("Request-Id") != "req-1" {
		t.Errorf("status=%d headers=%v", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(string(body), "message_start") || !strings.Contains(string(body), "message_stop") {
		t.Errorf("stream not passed through: %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/api/anthropic/v1/messages" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer zai-key" || gotVersion != "2023-06-01" || gotBeta != "interleaved-thinking-2025-05-14" {
		t.Errorf("headers: auth=%q version=%q beta=%q", gotAuth, gotVersion, gotBeta)
	}
	if gotBody["model"] != "glm-5.3-flash" || gotBody["output_config"].(map[string]any)["effort"] != "max" {
		t.Errorf("upstream body = %v", gotBody)
	}
}

func TestForwardAnthropicPassesErrorsThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer upstream.Close()

	resp, err := compatApp(compatConfig(upstream.URL, nil)).Test(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(sampleRequest)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" || !strings.Contains(string(body), "slow down") {
		t.Errorf("status=%d retry=%q body=%s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

func TestForwardAnthropicBadRequestAndUnreachableUpstream(t *testing.T) {
	app := compatApp(compatConfig("http://127.0.0.1:1", nil))
	resp, err := app.Test(httptest.NewRequest("POST", "/v1/messages", strings.NewReader("not json")))
	if err != nil || resp.StatusCode != 400 {
		t.Errorf("bad body: %v, %v", resp, err)
	}
	resp, err = app.Test(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(sampleRequest)))
	if err != nil || resp.StatusCode != 502 {
		t.Errorf("unreachable upstream: %v, %v", resp, err)
	}
}

// zaiLines sends n requests through handleAnthropicCompat with the access log
// on, over a client of one lane, and returns the log lines.
func zaiLines(t *testing.T, upstreamURL string, n int) []string {
	t.Helper()
	old := UpstreamClient
	UpstreamClient = &http.Client{Transport: connpool.NewRouter(0, 1, 1, nil)}
	t.Cleanup(func() { UpstreamClient = old })

	var al syncBuf
	cfg := compatConfig(upstreamURL, nil)
	app := fiber.New()
	app.Use(rt.AccessLog(&al))
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return HandleAnthropicCompat(c, cfg) })
	for range n {
		req := httptest.NewRequest(fiber.MethodPost, "/v1/messages", strings.NewReader(sampleRequest))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		time.Sleep(100 * time.Millisecond) // the transport takes the connection back after the body ended
	}
	return strings.Split(strings.TrimSpace(al.String()), "\n")
}

// The line of a request that opened its own connection says so; one that went
// over an open connection stays quiet. Counting the first kind shows whether the
// connections to the provider are kept.
func TestZaiLineMarksAConnectionOpenedForTheRequest(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	defer up.Close()

	lines := zaiLines(t, up.URL, 2)
	if len(lines) != 2 {
		t.Fatalf("log = %q, want 2 lines", lines)
	}
	if !strings.Contains(lines[0], " glm-5.3-flash | conn new (") {
		t.Errorf("first request opened the connection: %q", lines[0])
	}
	if strings.Contains(lines[1], " | conn") {
		t.Errorf("second request reused it: %q", lines[1])
	}
}

// A refusal or an unreachable upstream says which connection it was on.
func TestZaiFailureLinesNameTheConnection(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer refusing.Close()
	if lines := zaiLines(t, refusing.URL, 1); !strings.Contains(lines[0], " 429 - ") || !strings.Contains(lines[0], " | conn new (dns ") {
		t.Errorf("429 line lacks the connection: %q", lines)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	if lines := zaiLines(t, url, 1); !strings.Contains(lines[0], " 502 - ") || !strings.Contains(lines[0], " | conn new (dns ") {
		t.Errorf("502 line lacks the connection: %q", lines)
	}
}

func TestAnthropicCountTokensFallsBackToEstimate(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler()) // no count_tokens endpoint
	defer upstream.Close()

	resp, err := compatApp(compatConfig(upstream.URL, nil)).Test(httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(sampleRequest)))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]int
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil || resp.StatusCode != 200 || out["input_tokens"] < 10 {
		t.Errorf("status=%d body=%s err=%v", resp.StatusCode, b, err)
	}
}

func TestCopySSE(t *testing.T) {
	var mu sync.Mutex
	var errs []error
	onErr := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	discard := bufio.NewWriter(io.Discard)

	// A normal end stays silent.
	errs = nil
	copySSE(discard, strings.NewReader("event: x\ndata: {}\n\n"), onErr)
	if len(errs) != 0 {
		t.Errorf("a clean end logged %v", errs)
	}

	// A stream the provider breaks is reported.
	errs = nil
	copySSE(discard, io.MultiReader(strings.NewReader("data: one\n\n"), iotest.ErrReader(errors.New("boom"))), onErr)
	if len(errs) != 1 || errs[0].Error() != "boom" {
		t.Errorf("broken stream logged %v, want boom", errs)
	}

	// A client that left (the write fails) stays silent, even though a read
	// error follows.
	errs = nil
	w := &failSecondWrite{}
	copySSE(bufio.NewWriterSize(w, 1), io.MultiReader(strings.NewReader("data: one\n\n"), strings.NewReader("data: two\n\n"), iotest.ErrReader(errors.New("boom"))), onErr)
	if len(errs) != 0 {
		t.Errorf("a client that left logged %v", errs)
	}
}

type failSecondWrite struct{ n int }

func (w *failSecondWrite) Write(p []byte) (int, error) {
	w.n++
	if w.n > 1 {
		return 0, errors.New("client gone")
	}
	return len(p), nil
}

// A stream the provider breaks mid-reply is logged; the access line already
// said 200, so this is the only trace.
func TestForwardAnthropicLogsAStreamTheProviderBroke(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // cut the chunked stream: the proxy reads an unexpected EOF
	}))
	defer upstream.Close()

	old := shared.LogOut
	var buf syncBuf
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(sampleRequest))
	req.Header.Set("Content-Type", "application/json")
	resp, err := compatApp(compatConfig(upstream.URL, nil)).Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	log := buf.String()
	for _, want := range []string{"stream failed after", "[zai]", "claude-sonnet-5-5 -> glm-5.3-flash"} {
		if !strings.Contains(log, want) {
			t.Errorf("log = %q, want it to name the broken stream (%q)", log, want)
		}
	}
}

// Codex and Chat Completions have no counting endpoint: the answer scales with
// the request instead of being a constant.
func TestCountTokensEstimatesFromTheRequest(t *testing.T) {
	app := fiber.New()
	app.Post("/v1/messages/count_tokens", func(c *fiber.Ctx) error { return HandleCountTokens(c, &config.Config{}) })

	count := func(body string) int {
		t.Helper()
		resp, err := app.Test(httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body)))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]int
		b, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(b, &out); err != nil || resp.StatusCode != 200 {
			t.Fatalf("status=%d body=%s err=%v", resp.StatusCode, b, err)
		}
		return out["input_tokens"]
	}

	short := `{"messages":[{"role":"user","content":"hi"}]}`
	long := `{"messages":[{"role":"user","content":"` + strings.Repeat("word ", 4000) + `"}]}`
	if got := count(short); got != len(short)/4 {
		t.Errorf("short = %d, want %d", got, len(short)/4)
	}
	if got := count(long); got != len(long)/4 || got <= 100 {
		t.Errorf("long = %d, want %d", got, len(long)/4)
	}
}

func TestStreamOpenAIResponseStopsAfterContentWriterFailure(t *testing.T) {
	const firstText = "FIRST_CONTENT_DELTA_SENTINEL"
	writeErr := errors.New("client write failed")

	for _, tc := range []struct {
		name       string
		bufferSize int
		firstText  string
	}{
		{
			name:       "small buffered flush",
			bufferSize: 4096,
			firstText:  firstText,
		},
		{
			name:       "large content direct write",
			bufferSize: 1,
			firstText:  firstText + strings.Repeat("x", 8192),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &disconnectChunkReader{chunks: [][]byte{
				[]byte(fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tc.firstText)),
				[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"second delta\"}}]}\n\n"),
				[]byte("data: [DONE]\n\n"),
			}}
			body := &disconnectTrackingBody{Reader: reader}
			resp := &http.Response{Body: body}
			ctx, cancel := context.WithCancel(context.Background())
			writer := &sentinelArmedWriter{sentinel: firstText, failErr: writeErr}

			err := streamOpenAIResponse(bufio.NewWriterSize(writer, tc.bufferSize), resp, "test-model", &config.Config{}, time.Now(), cancel)
			if !errors.Is(err, writeErr) {
				t.Fatalf("streamOpenAIResponse() error = %v, want %v", err, writeErr)
			}
			if !body.closed {
				t.Error("response body was not closed after writer failure")
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("context error = %v, want context.Canceled", ctx.Err())
			}
			if reader.reads != 2 {
				t.Errorf("upstream Read calls = %d, want exactly 2 (no read after writer failure)", reader.reads)
			}
			if got := writer.output.String(); !strings.Contains(got, firstText) {
				t.Errorf("successfully delivered output does not contain first content sentinel %q", firstText)
			}
			if writer.failedWrites != 1 {
				t.Errorf("failed writer calls = %d, want 1", writer.failedWrites)
			}
			if tc.name == "large content direct write" && len(tc.firstText) <= tc.bufferSize {
				t.Fatalf("large content length = %d, want it to exceed bufio buffer size %d", len(tc.firstText), tc.bufferSize)
			}
		})
	}
}

type disconnectChunkReader struct {
	chunks [][]byte
	reads  int
}

func (r *disconnectChunkReader) Read(p []byte) (int, error) {
	r.reads++
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	if len(p) < len(chunk) {
		return 0, fmt.Errorf("test reader buffer (%d) is smaller than SSE chunk (%d)", len(p), len(chunk))
	}
	r.chunks = r.chunks[1:]
	return copy(p, chunk), nil
}
