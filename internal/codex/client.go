package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

// DefaultBaseURL is the ChatGPT Codex backend (Responses API at /responses).
const DefaultBaseURL = "https://chatgpt.com/backend-api/codex"

const originator = "codex_cli_rs"

// ResponseHeaderTimeout bounds the wait for the backend's reply headers. A
// request the backend stalls on fails after this instead of minutes later, and
// Claude Code retries it.
const ResponseHeaderTimeout = 90 * time.Second

// HTTPClient has no overall timeout: responses stream for minutes. Its
// connections are spread over lanes and renewed (connpool).
var HTTPClient = newClient(ResponseHeaderTimeout)

func newClient(headerTimeout time.Duration) *http.Client {
	return &http.Client{Transport: connpool.NewRouter(0, 0, 0, func() *http.Transport {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = headerTimeout
		return t
	})}
}

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
	url := strings.TrimRight(baseURL, "/") + "/responses"
	resp, err := send(ctx, url, body, tok)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if tok, err = s.ForceRefresh(ctx, tok.AccessToken); err != nil {
		return nil, err
	}
	return send(ctx, url, body, tok)
}

// post sends body once; encoding is its Content-Encoding ("" for none).
func post(ctx context.Context, url string, body []byte, encoding string, tok *oauth.Tokens) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	if encoding != "" {
		r.Header.Set("Content-Encoding", encoding)
	}
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
	if m, ok := htmlMessage(s); ok {
		return m
	}
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

var (
	htmlTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	htmlSkip  = regexp.MustCompile(`(?is)<(script|style|head)[^>]*>.*?</(script|style|head)>`)
	htmlTag   = regexp.MustCompile(`(?s)<[^>]*>`)
)

// htmlTextAt is how many runes of page text are shown when the page has no title.
const htmlTextAt = 150

// htmlIP is the blocked address an edge's block page ends with: "[IP: 203.0.113.7]".
var htmlIP = regexp.MustCompile(`\[IP:\s*[0-9A-Fa-f:.]{2,45}\s*\]`)

// htmlText is the visible text of a page, cut to htmlTextAt runes. A long text
// keeps the address of a block page after the cut: it tells which way out the
// edge turned away.
func htmlText(s string) string {
	text := strings.Join(strings.Fields(htmlTag.ReplaceAllString(htmlSkip.ReplaceAllString(s, " "), " ")), " ")
	if len([]rune(text)) <= htmlTextAt {
		return text
	}
	ip := ""
	if loc := htmlIP.FindStringIndex(text); loc != nil {
		ip = " " + text[loc[0]:loc[1]]
		text = strings.Join(strings.Fields(text[:loc[0]]+" "+text[loc[1]:]), " ")
	}
	if r := []rune(text); len(r) > htmlTextAt {
		text = string(r[:htmlTextAt]) + "…"
	}
	return text + ip
}

// IsHTMLPage reports whether body is an HTML page, not the JSON the backend
// itself answers with: a page comes from the edge in front of it.
func IsHTMLPage(body []byte) bool {
	b := bytes.TrimLeft(body, " \t\r\n\xef\xbb\xbf")
	return len(b) > 0 && b[0] == '<' && looksHTML(string(b[:min(len(b), 200)]))
}

// looksHTML reports whether the start of a body opens an HTML page.
func looksHTML(head string) bool {
	head = strings.ToLower(head)
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

// htmlMessage turns an HTML page (an error page of the edge in front of the
// backend, such as Cloudflare's) into a line: its title, else the start of its
// text, not a screen of markup.
func htmlMessage(s string) (string, bool) {
	if !looksHTML(s[:min(len(s), 200)]) {
		return "", false
	}
	if m := htmlTitle.FindStringSubmatch(s); m != nil {
		if title := strings.Join(strings.Fields(m[1]), " "); title != "" {
			return "html error page: " + title, true
		}
	}
	if text := htmlText(s); text != "" {
		return "html error page: " + text, true
	}
	return "html error page", true
}
