package codexh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/clientver"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/tier"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/PHPCraftdream/cc-go-proxy/pkg/models"
	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v2"
)

const raiseConfigText = "codex.client_version:: 0.169.0\nproviders: [\n    {\n        name: codex\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: codex\n        provider: codex\n    }\n]\n"

func raiseFixture(t *testing.T, latest func(context.Context) (string, error)) (*config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(path, []byte(raiseConfigText), 0o600); err != nil {
		t.Fatal(err)
	}
	old := tier.Upgrader
	tier.Upgrader = &clientver.Upgrader{Latest: latest}
	t.Cleanup(func() { tier.Upgrader = old })
	cfg := &config.Config{
		ConfigFile: path,
		Profile:    "codex", ProviderName: "codex", CodexClientVersionSet: "0.169.0",
		Providers: map[string]*config.Provider{"codex": {Name: "codex", Type: "codex"}},
		Profiles:  map[string]*config.Profile{"codex": {Name: "codex", Provider: "codex"}},
	}
	return cfg, path
}

func TestRaiseClientVersionNotes(t *testing.T) {
	t.Run("env-only mode has nothing to write", func(t *testing.T) {
		note, raised := tier.RaiseClientVersion(&config.Config{}, nil, "")
		if raised || !strings.Contains(note, "env-only mode") {
			t.Errorf("%v %q", raised, note)
		}
	})
	t.Run("lookup failure points at the config", func(t *testing.T) {
		cfg, path := raiseFixture(t, func(context.Context) (string, error) { return "", errors.New("offline") })
		note, raised := tier.RaiseClientVersion(cfg, nil, "")
		if raised || !strings.Contains(note, "offline") || !strings.Contains(note, path) || !strings.Contains(note, "ccgp --models apply") {
			t.Errorf("%v %q", raised, note)
		}
		if got, _ := os.ReadFile(path); string(got) != raiseConfigText {
			t.Errorf("the config changed:\n%s", got)
		}
	})
	t.Run("nothing newer is said plainly", func(t *testing.T) {
		cfg, path := raiseFixture(t, nil)
		note, raised := tier.RaiseClientVersion(cfg, nil, "0.100.0")
		if raised || !strings.Contains(note, "the newest Codex CLI release") {
			t.Errorf("%v %q", raised, note)
		}
		if got, _ := os.ReadFile(path); string(got) != raiseConfigText {
			t.Errorf("the config changed:\n%s", got)
		}
	})
	t.Run("a raise survives a mapping refresh that cannot run", func(t *testing.T) {
		cfg, path := raiseFixture(t, nil)
		noLogin := &oauth.Session{Store: oauth.Store{Path: filepath.Join(t.TempDir(), "none.json")}, Client: &oauth.Client{}}
		note, raised := tier.RaiseClientVersion(cfg, noLogin, "0.171.0")
		if !raised || !strings.Contains(note, "0.169.0 -> 0.171.0") || !strings.Contains(note, "model mappings not refreshed") {
			t.Errorf("%v %q", raised, note)
		}
		if got, _ := os.ReadFile(path); !strings.Contains(string(got), "codex.client_version:: 0.171.0") {
			t.Errorf("the config was not raised:\n%s", got)
		}
	})
}

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
	old := tier.Upgrader
	lookups := 0
	tier.Upgrader = &clientver.Upgrader{Latest: func(context.Context) (string, error) { lookups++; return "0.170.0", nil }}
	t.Cleanup(func() { tier.Upgrader = old })

	body := `{"model":"gpt-6.3-astra","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	status, reply := h.post(t, body)
	if status != fiber.StatusServiceUnavailable || !strings.Contains(reply, "raised codex.client_version 0.169.0 -") || !strings.Contains(reply, "0.170.0;") ||
		!strings.Contains(reply, "refreshed the model mapping of codex") {
		t.Fatalf("first reply %d: %s", status, reply)
	}
	b, _ := os.ReadFile(path)
	got, _ := cfgedit.CodexClientVersionText(string(b))
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

func resetFastTiers(t *testing.T) {
	t.Helper()
	reset := func() {
		tier.FastTiers.Lock()
		tier.FastTiers.Entries = make(map[[32]byte]tier.FastTierEntry)
		tier.FastTiers.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func fastTierCount() int {
	tier.FastTiers.Lock()
	defer tier.FastTiers.Unlock()
	return len(tier.FastTiers.Entries)
}

func fastSession(t *testing.T, access, account string) *oauth.Session {
	t.Helper()
	store := oauth.Store{Path: filepath.Join(t.TempDir(), "oauth.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: access, RefreshToken: "refresh", AccountID: account, LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return &oauth.Session{Store: store, Client: &oauth.Client{}}
}

// fastBackend serves a catalog with a Fast tier for gpt-6.2-sol and gpt-6.2-astra
// and none for gpt-6.2-terra; while fail is set it answers 500.
func fastBackend(t *testing.T, fail *atomic.Bool, calls *atomic.Int32) (*httptest.Server, *config.Config) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6.2-sol","service_tiers":[{"id":"sol-fast","name":"Fast"}]},{"slug":"gpt-6.2-astra","service_tiers":[{"id":"astra-fast","name":"Fast"}]},{"slug":"gpt-6.2-terra"}]}`)
	}))
	t.Cleanup(srv.Close)
	old := codex.HTTPClient
	codex.HTTPClient = &http.Client{}
	t.Cleanup(func() { codex.HTTPClient = old })
	cfg := &config.Config{
		Profile: "codex", ProviderName: "codex",
		Providers: map[string]*config.Provider{"codex": {Name: "codex", Type: "codex", BaseURL: srv.URL}},
	}
	return srv, cfg
}

func TestFastTierCachesPerRouteAndExpires(t *testing.T) {
	resetFastTiers(t)
	var fail atomic.Bool
	var calls atomic.Int32
	srv, cfg := fastBackend(t, &fail, &calls)
	s := fastSession(t, "access-1", "acct-1")
	ctx := context.Background()
	get := func(cfg *config.Config, s *oauth.Session, model string) string {
		t.Helper()
		tier, status, err := tier.FastTier(ctx, cfg, srv.URL, model, s)
		if err != nil || status != 0 {
			t.Fatalf("%s: %q, %d, %v", model, tier, status, err)
		}
		return tier
	}

	if get(cfg, s, "gpt-6.2-sol") != "sol-fast" || calls.Load() != 1 {
		t.Fatalf("first lookup: calls %d", calls.Load())
	}
	if get(cfg, s, "gpt-6.2-sol") != "sol-fast" || calls.Load() != 1 {
		t.Fatalf("a repeat within the TTL asked the catalog again: calls %d", calls.Load())
	}
	// Another model, another account or token, another client version: own entries.
	if get(cfg, s, "gpt-6.2-astra") != "astra-fast" || calls.Load() != 2 {
		t.Fatalf("another model shared an entry: calls %d", calls.Load())
	}
	if get(cfg, fastSession(t, "access-2", "acct-1"), "gpt-6.2-sol") != "sol-fast" || calls.Load() != 3 {
		t.Fatalf("another token shared an entry: calls %d", calls.Load())
	}
	if get(cfg, fastSession(t, "access-1", "acct-2"), "gpt-6.2-sol") != "sol-fast" || calls.Load() != 4 {
		t.Fatalf("another account shared an entry: calls %d", calls.Load())
	}
	newer := *cfg
	newer.CodexClientVersionSet = "0.999.0"
	if get(&newer, s, "gpt-6.2-sol") != "sol-fast" || calls.Load() != 5 {
		t.Fatalf("another client version shared an entry: calls %d", calls.Load())
	}
	if n := fastTierCount(); n != 5 {
		t.Fatalf("entries = %d, want 5", n)
	}

	// An expired entry is looked up again, and is replaced, not added.
	tier.FastTiers.Lock()
	for k, v := range tier.FastTiers.Entries {
		v.Expires = time.Now().Add(-time.Second)
		tier.FastTiers.Entries[k] = v
	}
	tier.FastTiers.Unlock()
	if get(cfg, s, "gpt-6.2-sol") != "sol-fast" || calls.Load() != 6 {
		t.Fatalf("an expired entry was served: calls %d", calls.Load())
	}
	if get(cfg, s, "gpt-6.2-sol") != "sol-fast" || calls.Load() != 6 {
		t.Fatalf("the refreshed entry was not kept: calls %d", calls.Load())
	}
}

func TestFastTierFailuresAreNotCached(t *testing.T) {
	resetFastTiers(t)
	var fail atomic.Bool
	var calls atomic.Int32
	srv, cfg := fastBackend(t, &fail, &calls)
	s := fastSession(t, "access", "acct")
	ctx := context.Background()

	fail.Store(true)
	if tier, status, err := tier.FastTier(ctx, cfg, srv.URL, "gpt-6.2-sol", s); err == nil || status != 502 || tier != "" || !strings.Contains(err.Error(), "Fast catalog lookup failed") {
		t.Fatalf("catalog failure: %q, %d, %v", tier, status, err)
	}
	if fastTierCount() != 0 {
		t.Fatal("a failed lookup was cached")
	}
	fail.Store(false)
	if tier, status, err := tier.FastTier(ctx, cfg, srv.URL, "gpt-6.2-sol", s); err != nil || status != 0 || tier != "sol-fast" {
		t.Fatalf("after recovery: %q, %d, %v", tier, status, err)
	}

	// A model without the tier is a 400 every time, never a cached success.
	for i := 0; i < 2; i++ {
		before := calls.Load()
		if _, status, err := tier.FastTier(ctx, cfg, srv.URL, "gpt-6.2-terra", s); err == nil || status != 400 || !strings.Contains(err.Error(), "does not advertise") {
			t.Fatalf("no Tier: %d, %v", status, err)
		}
		if calls.Load() == before {
			t.Fatal("a refusal was served from the cache")
		}
	}
	if _, status, err := tier.FastTier(ctx, cfg, srv.URL, "gpt-9-absent", s); err == nil || status != 400 || !strings.Contains(err.Error(), "absent from the account catalog") {
		t.Fatalf("absent model: %d, %v", status, err)
	}
	if n := fastTierCount(); n != 1 {
		t.Fatalf("entries = %d, want only the one success", n)
	}
}

func TestFastTierWithoutLoginIs401(t *testing.T) {
	resetFastTiers(t)
	var fail atomic.Bool
	var calls atomic.Int32
	srv, cfg := fastBackend(t, &fail, &calls)
	s := &oauth.Session{Store: oauth.Store{Path: filepath.Join(t.TempDir(), "none.json")}, Client: &oauth.Client{}}
	if _, status, err := tier.FastTier(context.Background(), cfg, srv.URL, "gpt-6.2-sol", s); err == nil || status != 401 {
		t.Fatalf("no login: %d, %v", status, err)
	}
	if calls.Load() != 0 {
		t.Error("the catalog was asked without a token")
	}
}

func TestFastTierCacheIsBounded(t *testing.T) {
	resetFastTiers(t)
	var fail atomic.Bool
	var calls atomic.Int32
	srv, cfg := fastBackend(t, &fail, &calls)
	s := fastSession(t, "access", "acct")
	now := time.Now()
	tier.FastTiers.Lock()
	for i := 0; i < tier.FastCatalogMaxEntries; i++ {
		tier.FastTiers.Entries[[32]byte{byte(i), 1}] = tier.FastTierEntry{Tier: "old", Expires: now.Add(time.Hour + time.Duration(i)*time.Minute)}
	}
	tier.FastTiers.Unlock()

	if _, _, err := tier.FastTier(context.Background(), cfg, srv.URL, "gpt-6.2-sol", s); err != nil {
		t.Fatal(err)
	}
	tier.FastTiers.Lock()
	defer tier.FastTiers.Unlock()
	if len(tier.FastTiers.Entries) != tier.FastCatalogMaxEntries {
		t.Fatalf("entries = %d, want %d", len(tier.FastTiers.Entries), tier.FastCatalogMaxEntries)
	}
	if _, ok := tier.FastTiers.Entries[[32]byte{0, 1}]; ok {
		t.Error("the entry closest to expiry was kept")
	}
	if _, ok := tier.FastTiers.Entries[[32]byte{1, 1}]; !ok {
		t.Error("a later entry was evicted")
	}
}

// A catalog that cannot be read refuses the Fast request with a 502 before any
// inference call: the request is not silently sent as Standard.
func TestFastCatalogFailureRefusesTheRequest(t *testing.T) {
	resetFastTiers(t)
	var inference atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		inference.Add(1)
		okStream(w)
	})
	h := newCodexWSTestHarness(t, backend, "http")
	body := `{"model":"gpt-6.2-sol","speed":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	status, reply := h.post(t, body)
	if status != 502 || !strings.Contains(reply, "Fast catalog lookup failed") || !strings.Contains(reply, `"api_error"`) {
		t.Fatalf("status %d: %s", status, reply)
	}
	if inference.Load() != 0 {
		t.Errorf("%d inference calls were made after the refusal", inference.Load())
	}
	// Standard needs no catalog at all.
	body = `{"model":"gpt-6.2-sol","speed":"standard","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	if status, reply = h.post(t, body); status != 200 || inference.Load() != 1 {
		t.Fatalf("standard: %d (inference %d): %s", status, inference.Load(), reply)
	}
}

func TestFastRequestPoliciesAndFallback(t *testing.T) {
	var mu sync.Mutex
	var catalogCalls, handshakes int
	var requests []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			mu.Lock()
			catalogCalls++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6.2-sol","service_tiers":[{"id":"priority-fixture","name":"Fast"}]},{"slug":"gpt-6.2-terra","additional_speed_tiers":["fast"]}]}`)
			return
		}
		// Refuse the websocket handshake to exercise the HTTP fallback.
		if r.Header.Get("Upgrade") != "" {
			mu.Lock()
			handshakes++
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n")
	})
	h := newCodexWSTestHarness(t, backend, "auto")
	h.cfg.Profiles = map[string]*config.Profile{"codex": {SpeedRules: map[string]string{"sol": "fast"}}}
	body := func(model, speed string) string {
		return fmt.Sprintf(`{"model":%q,"speed":%q,"stream":true,"metadata":{"user_id":"fast-policy"},"messages":[{"role":"user","content":"hi"}]}`, model, speed)
	}
	for i := 0; i < 2; i++ {
		if status, reply := h.post(t, body("gpt-6.2-sol", "standard")); status != 200 {
			t.Fatalf("fast override: %d %s", status, reply)
		}
	}
	mu.Lock()
	if handshakes == 0 || catalogCalls != 1 || len(requests) != 2 || !strings.Contains(requests[0], `"service_tier":"priority-fixture"`) || !strings.Contains(requests[1], `"service_tier":"priority-fixture"`) {
		t.Fatalf("fast: catalog=%d requests=%v", catalogCalls, requests)
	}
	mu.Unlock()
	h.cfg.Profiles["codex"].SpeedRules["sol"] = "standard"
	if status, reply := h.post(t, body("gpt-6.2-sol", "fast")); status != 200 {
		t.Fatalf("standard override: %d %s", status, reply)
	}
	mu.Lock()
	if catalogCalls != 1 || len(requests) != 3 || strings.Contains(requests[2], "service_tier") {
		t.Fatalf("standard: catalog=%d requests=%v", catalogCalls, requests)
	}
	mu.Unlock()
	if status, _ := h.post(t, body("gpt-6.2-terra", "fast")); status != 400 {
		t.Fatalf("unsupported status %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 || catalogCalls != 2 {
		t.Fatalf("unsupported called inference: catalog=%d requests=%v", catalogCalls, requests)
	}
}

func TestPersistedFastPolicyRequests(t *testing.T) {
	var mu sync.Mutex
	var catalogs int
	var requests []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+codexWSTestAccessToken {
			t.Errorf("missing synthetic OAuth authorization on %s", r.URL.Path)
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/models") {
			catalogs++
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6.2-astra","service_tiers":[{"id":"fixture-priority","name":"Fast"}]},{"slug":"gpt-6.3-astra","service_tiers":[{"id":"fixture-new","name":"Fast"}]}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n")
	})
	h := newCodexWSTestHarness(t, backend, "http")
	path := filepath.Join(t.TempDir(), "config.ktav")
	text := fmt.Sprintf("providers: [\n    {\n        name: codex\n        type: codex\n        base_url: %s\n    }\n]\nprofiles: [\n    {\n        name: codex\n        provider: codex\n    }\n    {\n        name: other\n        provider: codex\n    }\n]\n", h.backend.URL)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigFileEnv, path)
	edit := func(value string) string {
		t.Helper()
		backup, err := cfgedit.EditFile(path, func(s string) (string, error) {
			return cfgedit.SetProfileSpeedText(s, "codex", "astra", value)
		})
		if err != nil {
			t.Fatal(err)
		}
		return backup
	}
	post := func(profile, model, speed, tier string, wantCatalogs int) {
		t.Helper()
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		selected, err := cfg.ForProfile(profile, "")
		if err != nil {
			t.Fatal(err)
		}
		selected.CodexTransport = "http"
		*h.cfg = *selected
		body := fmt.Sprintf(`{"model":%q,"speed":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, model, speed)
		if status, reply := h.post(t, body); status != 200 {
			t.Fatalf("%s %s: %d %s", profile, speed, status, reply)
		}
		mu.Lock()
		defer mu.Unlock()
		request := requests[len(requests)-1]
		if catalogs != wantCatalogs || !strings.Contains(request, `"model":"`+model+`"`) || (tier == "" && strings.Contains(request, `"service_tier"`)) || (tier != "" && !strings.Contains(request, `"service_tier":"`+tier+`"`)) {
			t.Fatalf("catalogs=%d want=%d request=%s tier=%q", catalogs, wantCatalogs, request, tier)
		}
	}
	post("codex", "gpt-6.2-astra", "standard", "", 0)
	edit("fast")
	post("codex", "gpt-6.2-astra", "standard", "fixture-priority", 1)
	post("other", "gpt-6.2-astra", "standard", "", 1)
	post("codex", "gpt-6.3-astra", "standard", "fixture-new", 2)
	edit("standard")
	post("codex", "gpt-6.2-astra", "fast", "", 2)
	edit("")
	post("codex", "gpt-6.2-astra", "standard", "", 2)
	post("codex", "gpt-6.2-astra", "fast", "fixture-priority", 2)
	if backup := edit(""); backup != "" {
		t.Fatal("repeated unset created a backup")
	}
	post("other", "gpt-6.2-astra", "standard", "", 2)
}

func TestFastWebsocketSwitch(t *testing.T) {
	var mu sync.Mutex
	var frames []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"models":[{"slug":"gpt-6.2-sol","service_tiers":[{"id":"priority-fixture","name":"Fast"}]}]}`)
			return
		}
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for i := 0; i < 2; i++ {
			frame := readCodexWSTestFrame(t, conn)
			if frame == nil {
				return
			}
			mu.Lock()
			frames = append(frames, string(frame))
			mu.Unlock()
			writeCodexWSTestEvents(t, conn, `{"type":"response.created","response":{"id":"r"}}`, `{"type":"response.completed","response":{"id":"r"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	for _, speed := range []string{"standard", "fast"} {
		body := fmt.Sprintf(`{"model":"gpt-6.2-sol","speed":%q,"stream":true,"metadata":{"user_id":"tier-switch"},"messages":[{"role":"user","content":"hello"}]}`, speed)
		if status, reply := h.post(t, body); status != 200 {
			t.Fatalf("%s: %d %s", speed, status, reply)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 2 || strings.Contains(frames[0], "service_tier") || !strings.Contains(frames[1], `"service_tier":"priority-fixture"`) || strings.Contains(frames[1], "previous_response_id") {
		t.Fatalf("frames: %v", frames)
	}
}

// codexCallWith is codexCall with the upstream client and a change to the profile's config given.
func codexCallWith(t *testing.T, backend http.Handler, client *http.Client, tweak func(*config.Config)) (status int, body, access, stream string) {
	t.Helper()
	srv := httptest.NewServer(backend)
	defer srv.Close()

	oldClient, oldLog := codex.HTTPClient, shared.LogOut
	t.Cleanup(func() { codex.HTTPClient, shared.LogOut = oldClient, oldLog })
	codex.HTTPClient = client
	var sl syncBuf
	shared.LogOut = &sl

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sess := &oauth.Session{Store: store, Client: &oauth.Client{}}
	cfg := &config.Config{
		Profile:      "codex",
		ProviderName: "codex",
		Providers:    map[string]*config.Provider{"codex": {Name: "codex", Type: "codex", BaseURL: srv.URL}},
	}
	if tweak != nil {
		tweak(cfg)
	}

	var al syncBuf
	app := fiber.New()
	app.Use(rt.AccessLog(&al))
	app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error { return HandleCodexMessages(c, cfg, sess) })
	reqBody := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	if codexTestBody != "" {
		reqBody = codexTestBody
	}
	req := httptest.NewRequest(fiber.MethodPost, "/call/secret/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	lastCodexReply, lastCodexClose = resp.Header.Clone(), resp.Close
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), al.String(), sl.String()
}

// connectProxy is an HTTP CONNECT proxy that counts its tunnels.
func connectProxy(t *testing.T) (proxyURL string, tunnels *atomic.Int32) {
	t.Helper()
	tunnels = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		tunnels.Add(1)
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		_, _ = io.Copy(up, c)
		_ = up.Close()
	}))
	t.Cleanup(srv.Close)
	return srv.URL, tunnels
}

// A profile uses the global proxy unless it has a proxy of its own, or none.
func TestCodexRequestsFollowTheProfileProxy(t *testing.T) {
	var calls atomic.Int32
	backend := overloadBackend(&calls, 0) // always a complete answer
	global, viaGlobal := connectProxy(t)
	own, viaOwn := connectProxy(t)

	run := func(profile string) {
		t.Helper()
		router := connpool.NewRouter(0, 1, 1, nil)
		status, body, _, _ := codexCallWith(t, backend, &http.Client{Transport: router}, func(cfg *config.Config) {
			cfg.Profile = profile
			cfg.ConnectionsProxy = global
			cfg.Profiles = map[string]*config.Profile{
				"inherits": {Name: "inherits", Provider: "codex"},
				"direct":   {Name: "direct", Provider: "codex", Proxy: config.ProxyNone},
				"own":      {Name: "own", Provider: "codex", Proxy: own},
			}
		})
		if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
			t.Fatalf("%s: status %d, body %q", profile, status, body)
		}
	}
	run("inherits")
	if g, o := viaGlobal.Load(), viaOwn.Load(); g != 1 || o != 0 {
		t.Fatalf("inherits: tunnels global %d, own %d; want 1 and 0", g, o)
	}
	run("direct")
	if g, o := viaGlobal.Load(), viaOwn.Load(); g != 1 || o != 0 {
		t.Fatalf("direct: tunnels global %d, own %d; want no new tunnel", g, o)
	}
	run("own")
	if g, o := viaGlobal.Load(), viaOwn.Load(); g != 1 || o != 1 {
		t.Fatalf("own: tunnels global %d, own %d; want 1 and 1", g, o)
	}
}

func TestOpenAIRequestsFollowTheProfileProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer backend.Close()
	global, viaGlobal := connectProxy(t)
	oldT := rt.OpenAITransport
	t.Cleanup(func() { rt.OpenAITransport = oldT })

	call := func(profile string, proxy string) {
		t.Helper()
		rt.OpenAITransport = connpool.NewRouter(0, 1, 1, nil)
		cfg := &config.Config{
			Profile: profile, OpenAIBaseURL: backend.URL, ConnectionsProxy: global,
			Profiles: map[string]*config.Profile{profile: {Name: profile, Proxy: proxy}},
		}
		if _, err := rt.CallOpenAIInternal(&models.OpenAIRequest{Model: "m"}, cfg); err != nil {
			t.Fatalf("%s: %v", profile, err)
		}
	}
	call("inherits", "")
	if g := viaGlobal.Load(); g != 1 {
		t.Fatalf("inherits: tunnels = %d, want 1", g)
	}
	call("direct", config.ProxyNone)
	if g := viaGlobal.Load(); g != 1 {
		t.Fatalf("direct: tunnels = %d, want no new one", g)
	}
}

func TestUpstreamCtxCarriesTheProfileChoice(t *testing.T) {
	global, _ := proxydial.Parse("socks5://127.0.0.1:1080")
	cfg := &config.Config{
		Profile: "p", ConnectionsProxy: global.String(),
		Profiles: map[string]*config.Profile{"p": {Name: "p", Proxy: config.ProxyNone}},
	}
	if c := proxydial.ChoiceFor(rt.UpstreamCtx(context.Background(), cfg)); !c.Direct {
		t.Errorf("profile none: %+v", c)
	}
	cfg.Profiles["p"].Proxy = ""
	if c := proxydial.ChoiceFor(rt.UpstreamCtx(context.Background(), cfg)); c.URL == nil || c.URL.Host != "127.0.0.1:1080" {
		t.Errorf("inheriting profile: %+v", c)
	}
}

// lastCodexClose is whether that reply closed its connection (the client drops the header).
var lastCodexClose bool

// A registration is active only for the lifetime of its request, including
// paths that fail before a response can be translated.
func TestCodexPromptRegistrationFinishesOnEveryExit(t *testing.T) {
	oldWatch := obs.PromptWatch
	t.Cleanup(func() { obs.PromptWatch = oldWatch })
	keyedBody := func(stream bool) string {
		return `{"model":"claude-opus-5-5","max_tokens":10,"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `,"metadata":{"user_id":"shared-subagent-session"},"messages":[{"role":"user","content":"hi"}]}`
	}
	assertFinished := func(t *testing.T) {
		t.Helper()
		active, entries, unended := obs.PromptWatch.Pending()
		if active != 0 {
			t.Errorf("active prompt keys remain: %d", active)
		}
		if entries != 1 {
			t.Errorf("watched entries = %d, want one", entries)
		}
		for _, key := range unended {
			t.Errorf("current watched request %q has no end time", key)
		}
	}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { okStream(w) })

	t.Run("transport error", func(t *testing.T) {
		obs.PromptWatch = &obs.CacheWatch{}
		codexTestBody = keyedBody(true)
		t.Cleanup(func() { codexTestBody = "" })
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("injected transport failure")
		})}
		status, _, _, _ := codexCallWith(t, backend, client, nil)
		if status != fiber.StatusBadGateway {
			t.Fatalf("status = %d, want 502", status)
		}
		assertFinished(t)
	})

	t.Run("non-2xx response", func(t *testing.T) {
		obs.PromptWatch = &obs.CacheWatch{}
		codexTestBody = keyedBody(true)
		t.Cleanup(func() { codexTestBody = "" })
		status, _, _, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
		}), time.Minute)
		if status != fiber.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
		assertFinished(t)
	})

	t.Run("successful non-stream", func(t *testing.T) {
		obs.PromptWatch = &obs.CacheWatch{}
		codexTestBody = keyedBody(false)
		t.Cleanup(func() { codexTestBody = "" })
		status, _, _, _ := codexCall(t, backend, time.Minute)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		assertFinished(t)
	})

	t.Run("successful stream", func(t *testing.T) {
		obs.PromptWatch = &obs.CacheWatch{}
		codexTestBody = keyedBody(true)
		t.Cleanup(func() { codexTestBody = "" })
		status, _, _, _ := codexCall(t, backend, time.Minute)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		assertFinished(t)
	})
}

func TestCodexHTTPWithoutUserIDOmitsSessionCacheKey(t *testing.T) {
	var upgrades, posts atomic.Int32
	var mu sync.Mutex
	var headers http.Header
	var request map[string]json.RawMessage
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			upgrades.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		posts.Add(1)
		mu.Lock()
		headers = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &request)
		mu.Unlock()
		okStream(w)
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	status, body := h.post(t, `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"no cache key"}]}`)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q", status, body)
	}
	mu.Lock()
	gotHeaders, gotRequest := headers.Clone(), request
	mu.Unlock()
	if gotHeaders.Get("session-id") != "" || upgrades.Load() != 0 || posts.Load() != 1 {
		t.Errorf("HTTP session-id=%q websocket upgrades=%d HTTP posts=%d; want omitted, 0 and 1", gotHeaders.Get("session-id"), upgrades.Load(), posts.Load())
	}
	if _, ok := gotRequest["prompt_cache_key"]; ok {
		t.Errorf("HTTP request contains an invented prompt_cache_key: %s", gotRequest["prompt_cache_key"])
	}
}

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
