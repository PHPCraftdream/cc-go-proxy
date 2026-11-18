package codexh

import (
	"bytes"
	"context"
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
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/tier"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
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

func TestDropShownMessage(t *testing.T) {
	for _, tt := range []struct {
		name, detail, msg, want string
		dropped                 bool
	}{
		{"error event",
			`{"type":"error","error":{"code":"cyber_policy","message":"flagged"}}`, "cyber_policy: flagged",
			`{"type":"error","error":{"code":"cyber_policy","message":"(shown before)"}}`, true},
		{"response.failed, spaces",
			`{"type":"response.failed","response":{"error":{"message" : "boom ’x\""}}}`, "server_error: boom ’x\"",
			`{"type":"response.failed","response":{"error":{"message" : "(shown before)"}}}`, true},
		{"another message is kept",
			`{"error":{"message":"other"}}`, "boom", `{"error":{"message":"other"}}`, false},
		{"a value named message is kept",
			`{"type":"message","error":{"message":"boom"}}`, "boom", `{"type":"message","error":{"message":"(shown before)"}}`, true},
		{"empty message is kept",
			`{"error":{"message":""}}`, "boom", `{"error":{"message":""}}`, false},
		{"not a string",
			`{"message":null,"error":{"message":"boom"}}`, "boom", `{"message":null,"error":{"message":"(shown before)"}}`, true},
		{"broken json", `{"message":"bo`, "boom", `{"message":"bo`, false},
		{"found inside the reason is another text", `{"error":{"message":"a"}}`, "bad", `{"error":{"message":"a"}}`, false},
		{"the end of a word is not the message", `{"error":{"message":"boom"}}`, "kaboom", `{"error":{"message":"boom"}}`, false},
		{"no detail", "", "boom", "", false},
	} {
		got, dropped := shared.DropShownMessage(tt.detail, tt.msg)
		if got != tt.want || dropped != tt.dropped {
			t.Errorf("%s: got %q, %v; want %q, %v", tt.name, got, dropped, tt.want, tt.dropped)
		}
	}
}

// The live cyber_policy refusal: its long message is written once, whole.
func TestStreamFailureWritesARepeatedMessageOnce(t *testing.T) {
	text := "This content was flagged for possible cybersecurity risk. " + strings.Repeat("If this seems wrong, try rephrasing your request. ", 8) + "END"
	ev := `{"type":"error","error":{"type":"invalid_request_error","code":"cyber_policy","message":"` + text + `","param":null}}`
	var b bytes.Buffer
	shared.LogStreamFailure(&b, time.Now(), "stream", "codex", "a -> b", "cyber_policy: "+text, ev, "cf-ray=1")
	got := b.String()
	if n := strings.Count(got, "This content was flagged"); n != 1 {
		t.Errorf("message written %d times: %q", n, got)
	}
	want := ": cyber_policy: " + text + ` | upstream event: {"type":"error","error":{"type":"invalid_request_error","code":"cyber_policy","message":"(shown before)","param":null}} | cf-ray=1` + "\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("line %q\nwant suffix %q", got, want)
	}
}

func TestUpstreamRetryDropsTheShownMessage(t *testing.T) {
	var b bytes.Buffer
	rt.LogUpstreamRetry(&b, "codex", "a -> b", 1, 2, 2*time.Second, "server_is_overloaded: overloaded",
		`{"type":"error","error":{"code":"server_is_overloaded","message":"overloaded"}}`, "cf-ray=1")
	want := `server_is_overloaded: overloaded | upstream event: {"type":"error","error":{"code":"server_is_overloaded","message":"(shown before)"}} | cf-ray=1` + "\n"
	if got := b.String(); !strings.HasSuffix(got, want) {
		t.Errorf("line %q\nwant suffix %q", got, want)
	}
}

// Without a repeat the reason keeps its usual cap.
func TestEventTextCapsAReasonNotRepeated(t *testing.T) {
	long := strings.Repeat("x", shared.MaxLogErr+50)
	if got := shared.EventText(long, `{"error":{"message":"other"}}`); !strings.HasPrefix(got, strings.Repeat("x", shared.MaxLogErr)+"… | upstream event: ") {
		t.Errorf("got %q", got)
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

// syncBuf is a log sink the stream-writer goroutine and the test share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// codexCall runs one streaming /v1/messages request through handleCodexMessages
// against backend and returns the status, body, access log and stream log.
// codexTestBody replaces the request the helpers send (default: a streaming one).
var codexTestBody string

func nonStreamBody(t *testing.T) {
	t.Helper()
	codexTestBody = `{"model":"claude-opus-5-5","max_tokens":10,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	t.Cleanup(func() { codexTestBody = "" })
}

func codexCall(t *testing.T, backend http.Handler, headerTimeout time.Duration) (status int, body, access, stream string) {
	t.Helper()
	return codexCallWith(t, backend, &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: headerTimeout}}, nil)
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

func TestStalledBackendIs502WithReasonInLog(t *testing.T) {
	status, _, access, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the client hanging up; without
		// it the handler, and so httptest.Server.Close, would block forever.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}), 100*time.Millisecond)
	if status != fiber.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if !strings.Contains(access, " 502 - ") || !strings.Contains(access, "timeout awaiting response headers") {
		t.Errorf("access log = %q", access)
	}
	// Which connection it waited on tells a dead one from a bad route.
	if !strings.Contains(access, " | conn new (dns ") {
		t.Errorf("access log lacks the connection: %q", access)
	}
}

func TestUpstreamErrorMessageIsLogged(t *testing.T) {
	status, _, access, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"usage limit reached"}}`))
	}), time.Minute)
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if !strings.Contains(access, " 429 - ") || !strings.Contains(access, ": codex: usage limit reached") {
		t.Errorf("access log = %q", access)
	}
}

// A refusal by the edge (a 403 page) names the ids and the verdict of the edge
// and shows what the page says, so a challenge can be told from a block.
func TestEdgeRefusalLogsIdsAndPageText(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, access, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cf-Ray", "abc123-PIT")
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><body><script>var a=1;</script><h1>Enable JavaScript</h1></body></html>`))
	}), time.Minute)
	if status != fiber.StatusForbidden || !strings.Contains(body, "html error page: Enable JavaScript") {
		t.Fatalf("status %d, body %q", status, body)
	}
	for _, want := range []string{" 403 - ", "html error page: Enable JavaScript | cf-ray=abc123-PIT cf-mitigated=challenge | conn "} {
		if !strings.Contains(access, want) {
			t.Errorf("access log lacks %q: %q", want, access)
		}
	}
	if strings.Contains(access, "var a") {
		t.Errorf("a script reached the log: %q", access)
	}
	// A page is the edge's, not the backend's: asked again, twice, then given up.
	if calls.Load() != 3 {
		t.Errorf("backend calls = %d, want 3 (the request and two retries)", calls.Load())
	}
	for _, want := range []string{"upstream retry 1/2", "upstream retry 2/2", "HTTP 403: html error page: Enable JavaScript", "cf-ray=abc123-PIT cf-mitigated=challenge | conn new"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream log lacks %q: %q", want, stream)
		}
	}
}

// A page the edge turned the connection away with is asked again; the address
// the page names reaches the log whole, and the retry gets a fresh connection.
func TestBlockedRouteIsAskedAgainOnAFreshConnection(t *testing.T) {
	fastRetries(t, 2)
	var calls, dials atomic.Int32
	ok := overloadBackend(new(atomic.Int32), 0)
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Cf-Ray", "ray-hel")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<html><body><p>Unable to load site Please try again later. If you are using a VPN, try turning it off. `+
				`Check the status page for information on outages. [IP: 203.0.113.7]</p></body></html>`)
			return
		}
		ok.ServeHTTP(w, r)
	})
	router := connpool.NewRouter(0, 1, 1, func() *http.Transport {
		return &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
	})
	status, body, _, stream := codexCallWith(t, backend, &http.Client{Transport: router}, nil)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q: want the answer of the second call", status, body)
	}
	if calls.Load() != 2 {
		t.Errorf("backend calls = %d, want 2", calls.Load())
	}
	// The block was met on the first connection; the retry must not reuse it.
	if dials.Load() != 2 {
		t.Errorf("connections opened = %d, want 2: the retry needs a new one", dials.Load())
	}
	for _, want := range []string{"upstream retry 1/2", "[IP: 203.0.113.7]", "cf-ray=ray-hel | conn new"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream log lacks %q: %q", want, stream)
		}
	}
}

// The backend's own 403 is JSON: it is the client's to see, not to be asked again.
func TestBackendForbiddenIsNotAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, access, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"not allowed for this account"}}`)
	}), time.Minute)
	if status != fiber.StatusForbidden || !strings.Contains(body, "not allowed for this account") {
		t.Fatalf("status %d, body %q: want the backend's 403", status, body)
	}
	if calls.Load() != 1 || stream != "" {
		t.Errorf("backend calls = %d, log %q: a JSON 403 must not be retried", calls.Load(), stream)
	}
	if !strings.Contains(access, " 403 - ") || !strings.Contains(access, "codex: not allowed for this account") {
		t.Errorf("access log = %q", access)
	}
}

// A gateway error is still asked again, and the failed line says which connection it was on.
func TestGatewayErrorLineNamesTheConnection(t *testing.T) {
	fastRetries(t, 1)
	status, _, access, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "bad gateway")
	}), time.Minute)
	if status != fiber.StatusBadGateway {
		t.Fatalf("status %d, want 502", status)
	}
	if !strings.Contains(stream, "upstream retry 1/1") || !strings.Contains(access, " 502 - ") || !strings.Contains(access, "| conn ") {
		t.Errorf("stream %q, access %q", stream, access)
	}
}
