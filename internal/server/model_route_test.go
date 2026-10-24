package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/gofiber/fiber/v2"
)

func TestModelRoute(t *testing.T) {
	cfg := &config.Config{
		Profile:      "work",
		DefaultModel: "backend-default",
		ModelRules:   []config.ModelRule{{Match: "*opus*", Model: "backend-opus"}},
	}
	app := fiber.New()
	app.Get(callPrefix+":token/model", requireProfile(creds.StaticIndex("work", authTestToken),
		func(string) (*config.Config, error) { return cfg, nil }), modelRoute)
	for _, tt := range []struct {
		query string
		want  string
	}{
		{"claude-opus-5-5", "backend-opus"},
		{"claude-sonnet-5-5", "backend-default"},
	} {
		resp, err := app.Test(httptest.NewRequest("GET", "/call/"+authTestToken+"/model?requested="+tt.query, nil))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]string
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusOK || len(result) != 3 || result["kind"] != "ccgp:model-route:v1" ||
			result["requestedModel"] != tt.query || result["backendModel"] != tt.want {
			t.Fatalf("unexpected route response: %s", raw)
		}
		if strings.Contains(string(raw), authTestToken) || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("route metadata must not expose auth or allow HTTP caching")
		}
	}
	for _, path := range []string{"/call/wrong/model?requested=opus", "/call/" + authTestToken + "/model"} {
		resp, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == fiber.StatusOK {
			t.Fatalf("unexpected success for %s", path)
		}
	}
}
