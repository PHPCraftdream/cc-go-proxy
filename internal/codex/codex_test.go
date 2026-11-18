package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConvertRequest(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-5",
		"stream": true,
		"system": [
			{"type":"text","text":"x-anthropic-billing-header: abc"},
			{"type":"text","text":"You are Claude Code."}
		],
		"thinking": {"type":"enabled","budget_tokens": 32000},
		"metadata": {"user_id": "user_1_session_2"},
		"tools": [
			{"name":"Read","description":"read a file","input_schema":{"type":"object"}},
			{"type":"web_search_20250305","name":"web_search"}
		],
		"messages": [
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"plan","signature":"ccgp1:ENC"},
				{"type":"thinking","thinking":"foreign","signature":"anthropic-sig"},
				{"type":"text","text":"reading"},
				{"type":"tool_use","id":"call_1","name":"Read","input":{"path":"a.go"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"package a"}]},
				{"type":"text","text":"now explain"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}
			]}
		]
	}`
	req, stream, err := ConvertRequest([]byte(body), "gpt-5", "work")
	if err != nil {
		t.Fatal(err)
	}
	if !stream || !req.Stream || req.Store {
		t.Errorf("stream=%v req.Stream=%v store=%v", stream, req.Stream, req.Store)
	}
	if req.Model != "gpt-5" || req.Reasoning.Effort != "high" || req.PromptCacheKey == "" {
		t.Errorf("model=%q effort=%q cache=%q", req.Model, req.Reasoning.Effort, req.PromptCacheKey)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "Read" {
		t.Errorf("tools = %+v", req.Tools)
	}

	// Round-trip through any: struct field order vs sorted map keys.
	var norm any
	if err := json.Unmarshal([]byte(mustJSON(t, req.Input)), &norm); err != nil {
		t.Fatal(err)
	}
	got := mustJSON(t, norm)
	want := mustJSON(t, []any{
		map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "You are Claude Code."}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "plan"}}, "encrypted_content": "ENC"},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "reading"}}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "Read", "arguments": `{"path":"a.go"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "package a"},
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "now explain"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAA"},
		}},
	})
	if got != want {
		t.Errorf("input =\n%s\nwant\n%s", got, want)
	}
}

func TestConvertRequestToolChoice(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"T","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"T"}}`
	req, stream, err := ConvertRequest([]byte(body), "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		t.Error("stream defaulted to true")
	}
	if mustJSON(t, req.ToolChoice) != `{"name":"T","type":"function"}` {
		t.Errorf("tool_choice = %s", mustJSON(t, req.ToolChoice))
	}
	if req.Reasoning.Effort != "medium" {
		t.Errorf("default effort = %q", req.Reasoning.Effort)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestErrorType(t *testing.T) {
	tests := map[int]string{
		400: "invalid_request_error", 422: "invalid_request_error", 401: "authentication_error",
		403: "permission_error", 404: "not_found_error", 413: "request_too_large",
		429: "rate_limit_error", 500: "api_error", 503: "api_error", 418: "api_error",
	}
	for status, want := range tests {
		if got := ErrorType(status); got != want {
			t.Errorf("ErrorType(%d) = %q, want %q", status, got, want)
		}
	}
}

// A backend that never answers is cut off at the header timeout, not minutes later.
func TestStalledBackendFailsAtHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()
	defer close(release)

	old := HTTPClient
	HTTPClient = newClient(100 * time.Millisecond)
	defer func() { HTTPClient = old }()

	start := time.Now()
	resp, err := post(context.Background(), backend.URL, []byte("{}"), "", nil, &oauth.Tokens{AccessToken: "t"})
	if err == nil {
		resp.Body.Close()
		t.Fatal("stalled backend: no error")
	}
	if !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s", d)
	}
}

func TestDefaultClientHasHeaderTimeout(t *testing.T) {
	tr, ok := HTTPClient.Transport.(*connpool.Router)
	if !ok || tr.Pool().ResponseHeaderTimeout != ResponseHeaderTimeout {
		t.Errorf("transport = %#v", HTTPClient.Transport)
	}
}

func TestSessionHeaders(t *testing.T) {
	h := SessionHeaders(&Request{Model: "gpt-x", PromptCacheKey: "abc123"})
	if h.Get("session-id") != "abc123" || h.Get("x-codex-routing-hint") != "model=gpt-x" {
		t.Errorf("headers = %v", h)
	}
	if h := SessionHeaders(&Request{}); len(h) != 0 {
		t.Errorf("headers of an empty request = %v, want none", h)
	}
}

func TestCallSendsTheSessionHeaders(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("session-id") != "key1" || r.Header.Get("x-codex-routing-hint") != "model=m" {
			t.Errorf("call %d: session headers = %v", n, r.Header)
		}
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("data: {}\n\n"))
	}))
	defer backend.Close()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"new","refresh_token":"rt2"}`))
	}))
	defer issuer.Close()

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "old", RefreshToken: "rt1", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := &oauth.Session{Store: store, Client: &oauth.Client{Issuer: issuer.URL}}
	resp, err := Call(context.Background(), backend.URL, s, &Request{Model: "m", PromptCacheKey: "key1"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2: the retry after 401 must carry the headers too", calls.Load())
	}
}

func TestCallRetriesAfter401(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("ChatGPT-Account-ID") != "acc" || r.Header.Get("originator") != Originator {
			t.Errorf("headers = %v", r.Header)
		}
		if n == 1 {
			if r.Header.Get("Authorization") != "Bearer old" {
				t.Errorf("first auth = %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new" {
			t.Errorf("retry auth = %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte("data: {}\n\n"))
	}))
	defer backend.Close()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"new","refresh_token":"rt2"}`))
	}))
	defer issuer.Close()

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "old", RefreshToken: "rt1", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := &oauth.Session{Store: store, Client: &oauth.Client{Issuer: issuer.URL}}
	resp, err := Call(context.Background(), backend.URL, s, &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 2 {
		t.Errorf("status=%d calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestErrorMessage(t *testing.T) {
	tests := map[string]string{
		`{"error":{"type":"usage_limit_reached","message":"limit hit"}}`: "limit hit",
		`{"error":"bad"}`:   "bad",
		`{"detail":"nope"}`: "nope",
		`plain text`:        "plain text",
	}
	for in, want := range tests {
		if got := ErrorMessage([]byte(in)); got != want {
			t.Errorf("ErrorMessage(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertRequestCorrectsModelIdentity(t *testing.T) {
	body := `{"model":"claude-sonnet-5-5","system":[{"type":"text","text":"You are Claude Code.\nYou are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5. Assistant knowledge cutoff is June 2026.\nMore."}],"messages":[{"role":"user","content":"hi"}]}`
	req, _, err := ConvertRequest([]byte(body), "gpt-6-luna", "p")
	if err != nil {
		t.Fatal(err)
	}
	dev := mustJSON(t, req.Input[0])
	for _, want := range []string{"named gpt-6-luna", "The exact model ID is gpt-6-luna.", "You are Claude Code.", "More."} {
		if !strings.Contains(dev, want) {
			t.Errorf("developer message lacks %q: %s", want, dev)
		}
	}
	for _, bad := range []string{"Sonnet 5.5. The exact", "June 2026"} {
		if strings.Contains(dev, bad) {
			t.Errorf("developer message still has %q: %s", bad, dev)
		}
	}
}

func TestConvertRequestCorrectsIdentityInUserMessages(t *testing.T) {
	ctx := "You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5. Assistant knowledge cutoff is June 2026.\n\n# Language"
	body := `{"model":"claude-sonnet-5-5","system":"s","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"x"}]},` +
		`{"role":"user","content":[{"type":"text","text":` + mustJSON(t, ctx) + `},` +
		`{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":` + mustJSON(t, ctx) + `}]}]}]}`
	req, _, err := ConvertRequest([]byte(body), "gpt-6-luna", "p")
	if err != nil {
		t.Fatal(err)
	}
	all := mustJSON(t, req.Input)
	if !strings.Contains(all, "named gpt-6-luna") {
		t.Errorf("user text block not rewritten: %s", all)
	}
	// Tool results are data, not instructions: the one remaining original is theirs.
	if n := strings.Count(all, "Sonnet 5.5. The exact"); n != 1 {
		t.Errorf("original sentence occurs %d times, want 1 (the tool result): %s", n, all)
	}
}

// Claude Code sends some context as role "system" entries of the messages array.
func TestConvertRequestCorrectsIdentityInSystemRoleMessages(t *testing.T) {
	ctx := "ctx\nYou are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5. Assistant knowledge cutoff is June 2026.\n\n# Language"
	body := `{"model":"claude-sonnet-5-5","system":"s","messages":[` +
		`{"role":"system","content":[{"type":"text","text":` + mustJSON(t, ctx) + `}]},` +
		`{"role":"user","content":"hi"}]}`
	req, _, err := ConvertRequest([]byte(body), "gpt-6-luna", "p")
	if err != nil {
		t.Fatal(err)
	}
	all := mustJSON(t, req.Input)
	if strings.Contains(all, "Sonnet 5.5. The exact") || !strings.Contains(all, "named gpt-6-luna") {
		t.Errorf("system-role message not corrected: %s", all)
	}
}

type countedJSON struct {
	calls *atomic.Int32
}

func (c countedJSON) MarshalJSON() ([]byte, error) {
	c.calls.Add(1)
	return []byte(`"counted"`), nil
}

func TestPrepareRequestSnapshotsBodyAndHeaders(t *testing.T) {
	var marshals atomic.Int32
	req := &Request{
		Model:          "model-before",
		PromptCacheKey: "session-before",
		Input:          []any{countedJSON{calls: &marshals}},
	}
	prepared, err := PrepareRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := marshals.Load(); got != 1 {
		t.Fatalf("MarshalJSON called %d times during preparation, want 1", got)
	}
	if !json.Valid(prepared.body) || !strings.Contains(string(prepared.body), `"counted"`) {
		t.Fatalf("prepared body is not the serialized request: %s", prepared.body)
	}

	req.Model = "model-after"
	req.PromptCacheKey = "session-after"
	var tries atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read request: %v", readErr)
		}
		if string(body) != string(prepared.body) {
			t.Errorf("wire body = %s, want snapshot %s", body, prepared.body)
		}
		if got := r.Header.Get("session-id"); got != "session-before" {
			t.Errorf("session-id = %q, want snapshot", got)
		}
		if got := r.Header.Get("x-codex-routing-hint"); got != "model=model-before" {
			t.Errorf("routing hint = %q, want snapshot", got)
		}
		if tries.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new-token","refresh_token":"rt2"}`))
	}))
	defer issuer.Close()

	session := testSession(t)
	session.Client = &oauth.Client{Issuer: issuer.URL}
	for range 2 {
		resp, err := CallPrepared(context.Background(), backend.URL, session, prepared)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if got := marshals.Load(); got != 1 {
		t.Fatalf("MarshalJSON called %d times after prepared calls, want 1", got)
	}
}

func TestResponseDoesNotRetainUploadBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	resp, err := Call(context.Background(), backend.URL, testSession(t), &Request{
		Model: "m", Input: []any{strings.Repeat("large upload body ", 256<<10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Request == nil {
		t.Fatal("response has no request metadata")
	}
	if resp.Request.Body != http.NoBody {
		t.Errorf("response request body = %T, want http.NoBody", resp.Request.Body)
	}
	if resp.Request.GetBody != nil {
		t.Error("response request retains GetBody")
	}
	if resp.Request.URL == nil || resp.Request.URL.Host != strings.TrimPrefix(backend.URL, "http://") {
		t.Errorf("response request URL metadata was not preserved: %v", resp.Request.URL)
	}
	if got := resp.Request.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("response request headers were not preserved: Content-Type = %q", got)
	}
	if resp.Request.Context() == nil {
		t.Error("response request context was not preserved")
	}
}

func benchmarkPreparedSession(b *testing.B) *oauth.Session {
	b.Helper()
	store := oauth.Store{Path: filepath.Join(b.TempDir(), "oauth.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", LastRefresh: time.Now()}); err != nil {
		b.Fatal(err)
	}
	return &oauth.Session{Store: store, Client: &oauth.Client{}}
}

func benchmarkRetryTransport(b *testing.B) {
	old := HTTPClient
	var calls atomic.Int32
	HTTPClient = &http.Client{Transport: benchmarkRoundTripper(func(r *http.Request) (*http.Response, error) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return nil, err
		}
		n := calls.Add(1) % 3
		status := http.StatusServiceUnavailable
		if n == 0 {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})}
	b.Cleanup(func() { HTTPClient = old })
}

type benchmarkRoundTripper func(*http.Request) (*http.Response, error)

func (f benchmarkRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func benchmarkFiveMiBRequest() *Request {
	body := strings.Repeat("previous conversation includes tools, results, and context. ", (5<<20)/59)
	body = body[:5<<20]
	return &Request{Model: "benchmark-model", Input: []any{map[string]any{"type": "message", "role": "user", "content": body}}}
}

func BenchmarkRequestRetries(b *testing.B) {
	for _, mode := range []string{"BeforeEquivalent_CallEachRetry", "After_PrepareOnce"} {
		b.Run(mode, func(b *testing.B) {
			benchmarkRetryTransport(b)
			session := benchmarkPreparedSession(b)
			req := benchmarkFiveMiBRequest()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var prepared *Prepared
				if mode == "After_PrepareOnce" {
					var err error
					prepared, err = PrepareRequest(req)
					if err != nil {
						b.Fatal(err)
					}
				}
				for range 3 {
					var resp *http.Response
					var err error
					if prepared == nil {
						resp, err = Call(context.Background(), "https://benchmark.invalid", session, req)
					} else {
						resp, err = CallPrepared(context.Background(), "https://benchmark.invalid", session, prepared)
					}
					if err != nil {
						b.Fatal(err)
					}
					_ = resp.Body.Close()
				}
			}
		})
	}
}

func TestNeedsNewerClient(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		required string
		ok       bool
	}{
		{"backend wording", 400, `{"error":{"message":"The 'gpt-6.3-astra' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again.","type":"invalid_request_error"}}`, "", true},
		{"version in the text", 400, `{"detail":"Codex 0.171.0 or newer is required: client version is too old"}`, "0.171.0", true},
		{"version field", 426, `{"error":{"message":"unsupported client version","minimal_client_version":[0,172,3]}}`, "0.172.3", true},
		{"string field", 400, `{"error":{"message":"x","minimum_client_version":"0.173.0"}}`, "0.173.0", true},
		{"plan upsell", 403, `{"error":{"message":"Upgrade to Pro to use this model."}}`, "", false},
		{"usage limit", 429, `{"error":{"type":"usage_limit_reached","message":"Upgrade to the latest plan"}}`, "", false},
		{"unauthorized", 401, `{"error":{"message":"requires a newer version"}}`, "", false},
		{"server error", 500, `{"error":{"message":"requires a newer version"}}`, "", false},
		{"other 400", 400, `{"error":{"message":"Unsupported parameter: 'foo'"}}`, "", false},
	}
	for _, tt := range tests {
		required, ok := NeedsNewerClient(tt.status, []byte(tt.body))
		if required != tt.required || ok != tt.ok {
			t.Errorf("%s: got %q, %v; want %q, %v", tt.name, required, ok, tt.required, tt.ok)
		}
	}
}

// Inference presents the configured version the
// way the official client does (originator/version); without one, as before.
func TestClientVersionInUserAgent(t *testing.T) {
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
	}))
	defer srv.Close()
	old := HTTPClient
	HTTPClient = &http.Client{}
	defer func() { HTTPClient = old }()

	tok := &oauth.Tokens{AccessToken: "t"}
	for _, tt := range []struct{ version, want string }{{"0.170.0", "codex_cli_rs/0.170.0 (ccgp)"}, {"", "codex_cli_rs (ccgp)"}} {
		ctx := context.Background()
		if tt.version != "" {
			ctx = WithClientVersion(ctx, tt.version)
		}
		resp, err := post(ctx, srv.URL, []byte(`{}`), "", nil, tok)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if agent != tt.want {
			t.Errorf("HTTP user agent = %q, want %q", agent, tt.want)
		}
	}
}

type routingRoundTripper func(*http.Request) (*http.Response, error)

func (f routingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func useRoutingTransport(t *testing.T, before func(*http.Request), responseState string) {
	t.Helper()
	var responseCount atomic.Int32
	old := HTTPClient
	HTTPClient = &http.Client{Transport: routingRoundTripper(func(r *http.Request) (*http.Response, error) {
		if before != nil {
			before(r)
		}
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil && responseState != "" && responseCount.Add(1) == 1 {
			delete(resp.Header, http.CanonicalHeaderKey("x-codex-turn-state"))
			resp.Header["x-CoDeX-tUrN-sTaTe"] = []string{responseState}
		}
		return resp, err
	})}
	t.Cleanup(func() { HTTPClient = old })
}

func TestRoutingCapturesMixedCaseHeaderVerbatimAndReplaysOnNextCall(t *testing.T) {
	initial := "  raw: initial \t do not parse, hash, or trim  "
	returned := "  opaque: returned \t preserve, exactly  "
	var requestsMu sync.Mutex
	var requests []struct{ turnState, threadID, requestID, session, hint string }
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()
	useRoutingTransport(t, func(r *http.Request) {
		requestsMu.Lock()
		defer requestsMu.Unlock()
		requests = append(requests, struct{ turnState, threadID, requestID, session, hint string }{
			r.Header.Get("x-codex-turn-state"), r.Header.Get("thread-id"),
			r.Header.Get("x-client-request-id"), r.Header.Get("session-id"),
			r.Header.Get("x-codex-routing-hint"),
		})
	}, returned)

	var callback []string
	var callbackMu sync.Mutex
	routing := NewRouting("thread-17", initial, func(state string) {
		callbackMu.Lock()
		callback = append(callback, state)
		callbackMu.Unlock()
	})
	ctx := WithRouting(context.Background(), routing)
	req := &Request{Model: "gpt-test", PromptCacheKey: "session-9"}
	resp, err := Call(ctx, backend.URL, testSession(t), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// The custom RoundTripper inserts this mixed-case response header after
	// parsing; the next call verifies exact state replay and case-fold capture.
	resp, err = Call(ctx, backend.URL, testSession(t), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	requestsMu.Lock()
	got := append([]struct{ turnState, threadID, requestID, session, hint string }(nil), requests...)
	requestsMu.Unlock()
	if len(got) != 2 {
		t.Fatalf("captured %d requests, want 2", len(got))
	}
	if got[0].turnState != initial || got[1].turnState != returned {
		t.Fatalf("sent turn-state values = %q, %q", got[0].turnState, got[1].turnState)
	}
	for i, request := range got {
		if request.threadID != "thread-17" || request.requestID != "thread-17" || request.session != "session-9" || request.hint != "model=gpt-test" {
			t.Errorf("request %d routing/session headers = %+v", i, request)
		}
	}
	if got[0].requestID != "thread-17" || got[1].requestID != "thread-17" {
		t.Errorf("request IDs = %q, %q, want thread ID on both", got[0].requestID, got[1].requestID)
	}
	if !routing.Sent() || !routing.Returned() {
		t.Errorf("Sent()=%v Returned()=%v, want both true", routing.Sent(), routing.Returned())
	}
	callbackMu.Lock()
	gotCallback := append([]string(nil), callback...)
	callbackMu.Unlock()
	if len(gotCallback) != 1 || gotCallback[0] != returned {
		t.Errorf("state callbacks = %q, want [%q]", gotCallback, returned)
	}
}

func TestRoutingReplaysReturnedStateOnCompressionFallback(t *testing.T) {
	resetCompression(t)
	var got []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("x-codex-turn-state"))
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Content-Encoding") != "" {
			w.Header().Set("X-Codex-Turn-State", "after-compression-refusal;v=1")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()

	routing := NewRouting("thread-compress", "before-compression", nil)
	resp, err := Call(WithRouting(context.Background(), routing), backend.URL, testSession(t), bigRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(got) != 2 || got[0] != "before-compression" || got[1] != "after-compression-refusal;v=1" {
		t.Fatalf("turn-state values across compressed/plain POSTs = %q", got)
	}
	if !routing.Sent() || !routing.Returned() {
		t.Errorf("Sent()=%v Returned()=%v, want both true", routing.Sent(), routing.Returned())
	}
}

func TestRoutingReplaysReturnedStateOnOAuthRefresh(t *testing.T) {
	var got []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("x-codex-turn-state"))
		if len(got) == 1 {
			w.Header().Set("X-Codex-Turn-State", "after-401;opaque")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new-token","refresh_token":"rt2"}`))
	}))
	defer issuer.Close()

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "oauth.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "old-token", RefreshToken: "rt1", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	session := &oauth.Session{Store: store, Client: &oauth.Client{Issuer: issuer.URL}}
	routing := NewRouting("thread-oauth", "before-401", nil)
	resp, err := Call(WithRouting(context.Background(), routing), backend.URL, session, &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(got) != 2 || got[0] != "before-401" || got[1] != "after-401;opaque" {
		t.Fatalf("turn-state values across OAuth POSTs = %q", got)
	}
	if !routing.Sent() || !routing.Returned() {
		t.Errorf("Sent()=%v Returned()=%v, want both true", routing.Sent(), routing.Returned())
	}
}

func TestRoutingPreservesStateWhenResponseHeaderIsMissing(t *testing.T) {
	var got []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("x-codex-turn-state"))
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()
	useRoutingTransport(t, nil, "returned-on-first-post")

	routing := NewRouting("thread-preserve", "initial-state", nil)
	ctx := WithRouting(context.Background(), routing)
	for range 3 {
		resp, err := Call(ctx, backend.URL, testSession(t), &Request{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if len(got) != 3 || got[0] != "initial-state" || got[1] != "returned-on-first-post" || got[2] != "returned-on-first-post" {
		t.Fatalf("turn-state values after a response without the header = %q", got)
	}
}

func TestRoutingSentRemainsFalseWithoutOpaqueState(t *testing.T) {
	var threadID, requestID, turnState string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()
	useRoutingTransport(t, func(r *http.Request) {
		threadID = r.Header.Get("thread-id")
		requestID = r.Header.Get("x-client-request-id")
		turnState = r.Header.Get("x-codex-turn-state")
	}, "")

	routing := NewRouting("thread-empty", "", nil)
	resp, err := Call(WithRouting(context.Background(), routing), backend.URL, testSession(t), &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if threadID != "thread-empty" || requestID != "thread-empty" || turnState != "" {
		t.Errorf("routing headers: thread-id=%q x-client-request-id=%q turn-state=%q", threadID, requestID, turnState)
	}
	if routing.Sent() || routing.Returned() {
		t.Errorf("Sent()=%v Returned()=%v, want both false", routing.Sent(), routing.Returned())
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
