package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
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

// routeApp mounts modelRoute behind the real auth middleware.
func routeApp(build func(string) (*config.Config, error)) *fiber.App {
	app := fiber.New()
	app.Get(callPrefix+":token/model", requireProfile(creds.StaticIndex("work", authTestToken), build), modelRoute)
	return app
}

func routeGet(t *testing.T, app *fiber.App, path string) (int, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", path, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func TestModelRouteStatuses(t *testing.T) {
	app := routeApp(func(string) (*config.Config, error) { return &config.Config{}, nil })
	ok := "/call/" + authTestToken + "/model?requested="
	for _, tt := range []struct {
		name string
		path string
		want int
	}{
		{"unknown token", "/call/wrong/model?requested=opus", fiber.StatusNotFound},
		{"missing query", "/call/" + authTestToken + "/model", fiber.StatusBadRequest},
		{"empty value", ok, fiber.StatusBadRequest},
		{"too long", ok + strings.Repeat("a", 257), fiber.StatusBadRequest},
		{"max length", ok + strings.Repeat("a", 256), fiber.StatusOK},
		{"newline", ok + "a%0Ab", fiber.StatusBadRequest},
		{"carriage return", ok + "a%0Db", fiber.StatusBadRequest},
		{"nul", ok + "a%00b", fiber.StatusBadRequest},
	} {
		if got, body := routeGet(t, app, tt.path); got != tt.want {
			t.Errorf("%s: status %d, want %d (%s)", tt.name, got, tt.want, body)
		}
	}
}

func TestModelRouteMapping(t *testing.T) {
	app := routeApp(func(string) (*config.Config, error) {
		return &config.Config{OpusModel: "my-opus", ModelRules: []config.ModelRule{{Match: "special-*", Model: "ruled"}}}, nil
	})
	for _, tt := range []struct{ requested, want string }{
		{"Claude-OPUS-5", "my-opus"},   // tier match ignores case
		{"special-1", "ruled"},         // rule wins
		{"gpt-4o-mini", "gpt-4o-mini"}, // non-Claude passes through
		{"gpt%204%2Fx", "gpt 4/x"},     // query is decoded and echoed verbatim
		{"claude-haiku-5-5", converter.DefaultHaikuModel},
	} {
		status, body := routeGet(t, app, "/call/"+authTestToken+"/model?requested="+tt.requested)
		var got map[string]string
		if err := json.Unmarshal([]byte(body), &got); err != nil || status != fiber.StatusOK {
			t.Fatalf("%s: status %d body %q err %v", tt.requested, status, body, err)
		}
		if got["backendModel"] != tt.want {
			t.Errorf("%s: backendModel %q, want %q", tt.requested, got["backendModel"], tt.want)
		}
	}
}

func TestModelRouteFollowsReloadedConfig(t *testing.T) {
	cfg := &config.Config{DefaultModel: "before"}
	app := routeApp(func(string) (*config.Config, error) { return cfg, nil })
	path := "/call/" + authTestToken + "/model?requested=claude-sonnet-5-5"
	if _, body := routeGet(t, app, path); !strings.Contains(body, `"backendModel":"before"`) {
		t.Fatalf("before reload: %s", body)
	}
	cfg = &config.Config{DefaultModel: "after"}
	if _, body := routeGet(t, app, path); !strings.Contains(body, `"backendModel":"after"`) {
		t.Fatalf("after reload: %s", body)
	}
}

func TestModelRouteConfigErrorIs503(t *testing.T) {
	app := routeApp(func(string) (*config.Config, error) { return nil, errors.New("config broken") })
	status, body := routeGet(t, app, "/call/"+authTestToken+"/model?requested=opus")
	if status != fiber.StatusServiceUnavailable || strings.Contains(body, "backendModel") {
		t.Fatalf("status %d body %s", status, body)
	}
}

// The route must be mounted by the same function Start uses, behind auth.
func TestRegisterCallRoutesMountsModelRoute(t *testing.T) {
	app := fiber.New()
	registerCallRoutes(app, requireProfile(creds.StaticIndex("work", authTestToken),
		func(string) (*config.Config, error) { return &config.Config{Profile: "work", DefaultModel: "mapped"}, nil }),
		&sessions{store: creds.Store{Dir: t.TempDir()}})

	status, body := routeGet(t, app, "/call/"+authTestToken+"/model?requested=claude-opus-5-5")
	if status != fiber.StatusOK || !strings.Contains(body, `"backendModel":"mapped"`) {
		t.Fatalf("mounted route: status %d body %s", status, body)
	}
	if status, _ := routeGet(t, app, "/call/wrong/model?requested=claude-opus-5-5"); status != fiber.StatusNotFound {
		t.Fatalf("unknown token: status %d, want 404", status)
	}
	if status, body := routeGet(t, app, "/call/"+authTestToken+"/"); status != fiber.StatusOK || !strings.Contains(body, `"profile":"work"`) {
		t.Fatalf("info route: status %d body %s", status, body)
	}
}
