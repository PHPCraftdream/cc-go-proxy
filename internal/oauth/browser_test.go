package oauth

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ephemeralListen binds a random loopback port instead of the fixed 1455.
func ephemeralListen(string) ([]net.Listener, error) { return listenLoopback("0") }

func newIssuer(t *testing.T, check func(url.Values)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		if check != nil {
			check(r.PostForm)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  fakeJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acc-b"}}),
			"refresh_token": "rt-b",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, u string) int {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestBrowserLogin(t *testing.T) {
	var form url.Values
	issuer := newIssuer(t, func(v url.Values) { form = v })
	c := &Client{Issuer: issuer.URL}

	var authURL string
	done := make(chan struct{})
	var tok *Tokens
	var err error
	go func() {
		defer close(done)
		tok, err = c.browserLogin(context.Background(), "0", ephemeralListen, func(u string) {
			authURL = u
			parsed, perr := url.Parse(u)
			if perr != nil {
				t.Errorf("auth url: %v", perr)
				return
			}
			q := parsed.Query()
			if parsed.Path != "/oauth/authorize" || q.Get("client_id") != ClientID ||
				q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" ||
				!strings.Contains(q.Get("scope"), "offline_access") {
				t.Errorf("bad authorize url: %s", u)
			}
			cb := q.Get("redirect_uri")
			// Wrong state is rejected and does not end the flow.
			if s := get(t, cb+"?code=evil&state=wrong"); s != http.StatusBadRequest {
				t.Errorf("wrong state status = %d", s)
			}
			if s := get(t, cb+"?code=good-code&state="+url.QueryEscape(q.Get("state"))); s != http.StatusOK {
				t.Errorf("valid callback status = %d", s)
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("login did not finish")
	}
	if err != nil {
		t.Fatal(err)
	}
	if authURL == "" || tok.RefreshToken != "rt-b" || tok.AccountID != "acc-b" {
		t.Fatalf("tokens = %+v", tok)
	}
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "good-code" ||
		form.Get("code_verifier") == "" || !strings.HasPrefix(form.Get("redirect_uri"), "http://localhost:") {
		t.Errorf("exchange form = %v", form)
	}
}

func TestBrowserLoginDenied(t *testing.T) {
	c := &Client{Issuer: newIssuer(t, nil).URL}
	_, err := c.browserLogin(context.Background(), "0", ephemeralListen, func(u string) {
		parsed, _ := url.Parse(u)
		q := parsed.Query()
		get(t, q.Get("redirect_uri")+"?error=access_denied&state="+url.QueryEscape(q.Get("state")))
	})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v, want denied", err)
	}
}

func TestBrowserLoginCancel(t *testing.T) {
	c := &Client{Issuer: newIssuer(t, nil).URL}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := c.browserLogin(ctx, "0", ephemeralListen, func(string) { cancel() })
	if err == nil {
		t.Fatal("cancelled login succeeded")
	}
}

func TestBrowserLoginPortBusy(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, port, _ := net.SplitHostPort(l.Addr().String())

	c := &Client{Issuer: "http://127.0.0.1:1"}
	_, err = c.browserLogin(context.Background(), port, listenLoopback, func(string) {
		t.Error("onURL called although the port is busy")
	})
	if err == nil || !strings.Contains(err.Error(), "callback") {
		t.Fatalf("err = %v", err)
	}
}

func TestCallbackRejectsForeignHost(t *testing.T) {
	out := make(chan callbackResult, 1)
	h := callbackHandler("st", "1455", out)
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state=st", nil)
	r.Host = "evil.example:1455"
	r.RemoteAddr = "127.0.0.1:5555"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || len(out) != 0 {
		t.Errorf("foreign host: code=%d results=%d", w.Code, len(out))
	}
}
