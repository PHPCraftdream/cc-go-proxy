package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

// DefaultBaseURL is the ChatGPT Codex backend (Responses API at /responses).
const DefaultBaseURL = "https://chatgpt.com/backend-api/codex"

const originator = "codex_cli_rs"

// HTTPClient has no overall timeout: responses stream for minutes.
var HTTPClient = &http.Client{}

// Call POSTs req to baseURL/responses with OAuth headers. On 401 it forces a
// token refresh and retries once. Caller closes the body.
func Call(ctx context.Context, baseURL string, s *oauth.Session, req *Request) (*http.Response, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	tok, err := s.Token(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := post(ctx, strings.TrimRight(baseURL, "/")+"/responses", body, tok)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if tok, err = s.ForceRefresh(ctx, tok.AccessToken); err != nil {
		return nil, err
	}
	return post(ctx, strings.TrimRight(baseURL, "/")+"/responses", body, tok)
}

func post(ctx context.Context, url string, body []byte, tok *oauth.Tokens) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if tok.AccountID != "" {
		r.Header.Set("ChatGPT-Account-ID", tok.AccountID)
	}
	r.Header.Set("originator", originator)
	r.Header.Set("User-Agent", originator+" (ccgp)")
	return HTTPClient.Do(r)
}

// ErrorType maps an upstream HTTP status to an Anthropic error type.
func ErrorType(status int) string {
	switch {
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status >= 500:
		return "api_error"
	}
	return "api_error"
}

// ErrorMessage extracts a message from an upstream error body.
func ErrorMessage(body []byte) string {
	var v struct {
		Error any    `json:"error"`
		Msg   string `json:"message"`
		Det   string `json:"detail"`
	}
	if json.Unmarshal(body, &v) == nil {
		switch e := v.Error.(type) {
		case string:
			return e
		case map[string]any:
			if m, ok := e["message"].(string); ok && m != "" {
				return m
			}
			if t, ok := e["type"].(string); ok {
				return t
			}
		}
		if v.Msg != "" {
			return v.Msg
		}
		if v.Det != "" {
			return v.Det
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
