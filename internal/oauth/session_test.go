package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent requests with an expiring token share one refresh: the old refresh
// token dies on rotation, so a second refresh would log the user out.
func TestSessionConcurrentTokenRefreshesOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": accessToken(time.Now().Add(time.Hour)), "refresh_token": "rt-2",
		})
	}))
	defer srv.Close()

	store := Store{Path: filepath.Join(t.TempDir(), "codex.json")}
	if err := store.Save(&Tokens{AccessToken: accessToken(time.Now()), RefreshToken: "rt-1", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := &Session{Store: store, Client: &Client{Issuer: srv.URL}}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := s.Token(context.Background())
			if err != nil || tok.RefreshToken != "rt-2" {
				t.Errorf("token = %+v, err %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("refresh calls = %d, want 1", calls.Load())
	}
}

// A failed refresh must not touch the stored tokens.
func TestSessionFailedRefreshKeepsStoredTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := Store{Path: filepath.Join(t.TempDir(), "codex.json")}
	if err := store.Save(&Tokens{AccessToken: accessToken(time.Now()), RefreshToken: "rt-1", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := &Session{Store: store, Client: &Client{Issuer: srv.URL}}
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("refresh against a failing issuer succeeded")
	}
	saved, err := store.Load()
	if err != nil || saved.RefreshToken != "rt-1" {
		t.Errorf("stored = %+v, %v; want rt-1 untouched", saved, err)
	}
}

// `ccgp login` rewrites the file while the server runs; the session picks it up.
func TestSessionPicksUpNewLogin(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "codex.json")}
	fresh := func(refresh string) *Tokens {
		return &Tokens{AccessToken: accessToken(time.Now().Add(time.Hour)), RefreshToken: refresh, LastRefresh: time.Now()}
	}
	if err := store.Save(fresh("rt-1")); err != nil {
		t.Fatal(err)
	}
	s := &Session{Store: store, Client: &Client{}}
	if tok, err := s.Token(context.Background()); err != nil || tok.RefreshToken != "rt-1" {
		t.Fatalf("first = %+v, %v", tok, err)
	}

	if err := store.Save(fresh("rt-2")); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute) // distinct mtime even on coarse clocks
	if err := os.Chtimes(store.Path, later, later); err != nil {
		t.Fatal(err)
	}
	if tok, err := s.Token(context.Background()); err != nil || tok.RefreshToken != "rt-2" {
		t.Errorf("after login = %+v, %v; want rt-2", tok, err)
	}
}

// The token file of an earlier version was JSON: it is converted when first used.
func TestStoreMigratesLegacyJSON(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "oauth-codex.ktav")}
	old := filepath.Join(filepath.Dir(store.Path), "oauth-codex.json")
	refreshed := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	legacy := `{"id_token":"","access_token":"` + accessToken(time.Now().Add(time.Hour)) + `","refresh_token":"rt-old","account_id":"acc","last_refresh":"` + refreshed.Format(time.RFC3339) + `"}`
	if err := os.WriteFile(old, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	// Session.Token starts from a missing .ktav, as the running server does.
	s := &Session{Store: store, Client: &Client{}}
	tok, err := s.Token(context.Background())
	if err != nil || tok.RefreshToken != "rt-old" || tok.AccountID != "acc" {
		t.Fatalf("token = %+v, %v", tok, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("legacy file kept: %v", err)
	}
	saved, err := store.Load()
	if err != nil || saved.RefreshToken != "rt-old" || !saved.LastRefresh.Equal(refreshed) {
		t.Errorf("stored = %+v, %v", saved, err)
	}
}

func TestStoreRoundTripWithEmptyIDToken(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "o.ktav")}
	in := &Tokens{AccessToken: "a.b.c", RefreshToken: "rt", LastRefresh: time.Now()}
	if err := store.Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := store.Load()
	if err != nil || out.AccessToken != "a.b.c" || out.RefreshToken != "rt" || out.IDToken != "" || out.AccountID != "" || !out.LastRefresh.Equal(in.LastRefresh) {
		t.Errorf("loaded = %+v, %v", out, err)
	}
}
