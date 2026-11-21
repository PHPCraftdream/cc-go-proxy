package chat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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
