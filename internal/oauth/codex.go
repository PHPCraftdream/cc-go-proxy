// Package oauth implements ChatGPT (Codex) OAuth: headless device-code login,
// token persistence and refresh-token rotation.
//
// Protocol mirrors openai/codex codex-rs/login (device_code_auth.rs, oauth/client.rs,
// auth/manager.rs).
package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	Issuer   = "https://auth.openai.com"
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// Refresh when access token expires within this window, or tokens are older than refreshMaxAge.
	refreshWindow = 5 * time.Minute
	refreshMaxAge = 8 * 24 * time.Hour

	deviceLoginTimeout = 15 * time.Minute
)

// ErrReloginRequired: refresh token expired, reused or revoked.
var ErrReloginRequired = errors.New("oauth: refresh token rejected, run `login` again")

// Tokens is the persisted OAuth state.
type Tokens struct {
	IDToken      string    `json:"id_token,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	AccountID    string    `json:"account_id,omitempty"`
	LastRefresh  time.Time `json:"last_refresh"`
}

// Client talks to the auth issuer. Zero value uses defaults.
type Client struct {
	HTTP   *http.Client
	Issuer string
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) issuer() string {
	if c.Issuer != "" {
		return strings.TrimRight(c.Issuer, "/")
	}
	return Issuer
}

// DeviceCode is shown to the user; they open VerificationURL and enter UserCode.
type DeviceCode struct {
	VerificationURL string
	UserCode        string
	deviceAuthID    string
	interval        time.Duration
}

// flexInt accepts a JSON number or a numeric string.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	*f = flexInt(n)
	return err
}

// RequestDeviceCode starts device-code login. Never opens a browser.
func (c *Client) RequestDeviceCode(ctx context.Context) (*DeviceCode, error) {
	var resp struct {
		DeviceAuthID string  `json:"device_auth_id"`
		UserCode     string  `json:"user_code"`
		UserCodeAlt  string  `json:"usercode"`
		Interval     flexInt `json:"interval"`
	}
	status, err := c.postJSON(ctx, c.issuer()+"/api/accounts/deviceauth/usercode",
		map[string]string{"client_id": ClientID}, &resp)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, errors.New("oauth: device code login is not enabled on the issuer")
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("oauth: device code request failed: %d", status)
	}
	code := resp.UserCode
	if code == "" {
		code = resp.UserCodeAlt
	}
	interval := time.Duration(resp.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &DeviceCode{
		VerificationURL: c.issuer() + "/codex/device",
		UserCode:        code,
		deviceAuthID:    resp.DeviceAuthID,
		interval:        interval,
	}, nil
}

// CompleteDeviceLogin polls until the user approves, then exchanges the code for tokens.
func (c *Client) CompleteDeviceLogin(ctx context.Context, dc *DeviceCode) (*Tokens, error) {
	ctx, cancel := context.WithTimeout(ctx, deviceLoginTimeout)
	defer cancel()

	var code struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	for {
		status, err := c.postJSON(ctx, c.issuer()+"/api/accounts/deviceauth/token",
			map[string]string{"device_auth_id": dc.deviceAuthID, "user_code": dc.UserCode}, &code)
		if err != nil {
			return nil, err
		}
		if status/100 == 2 {
			break
		}
		// 403/404: not approved yet
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return nil, fmt.Errorf("oauth: device auth failed: %d", status)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("oauth: device auth timed out: %w", ctx.Err())
		case <-time.After(dc.interval):
		}
	}

	return c.exchangeCode(ctx, code.AuthorizationCode, code.CodeVerifier, c.issuer()+"/deviceauth/callback")
}

// exchangeCode trades an authorization code for tokens. Single attempt: a
// retried exchange may hit an already consumed code.
func (c *Client) exchangeCode(ctx context.Context, code, verifier, redirectURI string) (*Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	var tok struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	status, body, err := c.do(ctx, c.issuer()+"/oauth/token", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("oauth: code exchange failed: %d", status)
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("oauth: code exchange: bad response")
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return nil, errors.New("oauth: code exchange: missing tokens")
	}
	return &Tokens{
		IDToken:      tok.IDToken,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		AccountID:    accountID(tok.AccessToken, tok.IDToken),
		LastRefresh:  time.Now().UTC(),
	}, nil
}

// Refresh exchanges the refresh token. Rotated tokens replace old ones.
func (c *Client) Refresh(ctx context.Context, t *Tokens) (*Tokens, error) {
	var resp struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        any    `json:"error"`
		Code         string `json:"code"`
	}
	status, body, err := c.do(ctx, c.issuer()+"/oauth/token", "application/json",
		mustJSON(map[string]string{
			"grant_type":    "refresh_token",
			"client_id":     ClientID,
			"refresh_token": t.RefreshToken,
		}))
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(body, &resp)
	if status/100 != 2 {
		if isPermanentRefreshError(status, errorCode(resp.Error, resp.Code)) {
			return nil, ErrReloginRequired
		}
		return nil, fmt.Errorf("oauth: refresh failed: %d", status)
	}
	if resp.AccessToken == "" {
		return nil, errors.New("oauth: refresh: missing access token")
	}
	next := *t
	next.AccessToken = resp.AccessToken
	if resp.IDToken != "" {
		next.IDToken = resp.IDToken
	}
	if id := accountID(resp.AccessToken, resp.IDToken); id != "" {
		next.AccountID = id
	}
	if resp.RefreshToken != "" {
		next.RefreshToken = resp.RefreshToken
	}
	next.LastRefresh = time.Now().UTC()
	return &next, nil
}

// NeedsRefresh reports whether t should be refreshed at now.
func NeedsRefresh(t *Tokens, now time.Time) bool {
	if exp, ok := jwtExpiry(t.AccessToken); ok && now.Add(refreshWindow).After(exp) {
		return true
	}
	return now.Sub(t.LastRefresh) > refreshMaxAge
}

func isPermanentRefreshError(status int, code string) bool {
	switch strings.ToLower(code) {
	case "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		return true
	case "invalid_grant":
		return status == http.StatusBadRequest
	}
	return status == http.StatusUnauthorized
}

// errorCode extracts the code from `error` (string or {code|type}) or top-level `code`.
func errorCode(e any, code string) string {
	switch v := e.(type) {
	case string:
		return v
	case map[string]any:
		for _, k := range []string{"code", "type"} {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
		}
	}
	return code
}

func (c *Client) postJSON(ctx context.Context, u string, in, out any) (int, error) {
	status, body, err := c.do(ctx, u, "application/json", mustJSON(in))
	if err != nil {
		return 0, err
	}
	if status/100 == 2 && out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return 0, fmt.Errorf("oauth: bad response from %s", u)
		}
	}
	return status, nil
}

func (c *Client) do(ctx context.Context, u, contentType string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("oauth: %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

func mustJSON(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// jwtClaims decodes the JWT payload without verification (issuer is trusted over TLS).
func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func jwtExpiry(tok string) (time.Time, bool) {
	exp, ok := jwtClaims(tok)["exp"].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

// accountID returns chatgpt_account_id from the first JWT (access or id token)
// that carries it.
func accountID(tokens ...string) string {
	for _, t := range tokens {
		auth, _ := jwtClaims(t)["https://api.openai.com/auth"].(map[string]any)
		if id, _ := auth["chatgpt_account_id"].(string); id != "" {
			return id
		}
	}
	return ""
}
