package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/gofiber/fiber/v2"
)

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

// codexCall runs one streaming /v1/messages request through handleCodexMessages
// against backend and returns the status, body, access log and stream log.
func codexCall(t *testing.T, backend http.Handler, headerTimeout time.Duration) (status int, body, access, stream string) {
	t.Helper()
	srv := httptest.NewServer(backend)
	defer srv.Close()

	oldClient, oldLog := codex.HTTPClient, logOut
	t.Cleanup(func() { codex.HTTPClient, logOut = oldClient, oldLog })
	codex.HTTPClient = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: headerTimeout}}
	var sl syncBuf
	logOut = &sl

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sess := &oauth.Session{Store: store, Client: &oauth.Client{}}
	cfg := &config.Config{
		Profile:      "codex",
		ProviderName: "codex",
		Providers:    map[string]*config.Provider{"codex": {Name: "codex", Type: "codex", BaseURL: srv.URL}},
	}

	var al syncBuf
	app := fiber.New()
	app.Use(accessLog(&al))
	app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error { return handleCodexMessages(c, cfg, sess) })
	req := httptest.NewRequest(fiber.MethodPost, "/call/secret/v1/messages", strings.NewReader(
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), al.String(), sl.String()
}

func TestStalledBackendIs502WithReasonInLog(t *testing.T) {
	status, _, access, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the client hanging up; without
		// it the handler, and so httptest.Server.Close, would block forever.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}), 100*time.Millisecond)
	if status != fiber.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if !strings.Contains(access, " 502 - ") || !strings.Contains(access, "timeout awaiting response headers") {
		t.Errorf("access log = %q", access)
	}
}

func TestUpstreamErrorMessageIsLogged(t *testing.T) {
	status, _, access, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"usage limit reached"}}`))
	}), time.Minute)
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if !strings.Contains(access, " 429 - ") || !strings.Contains(access, ": codex: usage limit reached") {
		t.Errorf("access log = %q", access)
	}
}

func TestBrokenStreamIsLogged(t *testing.T) {
	status, body, _, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"))
	}), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "event: error") {
		t.Fatalf("status %d, body %q: want 200 with an error event", status, body)
	}
	if !strings.Contains(stream, "stream failed after") || !strings.Contains(stream, "ended before completion") ||
		!strings.Contains(stream, "[codex]") {
		t.Errorf("stream log = %q", stream)
	}
}

// The header timeout must not cut a reply that has started: streams run for minutes.
func TestSlowStreamPastHeaderTimeoutSurvives(t *testing.T) {
	status, body, access, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond) // well past the 100 ms header timeout
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"))
	}), 100*time.Millisecond)
	if status != fiber.StatusOK || strings.Contains(body, "event: error") || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q: want a complete stream", status, body)
	}
	if stream != "" || strings.Contains(access, ": ") {
		t.Errorf("unexpected failure log: access %q, stream %q", access, stream)
	}
}

func TestLogStreamFailureLine(t *testing.T) {
	var b bytes.Buffer
	logStreamFailure(&b, time.Now().Add(-2*time.Second), "codex", "claude-opus-5-5 -> gpt-6.1-sol", "stream read error:\n unexpected EOF", "", "")
	got := b.String()
	for _, want := range []string{"stream failed after 2", "[codex] claude-opus-5-5 -> gpt-6.1-sol: stream read error: unexpected EOF\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q lacks %q", got, want)
		}
	}
}

// An error event from the backend is logged raw, with the request ids.
func TestUpstreamErrorEventIsLoggedRaw(t *testing.T) {
	ev := `{"type":"error","error":{"type":"server_error","code":"overloaded","message":"try again"},"sequence_number":3}`
	_, body, _, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req_123")
		w.Header().Set("Cf-Ray", "ray-ams")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: " + ev + "\n\n"))
	}), time.Minute)
	if !strings.Contains(body, "codex: overloaded: try again") {
		t.Errorf("client error lacks the upstream message: %q", body)
	}
	for _, want := range []string{"overloaded: try again | upstream event: " + ev, "| x-request-id=req_123 cf-ray=ray-ams | conn new", "| request "} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream log %q lacks %q", stream, want)
		}
	}
}

func overloadBackend(calls *atomic.Int32, overloadedCalls int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		created := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"
		if calls.Add(1) <= overloadedCalls {
			_, _ = w.Write([]byte(created + "data: {\"type\":\"error\",\"error\":{\"type\":\"service_unavailable_error\",\"code\":\"server_is_overloaded\",\"message\":\"overloaded\"}}\n\n"))
			return
		}
		_, _ = w.Write([]byte(created +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"hi\"}\n\n" +
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n"))
	})
}

func fastRetries(t *testing.T, n int) {
	old := overloadBackoff
	overloadBackoff = make([]time.Duration, n)
	for i := range overloadBackoff {
		overloadBackoff[i] = time.Millisecond
	}
	t.Cleanup(func() { overloadBackoff = old })
}

func TestOverloadedBackendIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, _, stream := codexCall(t, overloadBackend(&calls, 1), time.Minute)
	if status != fiber.StatusOK || strings.Contains(body, "event: error") || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q: want a complete stream", status, body)
	}
	if strings.Count(body, "event: message_start") != 1 || !strings.Contains(body, "event: ping") {
		t.Errorf("body %q: want one message_start and a ping", body)
	}
	if calls.Load() != 2 || !strings.Contains(stream, "upstream retry 1/2 in 1ms") || !strings.Contains(stream, "server_is_overloaded") {
		t.Errorf("calls %d, log %q", calls.Load(), stream)
	}
	if strings.Contains(stream, "stream failed") {
		t.Errorf("a retried stream is logged as failed: %q", stream)
	}
}

func TestOverloadRetriesRunOut(t *testing.T) {
	fastRetries(t, 1)
	var calls atomic.Int32
	_, body, _, stream := codexCall(t, overloadBackend(&calls, 10), time.Minute)
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("body %q: want the overload error after the retries", body)
	}
	if calls.Load() != 2 || !strings.Contains(stream, "upstream retry 1/1") || !strings.Contains(stream, "stream failed") {
		t.Errorf("calls %d, log %q", calls.Load(), stream)
	}
}

func TestLogStreamCanceledLine(t *testing.T) {
	var b bytes.Buffer
	logStreamCanceled(&b, time.Now().Add(-3*time.Second), "codex", "a -> b", "conn reused (idle 1s)")
	if got := b.String(); !strings.Contains(got, "stream canceled by client after 3") || !strings.HasSuffix(got, "[codex] a -> b: conn reused (idle 1s)\n") {
		t.Errorf("line %q", got)
	}
}
