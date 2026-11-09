package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

func testSession(t *testing.T) *oauth.Session {
	t.Helper()
	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return &oauth.Session{Store: store, Client: &oauth.Client{}}
}

// bigRequest is well over compressThreshold.
func bigRequest() *Request {
	text := strings.Repeat("hello world, this is a long conversation. ", 4000)
	return &Request{Model: "m", Input: []any{map[string]any{"type": "message", "role": "user", "content": text}}}
}

// resetCompression makes a test independent of the refusal cache.
func resetCompression(t *testing.T) {
	t.Helper()
	compressionRefusals.Lock()
	compressionRefusals.endpoints = make(map[string]uint64)
	compressionRefusals.next = 0
	compressionRefusals.Unlock()
	t.Cleanup(func() {
		compressionRefusals.Lock()
		compressionRefusals.endpoints = make(map[string]uint64)
		compressionRefusals.next = 0
		compressionRefusals.Unlock()
	})
}

// seen holds what the backend handler saw; the test reads it from another goroutine.
type seen struct {
	mu   sync.Mutex
	enc  string
	body []byte
}

func (s *seen) set(enc string, body []byte) {
	s.mu.Lock()
	s.enc, s.body = enc, body
	s.mu.Unlock()
}

func (s *seen) get() (string, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc, s.body
}

func decodeZstd(t *testing.T, b []byte) []byte {
	t.Helper()
	d, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	out, err := d.DecodeAll(b, nil)
	if err != nil {
		t.Fatalf("body is not valid zstd: %v", err)
	}
	return out
}

func TestCallSendsALargeBodyCompressed(t *testing.T) {
	resetCompression(t)
	var rec seen
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.set(r.Header.Get("Content-Encoding"), b)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer backend.Close()

	var wire Wire
	resp, err := Call(WithWire(context.Background(), &wire), backend.URL, testSession(t), bigRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	enc, got := rec.get()
	if enc != "zstd" {
		t.Fatalf("Content-Encoding = %q, want zstd", enc)
	}
	plain := decodeZstd(t, got)
	var back Request
	if err := json.Unmarshal(plain, &back); err != nil || back.Model != "m" || len(back.Input) != 1 {
		t.Fatalf("decoded body is not the request: %v %q", err, plain[:min(len(plain), 80)])
	}
	if wire.Encoding != "zstd" || wire.Raw != len(plain) || wire.Sent != len(got) || wire.Sent*3 > wire.Raw {
		t.Errorf("wire = %+v for %d bytes sent of %d", wire, len(got), len(plain))
	}
}

func TestCallLeavesASmallBodyPlain(t *testing.T) {
	resetCompression(t)
	var rec seen
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.set(r.Header.Get("Content-Encoding"), b)
	}))
	defer backend.Close()

	var wire Wire
	resp, err := Call(WithWire(context.Background(), &wire), backend.URL, testSession(t), &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	enc, got := rec.get()
	if enc != "" || !json.Valid(got) {
		t.Errorf("Content-Encoding = %q, body valid JSON: %v", enc, json.Valid(got))
	}
	if wire.Encoding != "" || wire.Raw != len(got) || wire.Sent != len(got) {
		t.Errorf("wire = %+v for %d bytes", wire, len(got))
	}
}

// A backend that rejects the encoding still gets served: the same request goes
// again as plain, and later requests do not try the compression.
func TestCallFallsBackToPlainWhenCompressionIsRefused(t *testing.T) {
	resetCompression(t)
	var compressed, plain atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Content-Encoding") != "" {
			compressed.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		plain.Add(1)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer backend.Close()

	var notices []string
	oldNotice := Notice
	Notice = func(m string) { notices = append(notices, m) }
	t.Cleanup(func() { Notice = oldNotice })

	s := testSession(t)
	var wire Wire
	resp, err := Call(WithWire(context.Background(), &wire), backend.URL, s, bigRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || compressed.Load() != 1 || plain.Load() != 1 {
		t.Fatalf("status %d, compressed tries %d, plain tries %d", resp.StatusCode, compressed.Load(), plain.Load())
	}
	if wire.Encoding != "" || wire.Sent != wire.Raw {
		t.Errorf("wire after the fallback = %+v", wire)
	}
	if !compressionRefused(backend.URL + "/responses") {
		t.Error("compression is still enabled for the endpoint after it refused compression")
	}

	resp, err = Call(context.Background(), backend.URL, s, bigRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if compressed.Load() != 1 || plain.Load() != 2 {
		t.Errorf("second call: compressed tries %d, plain tries %d, want 1 and 2", compressed.Load(), plain.Load())
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "refused a compressed body (HTTP 400)") {
		t.Errorf("notices = %q, want one about the refusal", notices)
	}
}

// The session headers ride on every try: the compressed one and the plain one
// sent again after a refusal.
func TestCallKeepsTheSessionHeadersOnEveryBodyForm(t *testing.T) {
	resetCompression(t)
	type try struct{ enc, session, hint string }
	var mu sync.Mutex
	var tries []try
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		enc := r.Header.Get("Content-Encoding")
		mu.Lock()
		tries = append(tries, try{enc, r.Header.Get("session-id"), r.Header.Get("x-codex-routing-hint")})
		mu.Unlock()
		if enc != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer backend.Close()

	req := bigRequest()
	req.PromptCacheKey = "key9"
	resp, err := Call(context.Background(), backend.URL, testSession(t), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(tries) != 2 || tries[0].enc != "zstd" || tries[1].enc != "" {
		t.Fatalf("tries = %+v, want a zstd one, then a plain one", tries)
	}
	for i, tr := range tries {
		if tr.session != "key9" || tr.hint != "model=m" {
			t.Errorf("try %d (%q): session-id %q, routing hint %q", i, tr.enc, tr.session, tr.hint)
		}
	}
}

// A request that is bad in itself fails both ways and must not switch the
// compression off.
func TestCallKeepsCompressionWhenThePlainBodyFailsToo(t *testing.T) {
	resetCompression(t)
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad input"}}`))
	}))
	defer backend.Close()

	resp, err := Call(context.Background(), backend.URL, testSession(t), bigRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 || calls.Load() != 2 {
		t.Fatalf("status %d after %d calls, want 400 after 2", resp.StatusCode, calls.Load())
	}
	if compressionRefused(backend.URL + "/responses") {
		t.Error("a genuinely bad request switched the compression off")
	}
}

func TestCallCompressionRefusalIsPerNormalizedEndpoint(t *testing.T) {
	resetCompression(t)
	type counts struct {
		compressed atomic.Int32
		plain      atomic.Int32
	}
	var a, b counts
	newBackend := func(c *counts) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if r.Header.Get("Content-Encoding") != "" {
				c.compressed.Add(1)
				w.WriteHeader(http.StatusUnsupportedMediaType)
				return
			}
			c.plain.Add(1)
			_, _ = w.Write([]byte("data: {}\n\n"))
		}))
	}
	backendA, backendB := newBackend(&a), newBackend(&b)
	defer backendA.Close()
	defer backendB.Close()

	var noticeMu sync.Mutex
	var notices []string
	oldNotice := Notice
	Notice = func(m string) {
		noticeMu.Lock()
		notices = append(notices, m)
		noticeMu.Unlock()
	}
	t.Cleanup(func() { Notice = oldNotice })

	s := testSession(t)
	urls := []string{
		strings.Replace(backendA.URL, "://", "://user:secret@", 1) + "/API/",
		backendA.URL + "/api",
		backendB.URL,
		backendB.URL,
	}
	for _, endpoint := range urls {
		resp, err := Call(context.Background(), endpoint, s, bigRequest())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Call(%q) status = %d, want 200", endpoint, resp.StatusCode)
		}
	}
	if a.compressed.Load() != 1 || a.plain.Load() != 2 {
		t.Errorf("endpoint A: compressed tries %d, plain tries %d, want 1 and 2", a.compressed.Load(), a.plain.Load())
	}
	if b.compressed.Load() != 1 || b.plain.Load() != 2 {
		t.Errorf("endpoint B: compressed tries %d, plain tries %d, want 1 and 2", b.compressed.Load(), b.plain.Load())
	}
	noticeMu.Lock()
	defer noticeMu.Unlock()
	if len(notices) != 2 {
		t.Fatalf("notices = %q, want one per endpoint", notices)
	}
	for _, notice := range notices {
		if strings.Contains(notice, "secret") || strings.Contains(notice, "user") {
			t.Errorf("notice leaked URL credentials: %q", notice)
		}
	}
	if !strings.Contains(notices[0], "http://") || !strings.Contains(notices[0], "/api ") ||
		!strings.Contains(notices[1], backendB.Listener.Addr().String()) {
		t.Errorf("notices do not identify their endpoints: %q", notices)
	}
	if got := normalizedEndpoint("HTTPS://EXAMPLE.COM/API///responses"); got != "https://example.com/api" {
		t.Errorf("normalized endpoint = %q, want https://example.com/api", got)
	}
}

func TestCompressionRefusalStateIsBounded(t *testing.T) {
	resetCompression(t)
	for i := 0; i < maxRefusedEndpoints+10; i++ {
		refuseCompression("https://example.com/" + strconv.Itoa(i) + "/responses")
	}
	compressionRefusals.Lock()
	got := len(compressionRefusals.endpoints)
	compressionRefusals.Unlock()
	if got > maxRefusedEndpoints {
		t.Fatalf("refusal state has %d entries, cap is %d", got, maxRefusedEndpoints)
	}
	if compressionRefused("https://example.com/0/responses") {
		t.Error("oldest endpoint should have been evicted")
	}
	if !compressionRefused("https://example.com/" + strconv.Itoa(maxRefusedEndpoints+9) + "/responses") {
		t.Error("most recent endpoint should remain cached")
	}
}

func TestConcurrentCompressionRefusalsAreSafe(t *testing.T) {
	resetCompression(t)
	var compressed, plain atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Content-Encoding") != "" {
			compressed.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		plain.Add(1)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer backend.Close()

	var noticeMu sync.Mutex
	notices := 0
	oldNotice := Notice
	Notice = func(string) {
		noticeMu.Lock()
		notices++
		noticeMu.Unlock()
	}
	t.Cleanup(func() { Notice = oldNotice })

	const workers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := send(context.Background(), backend.URL+"/responses", []byte(strings.Repeat("x", compressThreshold+1)), nil, &oauth.Tokens{AccessToken: "tok"})
			if err != nil {
				t.Errorf("send: %v", err)
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("send status = %d, want 200", resp.StatusCode)
			}
		}()
	}
	close(start)
	wg.Wait()
	if compressed.Load() == 0 || plain.Load() == 0 {
		t.Errorf("compressed tries %d, plain tries %d, want both", compressed.Load(), plain.Load())
	}
	noticeMu.Lock()
	defer noticeMu.Unlock()
	if notices != 1 {
		t.Errorf("notices = %d, want one", notices)
	}
}

func TestErrorMessageOfAnHTMLPage(t *testing.T) {
	page := "<html> <head> <meta name=\"viewport\" content=\"width=device-width\" /> <style global>body{font-family:Arial}</style>\n" +
		"<title>chatgpt.com | 504:\n Gateway time-out </title></head><body>" + strings.Repeat("x", 2000) + "</body></html>"
	for _, c := range []struct{ in, want string }{
		{page, "html error page: chatgpt.com | 504: Gateway time-out"},
		{"<!DOCTYPE html><html><body>oops</body></html>", "html error page: oops"},
		{"<html><head><title></title></head></html>", "html error page"},
		// no title: the start of the visible text, without scripts, styles and tags
		{"<html><head><style>p{}</style></head><body><script>var a=1;</script><h1>Attention\nRequired!</h1><p>Sorry,  you have been blocked</p></body></html>",
			"html error page: Attention Required! Sorry, you have been blocked"},
		{"<html><body>" + strings.Repeat("é", 400) + "</body></html>", "html error page: " + strings.Repeat("é", 150) + "…"},
		{strings.Repeat("y", 600), strings.Repeat("y", 500)}, // not a page: cut as before
		// the address a block page ends with is kept past the cut
		{"<html><body><p>" + strings.Repeat("v", 160) + " [IP: 203.0.113.7]</p></body></html>",
			"html error page: " + strings.Repeat("v", 150) + "… [IP: 203.0.113.7]"},
		{"<html><body><p>" + strings.Repeat("v", 160) + " [IP:2001:db8::1]</p></body></html>",
			"html error page: " + strings.Repeat("v", 150) + "… [IP:2001:db8::1]"},
		// the address is not cut in two, wherever it stands in a long text
		{"<html><body><p>Blocked [IP:203.0.113.7] " + strings.Repeat("w", 300) + "</p></body></html>",
			"html error page: Blocked " + strings.Repeat("w", 150-len("Blocked ")) + "… [IP:203.0.113.7]"},
		{"<html><body><p>" + strings.Repeat("v", 140) + " [IP: 203.0.113.7] tail</p></body></html>",
			"html error page: " + strings.Repeat("v", 140) + " tail [IP: 203.0.113.7]"},
		// a short text is shown as it is
		{"<html><body><p>Blocked [IP:203.0.113.7] here</p></body></html>", "html error page: Blocked [IP:203.0.113.7] here"},
	} {
		if got := ErrorMessage([]byte(c.in)); got != c.want {
			t.Errorf("ErrorMessage(%.40q…) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsHTMLPage(t *testing.T) {
	for in, want := range map[string]bool{
		"<html><body>x</body></html>":          true,
		"<!DOCTYPE HTML><p>x</p>":              true,
		"  \n<HTML>":                           true,
		`{"error":{"message":"usage limit"}}`:  false,
		`{"detail":"<html> in a JSON string"}`: false, // JSON is the backend's own
		"":                                     false,
		strings.Repeat("a", 300) + "<html>":    false,
		"plain text, nothing to see":           false,
	} {
		if got := IsHTMLPage([]byte(in)); got != want {
			t.Errorf("IsHTMLPage(%.30q) = %v, want %v", in, got, want)
		}
	}
}
