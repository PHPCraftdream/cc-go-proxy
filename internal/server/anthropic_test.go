package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

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
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return handleAnthropicCompat(c, cfg) })
	app.Post("/v1/messages/count_tokens", func(c *fiber.Ctx) error { return handleAnthropicCountTokens(c, cfg) })
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

// Codex and Chat Completions have no counting endpoint: the answer scales with
// the request instead of being a constant.
func TestCountTokensEstimatesFromTheRequest(t *testing.T) {
	app := fiber.New()
	app.Post("/v1/messages/count_tokens", func(c *fiber.Ctx) error { return handleCountTokens(c, &config.Config{}) })

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
