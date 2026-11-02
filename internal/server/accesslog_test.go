package server

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func logLine(t *testing.T, handler fiber.Handler) string {
	t.Helper()
	var buf bytes.Buffer
	app := fiber.New()
	app.Use(accessLog(&buf))
	app.Post("/call/:token/v1/messages", handler)
	resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/call/secret/v1/messages", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return buf.String()
}

func TestAccessLogShowsErrorReason(t *testing.T) {
	got := logLine(t, func(c *fiber.Ctx) error {
		return anthropicError(c, fiber.StatusBadGateway, "api_error", "Post \"https://x/responses\":\n  net/http: timeout awaiting response headers")
	})
	for _, want := range []string{
		" 502 - ", "POST /call/***/v1/messages",
		`: Post "https://x/responses": net/http: timeout awaiting response headers` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("token leaked: %q", got)
	}
}

func TestAccessLogHasNoReasonOnSuccess(t *testing.T) {
	got := logLine(t, func(c *fiber.Ctx) error {
		c.Locals(localsErr, "left over")
		return c.SendStatus(fiber.StatusOK)
	})
	if !strings.Contains(got, " 200 - ") || strings.Contains(got, ": ") {
		t.Errorf("log = %q", got)
	}
}

// The model lookup and the health check are repeated all day: a success says
// nothing and gets no line, a failure does.
func TestAccessLogSkipsSuccessfulQuietRoutes(t *testing.T) {
	status := fiber.StatusOK
	var buf bytes.Buffer
	app := fiber.New()
	app.Use(accessLog(&buf))
	h := func(c *fiber.Ctx) error { return c.SendStatus(status) }
	app.Get("/health", h)
	app.Get("/call/:token/model", h)
	app.Post("/call/:token/model", h)
	app.Get("/call/:token/other", h)

	hit := func(method, path string) {
		t.Helper()
		resp, err := app.Test(httptest.NewRequest(method, path, nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	hit(fiber.MethodGet, "/health")
	hit(fiber.MethodGet, "/call/secret/model")
	if buf.Len() != 0 {
		t.Fatalf("successful quiet routes were logged: %q", buf.String())
	}
	hit(fiber.MethodGet, "/call/secret/other")
	hit(fiber.MethodPost, "/call/secret/model")
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Fatalf("log = %q, want a line for another route and for a POST", buf.String())
	}
	buf.Reset()
	status = fiber.StatusUnauthorized
	hit(fiber.MethodGet, "/call/secret/model")
	hit(fiber.MethodGet, "/health")
	if got := buf.String(); !strings.Contains(got, " 401 - ") || strings.Count(got, "\n") != 2 || strings.Contains(got, "secret") {
		t.Errorf("failed quiet routes: log = %q", got)
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a \n\t b", 10); got != "a b" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("я", 400)
	got := oneLine(long, 300)
	if !strings.HasSuffix(got, "…") || len(got) > 300+len("…") {
		t.Errorf("len %d", len(got))
	}
}
