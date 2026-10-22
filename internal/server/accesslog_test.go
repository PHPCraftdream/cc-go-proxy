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
