package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/clientver"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/gofiber/fiber/v2"
)

// A backend that asks for a newer client gets codex.client_version raised in
// config.ktav and the managed mapping refreshed; the client is told to retry
// (503), and the retry, made with the new version, goes through.
func TestBackendAskingForNewerClientRaisesConfig(t *testing.T) {
	var refuseAll atomic.Bool
	var mu sync.Mutex
	var agents []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-6.3-astra"},{"slug":"gpt-6.3-sol"},{"slug":"gpt-6.2-terra"},{"slug":"gpt-6.2-luna"}]}`))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		agents = append(agents, r.Header.Get("User-Agent"))
		mu.Unlock()
		if refuseAll.Load() || r.Header.Get("User-Agent") != "codex_cli_rs/0.170.0 (ccgp)" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The 'gpt-6.3-astra' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again.","type":"invalid_request_error"}}`))
			return
		}
		okStream(w)
	})
	h := newCodexWSTestHarness(t, backend, "http")
	path := filepath.Join(t.TempDir(), "config.ktav")
	text := "providers: [\n    {\n        name: codex\n        type: codex\n        base_url: " + h.backend.URL + "\n    }\n]\n" +
		"profiles: [\n    {\n        name: codex\n        provider: codex\n        ## ccgp:managed-models begin codex\n        models: [\n" +
		"            {\n                match: *\n                model: gpt-6.2-astra\n                context_window: 922000\n            }\n" +
		"        ]\n        ## ccgp:managed-models end codex\n    }\n]\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	h.cfg.ConfigFile = path
	h.cfg.Profiles = map[string]*config.Profile{"codex": {Name: "codex", Provider: "codex"}}
	old := upgrader
	lookups := 0
	upgrader = &clientver.Upgrader{Latest: func(context.Context) (string, error) { lookups++; return "0.170.0", nil }}
	t.Cleanup(func() { upgrader = old })

	body := `{"model":"gpt-6.3-astra","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	status, reply := h.post(t, body)
	if status != fiber.StatusServiceUnavailable || !strings.Contains(reply, "raised codex.client_version 0.169.0 -> 0.170.0") ||
		!strings.Contains(reply, "refreshed the model mapping of codex") {
		t.Fatalf("first reply %d: %s", status, reply)
	}
	b, _ := os.ReadFile(path)
	got, _ := config.CodexClientVersionText(string(b))
	if got != "0.170.0" || !strings.Contains(string(b), "model: gpt-6.3-astra") {
		t.Fatalf("config after the refusal:\n%s", b)
	}

	// The server reloads the changed config (cfgSource); here the field is set as a reload would.
	h.cfg.CodexClientVersionSet = got
	if status, reply = h.post(t, body); status != fiber.StatusOK {
		t.Fatalf("retry %d: %s", status, reply)
	}
	mu.Lock()
	if len(agents) != 2 || agents[0] != "codex_cli_rs/0.169.0 (ccgp)" || agents[1] != "codex_cli_rs/0.170.0 (ccgp)" {
		t.Errorf("user agents = %q", agents)
	}
	mu.Unlock()

	// Still refused with nothing newer to try: the backend's error goes to the client.
	refuseAll.Store(true)
	status, reply = h.post(t, body)
	if status != fiber.StatusBadRequest || !strings.Contains(reply, "requires a newer version") || lookups != 1 {
		t.Errorf("refused again %d (lookups %d): %s", status, lookups, reply)
	}
}
