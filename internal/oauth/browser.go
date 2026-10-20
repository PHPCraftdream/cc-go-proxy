package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// CallbackPort is fixed: the Codex OAuth client only allows this redirect.
	CallbackPort = "1455"

	browserScope   = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	browserTimeout = 10 * time.Minute
)

// ListenFunc binds the loopback callback listener (overridable in tests).
type ListenFunc func(port string) ([]net.Listener, error)

// BrowserLogin runs the OAuth authorization-code + PKCE flow with a loopback
// callback on localhost:1455. onURL receives the authorization URL; nothing is
// opened automatically, the caller decides how to show it.
func (c *Client) BrowserLogin(ctx context.Context, onURL func(authURL string)) (*Tokens, error) {
	return c.browserLogin(ctx, CallbackPort, listenLoopback, onURL)
}

func (c *Client) browserLogin(ctx context.Context, port string, listen ListenFunc, onURL func(string)) (*Tokens, error) {
	ctx, cancel := context.WithTimeout(ctx, browserTimeout)
	defer cancel()

	listeners, err := listen(port)
	if err != nil {
		return nil, fmt.Errorf("oauth: cannot listen for the OAuth callback on localhost:%s "+
			"(is another login, e.g. Codex CLI, running?): %w", port, err)
	}
	// The kernel may have chosen the port (tests): use the real one.
	port = portOf(listeners[0])
	redirectURI := "http://localhost:" + port + "/auth/callback"

	verifier, err := randomURLSafe(32)
	if err != nil {
		return nil, err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	result := make(chan callbackResult, 1)
	srv := &http.Server{
		Handler:           callbackHandler(state, port, result),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	for _, l := range listeners {
		go func(l net.Listener) { _ = srv.Serve(l) }(l)
	}
	defer func() { _ = srv.Close() }()

	onURL(c.authorizeURL(redirectURI, state, challenge))

	var res callbackResult
	select {
	case res = <-result:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("oauth: browser authorization timed out")
		}
		return nil, ctx.Err()
	}
	if res.err != nil {
		return nil, res.err
	}
	return c.exchangeCode(ctx, res.code, verifier, redirectURI)
}

func (c *Client) authorizeURL(redirectURI, state, challenge string) string {
	q := url.Values{
		"response_type":              {"code"},
		"client_id":                  {ClientID},
		"redirect_uri":               {redirectURI},
		"scope":                      {browserScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {originator},
	}
	return c.issuer() + "/oauth/authorize?" + q.Encode()
}

// originator identifies this client to the authorization server.
const originator = "codex_cli_rs"

type callbackResult struct {
	code string
	err  error
}

func callbackHandler(wantState, port string, out chan<- callbackResult) http.Handler {
	var once sync.Once
	send := func(r callbackResult) {
		once.Do(func() { out <- r })
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/callback" {
			http.NotFound(w, r)
			return
		}
		host := strings.ToLower(r.Host)
		if r.Method != http.MethodGet || (host != "localhost:"+port && host != "127.0.0.1:"+port) || !isLoopback(r.RemoteAddr) {
			http.Error(w, "invalid OAuth callback request", http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		got := q.Get("state")
		if len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(got), []byte(wantState)) != 1 {
			http.Error(w, "OAuth state did not match", http.StatusBadRequest)
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization was denied", http.StatusBadRequest)
			send(callbackResult{err: fmt.Errorf("oauth: authorization denied (%s)", e)})
			return
		}
		code := q.Get("code")
		if len(q["code"]) != 1 || code == "" {
			http.Error(w, "callback has no authorization code", http.StatusBadRequest)
			send(callbackResult{err: errors.New("oauth: callback has no authorization code")})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Authentication complete. You may close this window.")
		send(callbackResult{code: code})
	})
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func portOf(l net.Listener) string {
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

// listenLoopback binds 127.0.0.1:port and, when possible, [::1]:port on the
// same port: browsers resolve "localhost" to either.
func listenLoopback(port string) ([]net.Listener, error) {
	v4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, err
	}
	out := []net.Listener{v4}
	if v6, err := net.Listen("tcp6", net.JoinHostPort("::1", portOf(v4))); err == nil {
		out = append(out, v6)
	}
	return out, nil
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
