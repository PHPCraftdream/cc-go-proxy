package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// resetCompression makes a test independent of the process-wide switch.
func resetCompression(t *testing.T) {
	t.Helper()
	noCompress.Store(false)
	t.Cleanup(func() { noCompress.Store(false) })
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
	if !noCompress.Load() {
		t.Error("the compression is still on after the backend refused it")
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
	if noCompress.Load() {
		t.Error("a genuinely bad request switched the compression off")
	}
}

func TestErrorMessageOfAnHTMLPage(t *testing.T) {
	page := "<html> <head> <meta name=\"viewport\" content=\"width=device-width\" /> <style global>body{font-family:Arial}</style>\n" +
		"<title>chatgpt.com | 504:\n Gateway time-out </title></head><body>" + strings.Repeat("x", 2000) + "</body></html>"
	for _, c := range []struct{ in, want string }{
		{page, "html error page: chatgpt.com | 504: Gateway time-out"},
		{"<!DOCTYPE html><html><body>oops</body></html>", "html error page"},
		{"<html><head><title></title></head></html>", "html error page"},
		{strings.Repeat("y", 600), strings.Repeat("y", 500)}, // not a page: cut as before
	} {
		if got := ErrorMessage([]byte(c.in)); got != c.want {
			t.Errorf("ErrorMessage(%.40q…) = %q, want %q", c.in, got, c.want)
		}
	}
}
