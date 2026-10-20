package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func fakeJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(b) + ".sig"
}

func idToken(account string) string {
	return fakeJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account},
	})
}

func accessToken(exp time.Time) string {
	return fakeJWT(map[string]any{"exp": exp.Unix()})
}

func TestDeviceLogin(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["client_id"] != ClientID {
			t.Errorf("client_id = %q", req["client_id"])
		}
		w.Write([]byte(`{"device_auth_id":"dev-1","user_code":"ABCD-EFGH","interval":"0"}`))
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) < 2 {
			w.WriteHeader(http.StatusForbidden) // pending
			return
		}
		w.Write([]byte(`{"authorization_code":"code-1","code_challenge":"ch","code_verifier":"ver-1"}`))
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		want := url.Values{
			"grant_type": {"authorization_code"}, "client_id": {ClientID}, "code": {"code-1"},
			"code_verifier": {"ver-1"},
		}
		for k, v := range want {
			if r.PostForm.Get(k) != v[0] {
				t.Errorf("form %s = %q, want %q", k, r.PostForm.Get(k), v[0])
			}
		}
		json.NewEncoder(w).Encode(map[string]string{
			"id_token": idToken("acc-1"), "access_token": "at-1", "refresh_token": "rt-1",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{Issuer: srv.URL}
	dc, err := c.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode != "ABCD-EFGH" || dc.VerificationURL != srv.URL+"/codex/device" {
		t.Errorf("device code = %+v", dc)
	}
	dc.interval = time.Millisecond
	tok, err := c.CompleteDeviceLogin(context.Background(), dc)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" || tok.AccountID != "acc-1" {
		t.Errorf("tokens = %+v", tok)
	}
}

func TestRefreshRotation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["grant_type"] != "refresh_token" || req["refresh_token"] != "rt-old" {
			t.Errorf("refresh req = %v", req)
		}
		w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new"}`))
	}))
	defer srv.Close()

	c := &Client{Issuer: srv.URL}
	next, err := c.Refresh(context.Background(), &Tokens{AccessToken: "at-old", RefreshToken: "rt-old", AccountID: "acc"})
	if err != nil {
		t.Fatal(err)
	}
	if next.AccessToken != "at-new" || next.RefreshToken != "rt-new" || next.AccountID != "acc" {
		t.Errorf("next = %+v", next)
	}
}

func TestRefreshPermanentErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		perm   bool
	}{
		{"reused", 400, `{"error":{"code":"refresh_token_reused"}}`, true},
		{"expired", 401, `{"error":"refresh_token_expired"}`, true},
		{"invalid_grant", 400, `{"error":"invalid_grant"}`, true},
		{"server error", 503, `{}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			_, err := (&Client{Issuer: srv.URL}).Refresh(context.Background(), &Tokens{RefreshToken: "rt"})
			if got := errors.Is(err, ErrReloginRequired); got != tt.perm {
				t.Errorf("permanent = %v, want %v (err %v)", got, tt.perm, err)
			}
		})
	}
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	fresh := &Tokens{AccessToken: accessToken(now.Add(time.Hour)), LastRefresh: now}
	if NeedsRefresh(fresh, now) {
		t.Error("fresh token needs refresh")
	}
	expiring := &Tokens{AccessToken: accessToken(now.Add(time.Minute)), LastRefresh: now}
	if !NeedsRefresh(expiring, now) {
		t.Error("expiring token does not need refresh")
	}
	old := &Tokens{AccessToken: "opaque", LastRefresh: now.Add(-9 * 24 * time.Hour)}
	if !NeedsRefresh(old, now) {
		t.Error("old token does not need refresh")
	}
}

func TestSessionRefreshPersists(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": accessToken(time.Now().Add(time.Hour)), "refresh_token": "rt-2",
		})
	}))
	defer srv.Close()

	store := Store{Path: filepath.Join(t.TempDir(), "codex.json")}
	if err := store.Save(&Tokens{AccessToken: accessToken(time.Now()), RefreshToken: "rt-1", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := &Session{Store: store, Client: &Client{Issuer: srv.URL}}

	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "rt-2" {
		t.Errorf("refresh token = %q, want rt-2", tok.RefreshToken)
	}
	saved, err := store.Load()
	if err != nil || saved.RefreshToken != "rt-2" {
		t.Fatalf("persisted = %+v, %v", saved, err)
	}

	// Fresh now: no second refresh; stale ForceRefresh is skipped.
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForceRefresh(context.Background(), "not-current"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Errorf("refresh calls = %d, want 1", calls.Load())
	}
}

func TestSessionNotLoggedIn(t *testing.T) {
	s := &Session{Store: Store{Path: filepath.Join(t.TempDir(), "absent.json")}, Client: &Client{}}
	if _, err := s.Token(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("err = %v, want ErrNotLoggedIn", err)
	}
}
