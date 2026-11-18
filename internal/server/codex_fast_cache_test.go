package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

func resetFastTiers(t *testing.T) {
	t.Helper()
	reset := func() {
		fastTiers.Lock()
		fastTiers.entries = make(map[[32]byte]fastTierEntry)
		fastTiers.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func fastTierCount() int {
	fastTiers.Lock()
	defer fastTiers.Unlock()
	return len(fastTiers.entries)
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
		tier, status, err := fastTier(ctx, cfg, srv.URL, model, s)
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
	fastTiers.Lock()
	for k, v := range fastTiers.entries {
		v.expires = time.Now().Add(-time.Second)
		fastTiers.entries[k] = v
	}
	fastTiers.Unlock()
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
	if tier, status, err := fastTier(ctx, cfg, srv.URL, "gpt-6.2-sol", s); err == nil || status != 502 || tier != "" || !strings.Contains(err.Error(), "Fast catalog lookup failed") {
		t.Fatalf("catalog failure: %q, %d, %v", tier, status, err)
	}
	if fastTierCount() != 0 {
		t.Fatal("a failed lookup was cached")
	}
	fail.Store(false)
	if tier, status, err := fastTier(ctx, cfg, srv.URL, "gpt-6.2-sol", s); err != nil || status != 0 || tier != "sol-fast" {
		t.Fatalf("after recovery: %q, %d, %v", tier, status, err)
	}

	// A model without the tier is a 400 every time, never a cached success.
	for i := 0; i < 2; i++ {
		before := calls.Load()
		if _, status, err := fastTier(ctx, cfg, srv.URL, "gpt-6.2-terra", s); err == nil || status != 400 || !strings.Contains(err.Error(), "does not advertise") {
			t.Fatalf("no tier: %d, %v", status, err)
		}
		if calls.Load() == before {
			t.Fatal("a refusal was served from the cache")
		}
	}
	if _, status, err := fastTier(ctx, cfg, srv.URL, "gpt-9-absent", s); err == nil || status != 400 || !strings.Contains(err.Error(), "absent from the account catalog") {
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
	if _, status, err := fastTier(context.Background(), cfg, srv.URL, "gpt-6.2-sol", s); err == nil || status != 401 {
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
	fastTiers.Lock()
	for i := 0; i < fastCatalogMaxEntries; i++ {
		fastTiers.entries[[32]byte{byte(i), 1}] = fastTierEntry{tier: "old", expires: now.Add(time.Hour + time.Duration(i)*time.Minute)}
	}
	fastTiers.Unlock()

	if _, _, err := fastTier(context.Background(), cfg, srv.URL, "gpt-6.2-sol", s); err != nil {
		t.Fatal(err)
	}
	fastTiers.Lock()
	defer fastTiers.Unlock()
	if len(fastTiers.entries) != fastCatalogMaxEntries {
		t.Fatalf("entries = %d, want %d", len(fastTiers.entries), fastCatalogMaxEntries)
	}
	if _, ok := fastTiers.entries[[32]byte{0, 1}]; ok {
		t.Error("the entry closest to expiry was kept")
	}
	if _, ok := fastTiers.entries[[32]byte{1, 1}]; !ok {
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
