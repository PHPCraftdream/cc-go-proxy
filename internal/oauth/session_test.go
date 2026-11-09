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

// `ccgp --login` rewrites the file while the server runs; the session picks it up.
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

// Independent sessions must reload and serialize refresh-token rotation through
// the file lock, for both normal and forced refresh paths.
func TestIndependentSessionsRefreshOnce(t *testing.T) {
	for _, mode := range []string{"token", "force-refresh"} {
		t.Run(mode, func(t *testing.T) {
			store := Store{Path: filepath.Join(t.TempDir(), "codex.json")}
			initial := &Tokens{AccessToken: accessToken(time.Now()), RefreshToken: "rt-1", LastRefresh: time.Now()}
			if err := store.Save(initial); err != nil {
				t.Fatal(err)
			}

			var calls atomic.Int32
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{
					"access_token": accessToken(time.Now().Add(time.Hour)), "refresh_token": "rt-2",
				})
			}))
			defer srv.Close()

			sessions := []*Session{
				{Store: store, Client: &Client{Issuer: srv.URL}, tokens: initial},
				{Store: store, Client: &Client{Issuer: srv.URL}, tokens: initial},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := make(chan struct{}, len(sessions))
			type result struct {
				token *Tokens
				err   error
			}
			results := make(chan result, len(sessions))
			for _, session := range sessions {
				session := session
				go func() {
					started <- struct{}{}
					var tok *Tokens
					var err error
					if mode == "token" {
						tok, err = session.Token(ctx)
					} else {
						tok, err = session.ForceRefresh(ctx, initial.AccessToken)
					}
					results <- result{token: tok, err: err}
				}()
			}
			for range sessions {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("sessions did not start before deadline")
				}
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("refresh did not reach issuer before deadline")
			}
			// Keep the first exchange in flight while a second session contends.
			// A correctly locked session cannot issue a second request.
			select {
			case <-entered:
			case <-time.After(100 * time.Millisecond):
			}
			close(release)
			for range sessions {
				select {
				case got := <-results:
					if got.err != nil || got.token == nil || got.token.RefreshToken != "rt-2" {
						t.Errorf("result = %+v, %v; want rotated rt-2", got.token, got.err)
					}
				case <-ctx.Done():
					t.Fatal("refresh callers did not finish before deadline")
				}
			}
			if calls.Load() != 1 {
				t.Errorf("refresh calls = %d, want 1", calls.Load())
			}
			saved, err := store.Load()
			if err != nil || saved.RefreshToken != "rt-2" {
				t.Errorf("disk rotation = %+v, %v; want rt-2", saved, err)
			}
		})
	}
}

func TestSessionReloadsWhenFileMtimeIsUnchanged(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "codex.json")}
	fresh := func(refresh string) *Tokens {
		return &Tokens{AccessToken: accessToken(time.Now().Add(time.Hour)), RefreshToken: refresh, LastRefresh: time.Now()}
	}
	if err := store.Save(fresh("rt-1")); err != nil {
		t.Fatal(err)
	}
	s := &Session{Store: store, Client: &Client{}}
	if tok, err := s.Token(context.Background()); err != nil || tok.RefreshToken != "rt-1" {
		t.Fatalf("initial token = %+v, %v", tok, err)
	}
	st, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(fresh("rt-2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(store.Path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	stAfter, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !stAfter.ModTime().Equal(st.ModTime()) {
		t.Fatalf("test setup changed mtime: before %v, after %v", st.ModTime(), stAfter.ModTime())
	}
	if tok, err := s.Token(context.Background()); err != nil || tok.RefreshToken != "rt-2" {
		t.Errorf("after same-mtime rewrite = %+v, %v; want rt-2", tok, err)
	}
}

func TestSessionDoesNotReturnOrInstallRotationWhenSaveFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.json")
	store := Store{Path: path}
	initial := &Tokens{AccessToken: accessToken(time.Now()), RefreshToken: "rt-1", LastRefresh: time.Now()}
	if err := store.Save(initial); err != nil {
		t.Fatal(err)
	}
	setupErr := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.Remove(path); err != nil {
			setupErr <- err
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			setupErr <- err
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		setupErr <- nil
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": accessToken(time.Now().Add(time.Hour)), "refresh_token": "rt-2",
		})
	}))
	defer srv.Close()
	s := &Session{Store: store, Client: &Client{Issuer: srv.URL}}
	tok, err := s.Token(context.Background())
	if setup := <-setupErr; setup != nil {
		t.Fatalf("could not make rotated-token save fail: %v", setup)
	}
	if err == nil || tok != nil {
		t.Fatalf("Token() = %+v, %v; want save error and no token", tok, err)
	}
	if s.tokens == nil || s.tokens.RefreshToken != "rt-1" || s.tokens.AccessToken != initial.AccessToken {
		t.Errorf("cached token after failed save = %+v; want original", s.tokens)
	}
}

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
