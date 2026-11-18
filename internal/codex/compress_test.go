package codex

import (
	"bytes"
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

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/klauspost/compress/zstd"
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

// resetCompression makes a test independent of the refusal and acceptance caches.
func resetCompression(t *testing.T) {
	t.Helper()
	clear := func(m *endpointMemory) {
		m.Lock()
		m.endpoints = make(map[string]uint64)
		m.next = 0
		m.Unlock()
	}
	clear(compressionRefusals)
	clear(compressionAcceptances)
	t.Cleanup(func() {
		clear(compressionRefusals)
		clear(compressionAcceptances)
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

// A 400 for a compressed body at an endpoint that has accepted compression
// before is the request's own error (a genuinely bad request): it goes back
// to the caller without the plain retry.
func TestCallRememberedAcceptedEndpointKeepsABadRequestCompressed(t *testing.T) {
	resetCompression(t)
	var compressed, plain atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Content-Encoding") != "" {
			if compressed.Add(1) == 1 {
				_, _ = w.Write([]byte("data: {}\n\n"))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad input"}}`))
			return
		}
		plain.Add(1)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer backend.Close()

	s := testSession(t)
	endpoint := backend.URL + "/responses"
	resp, err := Call(context.Background(), endpoint, s, bigRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || compressed.Load() != 1 {
		t.Fatalf("first call: status %d after %d compressed tries, want 200 after 1", resp.StatusCode, compressed.Load())
	}

	var wire Wire
	resp, err = Call(WithWire(context.Background(), &wire), endpoint, s, bigRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || compressed.Load() != 2 || plain.Load() != 0 {
		t.Fatalf("second call: status %d after %d compressed and %d plain tries, want 400 after 2 and 0", resp.StatusCode, compressed.Load(), plain.Load())
	}
	if wire.Encoding != "zstd" {
		t.Errorf("wire after the remembered acceptance = %+v, want zstd as sent", wire)
	}
	if compressionRefused(endpoint) {
		t.Error("a bad request at an endpoint that takes compression switched the compression off")
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

func TestCallCompressionAcceptedStateIsBounded(t *testing.T) {
	resetCompression(t)
	for i := 0; i < maxAcceptedEndpoints+10; i++ {
		acceptCompression("https://example.com/" + strconv.Itoa(i) + "/responses")
	}
	compressionAcceptances.Lock()
	got := len(compressionAcceptances.endpoints)
	compressionAcceptances.Unlock()
	if got > maxAcceptedEndpoints {
		t.Fatalf("accepted state has %d entries, cap is %d", got, maxAcceptedEndpoints)
	}
	if compressionAccepted("https://example.com/0/responses") {
		t.Error("oldest endpoint should have been evicted")
	}
	if !compressionAccepted("https://example.com/" + strconv.Itoa(maxAcceptedEndpoints+9) + "/responses") {
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
			resp, err := sendPrepared(context.Background(), backend.URL+"/responses", &Prepared{body: []byte(strings.Repeat("x", compressThreshold+1))}, &oauth.Tokens{AccessToken: "tok"})
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

type countingBodyEncoder struct {
	delegate bodyEncoder
	calls    atomic.Int32
}

func (e *countingBodyEncoder) EncodeAll(src, dst []byte) []byte {
	e.calls.Add(1)
	return e.delegate.EncodeAll(src, dst)
}

func countEncodes(t *testing.T) *countingBodyEncoder {
	t.Helper()
	old := encoder
	counting := &countingBodyEncoder{delegate: old}
	encoder = counting
	t.Cleanup(func() { encoder = old })
	return counting
}

type countedText struct {
	calls *atomic.Int32
	text  string
}

func (v countedText) MarshalJSON() ([]byte, error) {
	v.calls.Add(1)
	return json.Marshal(v.text)
}

func preparedCountedRequest(t *testing.T) (*Prepared, []byte, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	req := &Request{
		Model: "m", PromptCacheKey: "prepared-session",
		Input: []any{countedText{calls: calls, text: strings.Repeat("prepared body ", 5000)}},
	}
	want, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	calls.Store(0)
	p, err := PrepareRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("MarshalJSON calls during preparation = %d, want 1", calls.Load())
	}
	if !bytes.Equal(p.body, want) {
		t.Fatal("prepared body differs from json.Marshal(req)")
	}
	return p, want, calls
}

func TestPreparedCompressionIsReusedAcrossRetries(t *testing.T) {
	resetCompression(t)
	counting := countEncodes(t)
	p, body, marshals := preparedCountedRequest(t)
	wantPacked := counting.delegate.EncodeAll(body, nil)
	type attempt struct {
		encoding, turnState, threadID, requestID string
		body                                     []byte
	}
	var attempts []attempt
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		attempts = append(attempts, attempt{
			encoding: r.Header.Get("Content-Encoding"), turnState: r.Header.Get("x-codex-turn-state"),
			threadID: r.Header.Get("thread-id"), requestID: r.Header.Get("x-client-request-id"), body: got,
		})
		w.Header().Set("X-Codex-Turn-State", "turn-"+string(rune('0'+len(attempts))))
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer backend.Close()

	const retries = 4
	var packed []byte
	routing := NewRouting("prepared-thread", "initial-turn", nil)
	ctx := WithRouting(context.Background(), routing)
	for i := 0; i < retries; i++ {
		var wire Wire
		resp, err := CallPrepared(WithWire(ctx, &wire), backend.URL, testSession(t), p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("attempt %d status = %d", i, resp.StatusCode)
		}
		if wire.Raw != len(body) || wire.Encoding != "zstd" || wire.Sent == len(body) {
			t.Errorf("attempt %d wire = %+v", i, wire)
		}
	}
	if len(attempts) != retries {
		t.Fatalf("got %d attempts, want %d", len(attempts), retries)
	}
	if !routing.Sent() || !routing.Returned() {
		t.Errorf("routing sent=%v returned=%v, want both true", routing.Sent(), routing.Returned())
	}
	for i, got := range attempts {
		if got.encoding != "zstd" {
			t.Errorf("attempt %d encoding = %q, want zstd", i, got.encoding)
		}
		if got.threadID != "prepared-thread" || got.requestID != "prepared-thread" {
			t.Errorf("attempt %d routing IDs = %q, %q", i, got.threadID, got.requestID)
		}
		wantState := []string{"initial-turn", "turn-1", "turn-2", "turn-3"}[i]
		if got.turnState != wantState {
			t.Errorf("attempt %d turn state = %q, want %q", i, got.turnState, wantState)
		}
		if i == 0 {
			packed = got.body
		} else if !bytes.Equal(got.body, packed) {
			t.Errorf("attempt %d packed bytes differ from first attempt", i)
		}
	}
	if got := decodeZstd(t, packed); !bytes.Equal(got, body) {
		t.Error("decoded packed bytes differ from the prepared JSON body")
	}
	if !bytes.Equal(p.packed, packed) {
		t.Error("sent packed bytes differ from the prepared cache")
	}
	if !bytes.Equal(packed, wantPacked) {
		t.Error("sent packed bytes differ from original encoder.EncodeAll output")
	}
	if got := counting.calls.Load(); got != 1 {
		t.Errorf("EncodeAll calls = %d, want exactly 1", got)
	}
	if got := marshals.Load(); got != 1 {
		t.Errorf("request MarshalJSON calls = %d, want exactly 1", got)
	}
}

func TestPreparedCompressionCacheSurvives401Retry(t *testing.T) {
	resetCompression(t)
	counting := countEncodes(t)
	p, body, _ := preparedCountedRequest(t)
	type attempt struct {
		encoding, authorization, turnState, threadID, requestID, sessionID string
		body                                                               []byte
	}
	var attempts []attempt
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		attempts = append(attempts, attempt{
			encoding: r.Header.Get("Content-Encoding"), authorization: r.Header.Get("Authorization"),
			turnState: r.Header.Get("x-codex-turn-state"), threadID: r.Header.Get("thread-id"),
			requestID: r.Header.Get("x-client-request-id"), sessionID: r.Header.Get("session-id"), body: got,
		})
		if len(attempts) == 1 {
			w.Header().Set("X-Codex-Turn-State", "refreshed-turn")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "oauth.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "old-token", RefreshToken: "rt", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new-token","refresh_token":"rt2"}`))
	}))
	defer issuer.Close()
	session := &oauth.Session{Store: store, Client: &oauth.Client{Issuer: issuer.URL}}
	routing := NewRouting("prepared-thread", "initial-turn", nil)
	resp, err := CallPrepared(WithRouting(context.Background(), routing), backend.URL, session, p)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(attempts) != 2 {
		t.Fatalf("POST attempts = %d, want 2", len(attempts))
	}
	for i, got := range attempts {
		if got.encoding != "zstd" || !bytes.Equal(got.body, attempts[0].body) {
			t.Errorf("attempt %d did not reuse the compressed body", i)
		}
		if got.authorization != "Bearer "+[]string{"old-token", "new-token"}[i] {
			t.Errorf("attempt %d authorization = %q", i, got.authorization)
		}
		if got.threadID != "prepared-thread" || got.requestID != "prepared-thread" || got.sessionID != "prepared-session" {
			t.Errorf("attempt %d headers thread=%q request=%q session=%q", i, got.threadID, got.requestID, got.sessionID)
		}
	}
	if attempts[0].turnState != "initial-turn" || attempts[1].turnState != "refreshed-turn" || !routing.Sent() || !routing.Returned() {
		t.Errorf("dynamic turn state: attempts=%+v sent=%v returned=%v", attempts, routing.Sent(), routing.Returned())
	}
	if decoded := decodeZstd(t, attempts[0].body); !bytes.Equal(decoded, body) {
		t.Error("decoded body differs from prepared JSON")
	}
	if !bytes.Equal(p.packed, attempts[0].body) {
		t.Error("sent packed bytes differ from prepared cache")
	}
	if got := counting.calls.Load(); got != 1 {
		t.Errorf("EncodeAll calls = %d, want exactly 1", got)
	}
}

func TestPreparedFallbackAndPlainEndpointRemainLazy(t *testing.T) {
	resetCompression(t)
	counting := countEncodes(t)
	p, body, _ := preparedCountedRequest(t)
	type attempt struct {
		path, encoding string
		body           []byte
	}
	var attempts []attempt
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		attempts = append(attempts, attempt{path: r.URL.Path, encoding: r.Header.Get("Content-Encoding"), body: got})
		if r.URL.Path == "/reject/responses" && r.Header.Get("Content-Encoding") != "" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer backend.Close()

	refuseCompression(backend.URL + "/plain/responses")
	for i, path := range []string{"/plain/responses", "/reject/responses", "/reject/responses"} {
		var wire Wire
		resp, err := sendPrepared(WithWire(context.Background(), &wire), backend.URL+path, p, &oauth.Tokens{AccessToken: "tok"})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if wire.Raw != len(body) {
			t.Errorf("call %d wire raw = %d, want %d", i, wire.Raw, len(body))
		}
		if wire.Encoding != "" || wire.Sent != len(body) {
			t.Errorf("call %d final wire = %+v, want plain %d-byte body", i, wire, len(body))
		}
		if i == 0 && counting.calls.Load() != 0 {
			t.Errorf("plain endpoint triggered %d encodes", counting.calls.Load())
		}
	}
	acceptingURL := backend.URL + "/accept"
	for i := 0; i < 2; i++ {
		var wire Wire
		resp, err := CallPrepared(WithWire(context.Background(), &wire), acceptingURL, testSession(t), p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if wire.Raw != len(body) || wire.Encoding != "zstd" || wire.Sent != len(p.packed) {
			t.Errorf("accepted call %d wire = %+v, want cached zstd body (%d bytes)", i, wire, len(p.packed))
		}
	}
	if len(attempts) != 6 {
		t.Fatalf("wire attempts = %d, want refused plain, compression refusal fallback and cache hit, plus accepted compressed twice", len(attempts))
	}
	if attempts[0].path != "/plain/responses" || attempts[0].encoding != "" || !bytes.Equal(attempts[0].body, body) {
		t.Errorf("refused endpoint request = %+v", attempts[0])
	}
	if attempts[1].encoding != "zstd" || attempts[2].encoding != "" || attempts[3].encoding != "" {
		t.Errorf("refusal sequence encodings = %q, %q, %q", attempts[1].encoding, attempts[2].encoding, attempts[3].encoding)
	}
	if attempts[4].encoding != "zstd" || attempts[5].encoding != "zstd" {
		t.Errorf("accepted endpoint encodings = %q, %q, want zstd twice", attempts[4].encoding, attempts[5].encoding)
	}
	packed := attempts[1].body
	if decoded := decodeZstd(t, packed); !bytes.Equal(decoded, body) {
		t.Error("decoded packed bytes differ from prepared JSON")
	}
	if !bytes.Equal(p.packed, packed) || !bytes.Equal(attempts[2].body, body) || !bytes.Equal(attempts[3].body, body) {
		t.Error("fallback did not preserve the cached packed and original plain bodies")
	}
	if !bytes.Equal(attempts[4].body, packed) || !bytes.Equal(attempts[5].body, packed) {
		t.Error("accepted endpoint retries did not send the exact cached packed bytes")
	}
	if got := counting.calls.Load(); got != 1 {
		t.Errorf("EncodeAll calls = %d, want exactly 1", got)
	}
}

// Only the top-level string "fast" selects Fast; anything else parses and is not Fast.
func TestClientFastOnlyForTheFastString(t *testing.T) {
	for _, speed := range []string{`1`, `true`, `{"a":1}`, `["fast"]`, `"FAST"`, `"fast "`, `""`} {
		p, err := ParseRequest([]byte(`{"model":"x","speed":` + speed + `}`))
		if err != nil {
			t.Fatalf("speed %s rejected: %v", speed, err)
		}
		if p.ClientFast() {
			t.Errorf("speed %s selected Fast", speed)
		}
	}
	p, err := ParseRequest([]byte(`{"model":"x","metadata":{"speed":"fast"}}`))
	if err != nil || p.ClientFast() {
		t.Errorf("nested speed selected Fast: %v", err)
	}
}

// The client's speed field is not forwarded: only a resolved tier reaches the wire.
func TestSpeedFieldIsNotForwarded(t *testing.T) {
	p, err := ParseRequest([]byte(`{"model":"x","speed":"fast","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req, _, err := p.Convert("gpt-6.2-sol", "codex")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if req.ServiceTier != "" || strings.Contains(string(body), "speed") || strings.Contains(string(body), "service_tier") {
		t.Errorf("request body: %s", body)
	}
}
