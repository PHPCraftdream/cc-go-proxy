package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/gofiber/fiber/v2"
)

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

				oldTransport := openAITransport
				openAITransport = connpool.NewRouter(0, 1, 1, nil)
				t.Cleanup(func() { openAITransport = oldTransport })
				cfg := &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "openai-secret", AnthropicAPIKey: "anthropic-secret"}
				app := fiber.New()
				app.Post("/v1/messages", func(c *fiber.Ctx) error { return handleMessages(c, cfg) })
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
	oldTransport := openAITransport
	openAITransport = connpool.NewRouter(0, 1, 1, nil)
	t.Cleanup(func() { openAITransport = oldTransport })
	cfg := &config.Config{OpenAIBaseURL: upstream.URL, OpenAIAPIKey: "openai-secret", AnthropicAPIKey: "anthropic-secret"}
	app := fiber.New()
	app.Post("/v1/messages", func(c *fiber.Ctx) error { return handleMessages(c, cfg) })
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
