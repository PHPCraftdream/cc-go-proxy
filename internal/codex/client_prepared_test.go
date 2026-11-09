package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

type countedJSON struct {
	calls *atomic.Int32
}

func (c countedJSON) MarshalJSON() ([]byte, error) {
	c.calls.Add(1)
	return []byte(`"counted"`), nil
}

func TestPrepareRequestSnapshotsBodyAndHeaders(t *testing.T) {
	var marshals atomic.Int32
	req := &Request{
		Model:          "model-before",
		PromptCacheKey: "session-before",
		Input:          []any{countedJSON{calls: &marshals}},
	}
	prepared, err := PrepareRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := marshals.Load(); got != 1 {
		t.Fatalf("MarshalJSON called %d times during preparation, want 1", got)
	}
	if !json.Valid(prepared.body) || !strings.Contains(string(prepared.body), `"counted"`) {
		t.Fatalf("prepared body is not the serialized request: %s", prepared.body)
	}

	req.Model = "model-after"
	req.PromptCacheKey = "session-after"
	var tries atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read request: %v", readErr)
		}
		if string(body) != string(prepared.body) {
			t.Errorf("wire body = %s, want snapshot %s", body, prepared.body)
		}
		if got := r.Header.Get("session-id"); got != "session-before" {
			t.Errorf("session-id = %q, want snapshot", got)
		}
		if got := r.Header.Get("x-codex-routing-hint"); got != "model=model-before" {
			t.Errorf("routing hint = %q, want snapshot", got)
		}
		if tries.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new-token","refresh_token":"rt2"}`))
	}))
	defer issuer.Close()

	session := testSession(t)
	session.Client = &oauth.Client{Issuer: issuer.URL}
	for range 2 {
		resp, err := CallPrepared(context.Background(), backend.URL, session, prepared)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if got := marshals.Load(); got != 1 {
		t.Fatalf("MarshalJSON called %d times after prepared calls, want 1", got)
	}
}

func TestResponseDoesNotRetainUploadBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	resp, err := Call(context.Background(), backend.URL, testSession(t), &Request{
		Model: "m", Input: []any{strings.Repeat("large upload body ", 256<<10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Request == nil {
		t.Fatal("response has no request metadata")
	}
	if resp.Request.Body != http.NoBody {
		t.Errorf("response request body = %T, want http.NoBody", resp.Request.Body)
	}
	if resp.Request.GetBody != nil {
		t.Error("response request retains GetBody")
	}
	if resp.Request.URL == nil || resp.Request.URL.Host != strings.TrimPrefix(backend.URL, "http://") {
		t.Errorf("response request URL metadata was not preserved: %v", resp.Request.URL)
	}
	if got := resp.Request.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("response request headers were not preserved: Content-Type = %q", got)
	}
	if resp.Request.Context() == nil {
		t.Error("response request context was not preserved")
	}
}

func benchmarkPreparedSession(b *testing.B) *oauth.Session {
	b.Helper()
	store := oauth.Store{Path: filepath.Join(b.TempDir(), "oauth.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "tok", RefreshToken: "rt", LastRefresh: time.Now()}); err != nil {
		b.Fatal(err)
	}
	return &oauth.Session{Store: store, Client: &oauth.Client{}}
}

func benchmarkRetryTransport(b *testing.B) {
	old := HTTPClient
	var calls atomic.Int32
	HTTPClient = &http.Client{Transport: benchmarkRoundTripper(func(r *http.Request) (*http.Response, error) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return nil, err
		}
		n := calls.Add(1) % 3
		status := http.StatusServiceUnavailable
		if n == 0 {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})}
	b.Cleanup(func() { HTTPClient = old })
}

type benchmarkRoundTripper func(*http.Request) (*http.Response, error)

func (f benchmarkRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func benchmarkFiveMiBRequest() *Request {
	body := strings.Repeat("previous conversation includes tools, results, and context. ", (5<<20)/59)
	body = body[:5<<20]
	return &Request{Model: "benchmark-model", Input: []any{map[string]any{"type": "message", "role": "user", "content": body}}}
}

func BenchmarkRequestRetries(b *testing.B) {
	for _, mode := range []string{"BeforeEquivalent_CallEachRetry", "After_PrepareOnce"} {
		b.Run(mode, func(b *testing.B) {
			benchmarkRetryTransport(b)
			session := benchmarkPreparedSession(b)
			req := benchmarkFiveMiBRequest()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var prepared *Prepared
				if mode == "After_PrepareOnce" {
					var err error
					prepared, err = PrepareRequest(req)
					if err != nil {
						b.Fatal(err)
					}
				}
				for range 3 {
					var resp *http.Response
					var err error
					if prepared == nil {
						resp, err = Call(context.Background(), "https://benchmark.invalid", session, req)
					} else {
						resp, err = CallPrepared(context.Background(), "https://benchmark.invalid", session, prepared)
					}
					if err != nil {
						b.Fatal(err)
					}
					_ = resp.Body.Close()
				}
			}
		})
	}
}
