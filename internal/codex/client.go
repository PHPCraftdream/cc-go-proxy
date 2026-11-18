package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

// DefaultBaseURL is the ChatGPT Codex backend (Responses API at /responses).
const DefaultBaseURL = "https://chatgpt.com/backend-api/codex"

const Originator = "codex_cli_rs"

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

type clientVersionKey struct{}

// WithClientVersion sets the Codex CLI version presented to the backend
// (config's codex.client_version) on the calls made with ctx.
func WithClientVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, clientVersionKey{}, version)
}

// ClientVersion returns the version set by WithClientVersion, or "".
func ClientVersion(ctx context.Context) string {
	v, _ := ctx.Value(clientVersionKey{}).(string)
	return v
}

// userAgent is what the official client sends (originator/version), marked as ccgp.
func UserAgent(ctx context.Context) string {
	if v := ClientVersion(ctx); v != "" {
		return Originator + "/" + v + " (ccgp)"
	}
	return Originator + " (ccgp)"
}

// Routing carries Codex conversation routing state across calls and retries.
// Its opaque state is sent verbatim and is never interpreted by the client.
type Routing struct {
	mu       sync.Mutex
	threadID string
	state    string
	sent     bool
	returned bool
	onState  func(string)
}

// NewRouting creates routing state for a Codex conversation. onState, when
// non-nil, is called with each nonempty state returned by the backend.
func NewRouting(threadID, state string, onState func(string)) *Routing {
	return &Routing{threadID: threadID, state: state, onState: onState}
}

// WithRouting carries routing state through Call and every underlying POST.
func WithRouting(ctx context.Context, routing *Routing) context.Context {
	return context.WithValue(ctx, routingKey{}, routing)
}

type routingKey struct{}

func RoutingFrom(ctx context.Context) *Routing {
	routing, _ := ctx.Value(routingKey{}).(*Routing)
	return routing
}

// Sent reports whether a nonempty opaque turn state has been sent on a POST.
func (r *Routing) Sent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

// Returned reports whether a response has included the turn-state header.
func (r *Routing) Returned() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.returned
}

func (r *Routing) Outgoing() (threadID, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != "" {
		r.sent = true
	}
	return r.threadID, r.state
}

func (r *Routing) Capture(h http.Header) {
	state, ok := HeaderValueFold(h, "x-codex-turn-state")
	if !ok {
		return
	}
	r.mu.Lock()
	r.state = state
	r.returned = true
	callback := r.onState
	r.mu.Unlock()
	if state != "" && callback != nil {
		callback(state)
	}
}

func HeaderValueFold(h http.Header, name string) (string, bool) {
	for key, values := range h {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0], true
		}
	}
	return "", false
}

// Prepared is an immutable snapshot of a Codex request for reuse across retries.
type Prepared struct {
	body     []byte
	headers  http.Header
	packed   []byte
	packOnce sync.Once
}

func (p *Prepared) compressed() []byte {
	p.packOnce.Do(func() { p.packed = encoder.EncodeAll(p.body, nil) })
	return p.packed
}

// PrepareRequest serializes req and snapshots its session headers for retries.
func PrepareRequest(req *Request) (*Prepared, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return &Prepared{body: body, headers: SessionHeaders(req)}, nil
}

// Call POSTs req to baseURL/responses with OAuth headers. On 401 it forces a
// token refresh and retries once. Caller closes the body.
func Call(ctx context.Context, baseURL string, s *oauth.Session, req *Request) (*http.Response, error) {
	prepared, err := PrepareRequest(req)
	if err != nil {
		return nil, err
	}
	return CallPrepared(ctx, baseURL, s, prepared)
}

// CallPrepared sends a prepared request, reusing its snapshot for OAuth retry.
func CallPrepared(ctx context.Context, baseURL string, s *oauth.Session, p *Prepared) (*http.Response, error) {
	if p == nil {
		return nil, errors.New("codex: nil prepared request")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	tok, err := s.Token(ctx)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(baseURL, "/") + "/responses"
	resp, err := sendPrepared(ctx, url, p, tok)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if tok, err = s.ForceRefresh(ctx, tok.AccessToken); err != nil {
		return nil, err
	}
	return sendPrepared(ctx, url, p, tok)
}

// post sends body once; encoding is its Content-Encoding ("" for none).
func post(ctx context.Context, url string, body []byte, encoding string, hdr http.Header, tok *oauth.Tokens) (*http.Response, error) {
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
	r.Header.Set("originator", Originator)
	r.Header.Set("User-Agent", UserAgent(ctx))
	for k, v := range hdr {
		r.Header[k] = v
	}
	routing := RoutingFrom(ctx)
	if routing != nil {
		threadID, state := routing.Outgoing()
		r.Header.Set("thread-id", threadID)
		r.Header.Set("x-client-request-id", threadID)
		if state != "" {
			r.Header.Set("x-codex-turn-state", state)
		}
	}
	resp, err := HTTPClient.Do(r)
	if resp != nil && resp.Request != nil {
		resp.Request = resp.Request.Clone(ctx)
		resp.Request.Body = http.NoBody
		resp.Request.GetBody = nil
	}
	if routing != nil && resp != nil {
		routing.Capture(resp.Header)
	}
	return resp, err
}

// sessionHeaders are what the official Codex client sends to tie a request to
// its conversation: the session id (here the prompt cache key, which is stable
// per Claude Code session) and a routing hint naming the model. The backend may
// route and cache by them; without them every request looked like a new client.
func SessionHeaders(req *Request) http.Header {
	h := http.Header{}
	if req.PromptCacheKey != "" {
		h.Set("session-id", req.PromptCacheKey)
	}
	if req.Model != "" {
		h.Set("x-codex-routing-hint", "model="+req.Model)
	}
	return h
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

// UsageLimitCode is the error of a used-up plan limit. It resets in hours or
// days: the same request sent again only uploads it once more.
const UsageLimitCode = "usage_limit_reached"

// IsUsageLimit reports whether an upstream error body is a used-up plan limit.
func IsUsageLimit(body []byte) bool {
	var v struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &v) == nil && (v.Error.Type == UsageLimitCode || v.Error.Code == UsageLimitCode)
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

var (
	// What the backend says when a model needs a newer client than the one
	// presented ("The 'X' model requires a newer version of Codex. Please upgrade
	// to the latest app or CLI and try again."). A plan upsell ("upgrade to Pro")
	// is not one of them.
	newerClientRe  = regexp.MustCompile(`(?i)(requires? a (newer|more recent) (version|release)|newer version of (the )?codex|upgrade to the latest (codex|app|cli|version)|(update|upgrade) (your )?codex|client version (is )?(too old|outdated|not supported|unsupported)|(unsupported|outdated) client version|minimum (client )?version)`)
	versionFieldRe = regexp.MustCompile(`"(?:minimal|minimum|min|required)_(?:client_)?version"\s*:\s*(?:"(\d+\.\d+\.\d+)"|\[\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*\])`)
	versionTextRe  = regexp.MustCompile(`\b(\d+\.\d+\.\d+)\b`)
)

// NeedsNewerClient reports whether an upstream error says the presented client
// version is too old, and the version it asks for when the error names one.
func NeedsNewerClient(status int, body []byte) (required string, ok bool) {
	if status < 400 || status >= 500 || status == http.StatusUnauthorized || status == http.StatusTooManyRequests || IsUsageLimit(body) {
		return "", false
	}
	msg := ErrorMessage(body)
	if !newerClientRe.MatchString(msg) && !versionFieldRe.Match(body) {
		return "", false
	}
	if m := versionFieldRe.FindSubmatch(body); m != nil {
		if len(m[1]) > 0 {
			return string(m[1]), true
		}
		return string(m[2]) + "." + string(m[3]) + "." + string(m[4]), true
	}
	if m := versionTextRe.FindStringSubmatch(msg); m != nil {
		return m[1], true
	}
	return "", true
}
