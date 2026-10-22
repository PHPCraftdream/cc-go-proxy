package server

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

// postBody sends a body of size bytes and returns the status, or the error
// app.Test reports when the server refuses the body before any route runs.
func postBody(t *testing.T, app *fiber.App, size int) (int, error) {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/call/x/v1/messages", bytes.NewReader(make([]byte, size)))
	resp, err := app.Test(req, -1)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

func TestBodyLimitAllowsLongConversations(t *testing.T) {
	app := fiber.New(appConfig((&config.Config{}).MaxBodyBytes()))
	// Past fiber's 4 MiB default; no route is registered, so a request that gets
	// through the body check ends in 404.
	if got, err := postBody(t, app, 5<<20); err != nil || got != fiber.StatusNotFound {
		t.Fatalf("5 MiB body: status %d, err %v; want %d", got, err, fiber.StatusNotFound)
	}
}

func TestBodyOverLimitIsRejected(t *testing.T) {
	cfg := &config.Config{MaxBodyMB: 1}
	app := fiber.New(appConfig(cfg.MaxBodyBytes()))
	// fasthttp refuses the body before any route runs: app.Test then reports the
	// error instead of a response (a real client gets 413).
	got, err := postBody(t, app, 2<<20)
	rejected := got == fiber.StatusRequestEntityTooLarge || (err != nil && strings.Contains(err.Error(), "exceeds the given limit"))
	if !rejected {
		t.Fatalf("2 MiB body over a 1 MiB limit: status %d, err %v; want a rejection", got, err)
	}
}

// The error handler logs refused bodies and headers (token masked) and nothing else.
func TestErrorHandlerLogsRejections(t *testing.T) {
	old := logOut
	t.Cleanup(func() { logOut = old })

	run := func(code int) string {
		var buf syncBuf
		logOut = &buf
		app := fiber.New(appConfig(1 << 20))
		app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error {
			return fiber.NewError(code, "refused")
		})
		resp, err := app.Test(httptest.NewRequest(fiber.MethodPost, "/call/secret/v1/messages", strings.NewReader("abcd")), -1)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != code {
			t.Fatalf("status = %d, want %d", resp.StatusCode, code)
		}
		return buf.String()
	}

	got := run(fiber.StatusRequestEntityTooLarge)
	for _, want := range []string{"413 - rejected POST /call/***/v1/messages", "content-length 4", "raise listen.max_body_mb"} {
		if !strings.Contains(got, want) {
			t.Errorf("413 log %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("token leaked: %q", got)
	}
	if got := run(fiber.StatusRequestHeaderFieldsTooLarge); !strings.Contains(got, "431 - rejected") || strings.Contains(got, "max_body_mb") {
		t.Errorf("431 log = %q", got)
	}
	if got := run(fiber.StatusBadRequest); got != "" {
		t.Errorf("400 logged: %q", got)
	}
}
