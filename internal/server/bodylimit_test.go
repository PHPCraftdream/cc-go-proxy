package server

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

func postBody(t *testing.T, app *fiber.App, size int) int {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/call/x/v1/messages", bytes.NewReader(make([]byte, size)))
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestBodyLimitAllowsLongConversations(t *testing.T) {
	app := fiber.New(appConfig((&config.Config{}).MaxBodyBytes()))
	// Past fiber's 4 MiB default; no route is registered, so a request that gets
	// through the body check ends in 404.
	if got := postBody(t, app, 5<<20); got != fiber.StatusNotFound {
		t.Fatalf("5 MiB body: status %d, want %d", got, fiber.StatusNotFound)
	}
}

func TestBodyOverLimitIsRejected(t *testing.T) {
	cfg := &config.Config{MaxBodyMB: 1}
	app := fiber.New(appConfig(cfg.MaxBodyBytes()))
	if got := postBody(t, app, 2<<20); got != fiber.StatusRequestEntityTooLarge {
		t.Fatalf("2 MiB body over a 1 MiB limit: status %d, want %d", got, fiber.StatusRequestEntityTooLarge)
	}
}
