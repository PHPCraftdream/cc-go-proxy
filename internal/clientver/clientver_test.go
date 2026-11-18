package clientver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

func TestLatestFrom(t *testing.T) {
	status, body := http.StatusOK, `{"tag_name":"rust-v0.170.2","name":"0.170.2"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	if v, err := latestFrom(context.Background(), srv.Client(), srv.URL); err != nil || v != "0.170.2" {
		t.Errorf("latest = %q, %v", v, err)
	}
	body = `{"tag_name":"nightly"}`
	if _, err := latestFrom(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Error("a tag without a version was accepted")
	}
	status = http.StatusForbidden
	if _, err := latestFrom(context.Background(), srv.Client(), srv.URL); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("HTTP 403: err = %v", err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const providerKtav = "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\n"

func TestUpgraderRaise(t *testing.T) {
	path := writeConfig(t, "codex.client_version:: 0.169.0\n"+providerKtav)
	lookups := 0
	u := &Upgrader{Latest: func(context.Context) (string, error) { lookups++; return "0.170.0", nil }}
	ctx := context.Background()

	// A version the backend named is used as is, without a lookup.
	if to, err := u.Raise(ctx, path, "0.169.0", "0.169.5"); err != nil || to != "0.169.5" || lookups != 0 {
		t.Fatalf("named: %q, %v, lookups %d", to, err, lookups)
	}
	// Otherwise the newest release.
	if to, err := u.Raise(ctx, path, "0.169.5", ""); err != nil || to != "0.170.0" || lookups != 1 {
		t.Fatalf("latest: %q, %v, lookups %d", to, err, lookups)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "codex.client_version:: 0.170.0") {
		t.Fatalf("config:\n%s", b)
	}
	// A second lookup right away is refused, not repeated.
	if _, err := u.Raise(ctx, path, "0.170.0", ""); !errors.Is(err, ErrCooldown) || lookups != 1 {
		t.Errorf("cooldown: %v, lookups %d", err, lookups)
	}
	// Nothing newer: nothing written.
	if to, err := u.Raise(ctx, path, "0.170.0", "0.169.0"); err != nil || to != "" {
		t.Errorf("older: %q, %v", to, err)
	}
	// Target reads without writing.
	u2 := &Upgrader{Latest: func(context.Context) (string, error) { return "0.180.0", nil }}
	if to, err := u2.Target(ctx, "0.170.0", ""); err != nil || to != "0.180.0" {
		t.Errorf("target: %q, %v", to, err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "codex.client_version:: 0.170.0") {
		t.Errorf("Target changed the config:\n%s", b)
	}
}

func TestRefreshMappings(t *testing.T) {
	var version string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version = r.URL.Query().Get("client_version")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-6.3-astra"},{"slug":"gpt-6.3-sol"},{"slug":"gpt-6.2-terra"},{"slug":"gpt-6.2-luna"}]}`))
	}))
	defer srv.Close()
	old := codex.HTTPClient
	codex.HTTPClient = &http.Client{}
	defer func() { codex.HTTPClient = old }()

	managed := func(name string) string {
		return "    {\n        name: " + name + "\n        provider: codex\n        ## ccgp:managed-models begin " + name +
			"\n        models: [\n            {\n                match: *\n                model: gpt-6.2-astra\n                context_window: 922000\n            }\n        ]\n        ## ccgp:managed-models end " + name + "\n    }\n"
	}
	hand := "    {\n        name: hand\n        provider: codex\n        models: [\n            {\n                match: *\n                model: gpt-6.1-sol\n                context_window: 922000\n            }\n        ]\n    }\n"
	path := writeConfig(t, "providers: [\n    {\n        name: codex\n        type: codex\n        base_url: "+srv.URL+"\n    }\n]\nprofiles: [\n"+managed("a")+managed("b")+hand+"]\n")
	t.Setenv(config.ConfigFileEnv, path)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx := codex.WithClientVersion(context.Background(), "0.170.0")
	sel, changed, err := RefreshMappings(ctx, cfg, "codex", &oauth.Tokens{AccessToken: "t"})
	if err != nil || sel.Fable != "gpt-6.3-astra" || strings.Join(changed, ",") != "a,b" || version != "0.170.0" {
		t.Fatalf("refresh = %+v, %v, %v (catalog asked as %q)", sel, changed, err, version)
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "model: gpt-6.3-astra") != 2 || !strings.Contains(string(b), "model: gpt-6.1-sol") {
		t.Errorf("config after refresh:\n%s", b)
	}
	// Already current: nothing is rewritten.
	if _, changed, err = RefreshMappings(ctx, cfg, "codex", &oauth.Tokens{AccessToken: "t"}); err != nil || len(changed) != 0 {
		t.Errorf("second refresh changed %v, %v", changed, err)
	}
}
