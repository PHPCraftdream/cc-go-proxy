package server

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/gofiber/fiber/v2"
)

const authTestToken = "0123456789abcdef0123456789abcdef"

func newAuthApp(build profileBuilder) *fiber.App {
	app := fiber.New()
	idx := creds.StaticIndex("work", authTestToken)
	app.Post(callPrefix+":token/v1/messages", requireProfile(idx, build), func(c *fiber.Ctx) error {
		return c.SendString(requestConfig(c).Profile)
	})
	return app
}

func okBuild(profile string) (*config.Config, error) {
	return &config.Config{Profile: profile}, nil
}

func TestRequireProfile(t *testing.T) {
	tests := []struct {
		name string
		path string
		want int
	}{
		{"valid token", "/call/" + authTestToken + "/v1/messages", fiber.StatusOK},
		{"wrong token", "/call/" + authTestToken[:31] + "x/v1/messages", fiber.StatusNotFound},
		{"short token", "/call/abc/v1/messages", fiber.StatusNotFound},
		{"token prefix", "/call/" + authTestToken + "extra/v1/messages", fiber.StatusNotFound},
		{"no prefix", "/v1/messages", fiber.StatusNotFound},
		{"empty token", "/call//v1/messages", fiber.StatusNotFound},
	}
	app := newAuthApp(okBuild)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := app.Test(httptest.NewRequest("POST", tt.path, nil))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestRequireProfileBuildError(t *testing.T) {
	app := newAuthApp(func(string) (*config.Config, error) { return nil, errors.New("not logged in") })
	resp, err := app.Test(httptest.NewRequest("POST", "/call/"+authTestToken+"/v1/messages", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestMaskToken(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/call/secret/v1/messages", "/call/***/v1/messages"},
		{"/call/secret", "/call/***"},
		{"/call/secret/", "/call/***/"},
		{"/health", "/health"},
		{"/v1/messages", "/v1/messages"},
	}
	for _, tt := range tests {
		if got := maskToken(tt.in); got != tt.want {
			t.Errorf("maskToken(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
