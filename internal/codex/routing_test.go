package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
)

type routingRoundTripper func(*http.Request) (*http.Response, error)

func (f routingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func useRoutingTransport(t *testing.T, before func(*http.Request), responseState string) {
	t.Helper()
	var responseCount atomic.Int32
	old := HTTPClient
	HTTPClient = &http.Client{Transport: routingRoundTripper(func(r *http.Request) (*http.Response, error) {
		if before != nil {
			before(r)
		}
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil && responseState != "" && responseCount.Add(1) == 1 {
			delete(resp.Header, http.CanonicalHeaderKey("x-codex-turn-state"))
			resp.Header["x-CoDeX-tUrN-sTaTe"] = []string{responseState}
		}
		return resp, err
	})}
	t.Cleanup(func() { HTTPClient = old })
}

func TestRoutingCapturesMixedCaseHeaderVerbatimAndReplaysOnNextCall(t *testing.T) {
	initial := "  raw: initial \t do not parse, hash, or trim  "
	returned := "  opaque: returned \t preserve, exactly  "
	var requestsMu sync.Mutex
	var requests []struct{ turnState, threadID, requestID, session, hint string }
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()
	useRoutingTransport(t, func(r *http.Request) {
		requestsMu.Lock()
		defer requestsMu.Unlock()
		requests = append(requests, struct{ turnState, threadID, requestID, session, hint string }{
			r.Header.Get("x-codex-turn-state"), r.Header.Get("thread-id"),
			r.Header.Get("x-client-request-id"), r.Header.Get("session-id"),
			r.Header.Get("x-codex-routing-hint"),
		})
	}, returned)

	var callback []string
	var callbackMu sync.Mutex
	routing := NewRouting("thread-17", initial, func(state string) {
		callbackMu.Lock()
		callback = append(callback, state)
		callbackMu.Unlock()
	})
	ctx := WithRouting(context.Background(), routing)
	req := &Request{Model: "gpt-test", PromptCacheKey: "session-9"}
	resp, err := Call(ctx, backend.URL, testSession(t), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// The custom RoundTripper inserts this mixed-case response header after
	// parsing; the next call verifies exact state replay and case-fold capture.
	resp, err = Call(ctx, backend.URL, testSession(t), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	requestsMu.Lock()
	got := append([]struct{ turnState, threadID, requestID, session, hint string }(nil), requests...)
	requestsMu.Unlock()
	if len(got) != 2 {
		t.Fatalf("captured %d requests, want 2", len(got))
	}
	if got[0].turnState != initial || got[1].turnState != returned {
		t.Fatalf("sent turn-state values = %q, %q", got[0].turnState, got[1].turnState)
	}
	for i, request := range got {
		if request.threadID != "thread-17" || request.requestID != "thread-17" || request.session != "session-9" || request.hint != "model=gpt-test" {
			t.Errorf("request %d routing/session headers = %+v", i, request)
		}
	}
	if got[0].requestID != "thread-17" || got[1].requestID != "thread-17" {
		t.Errorf("request IDs = %q, %q, want thread ID on both", got[0].requestID, got[1].requestID)
	}
	if !routing.Sent() || !routing.Returned() {
		t.Errorf("Sent()=%v Returned()=%v, want both true", routing.Sent(), routing.Returned())
	}
	callbackMu.Lock()
	gotCallback := append([]string(nil), callback...)
	callbackMu.Unlock()
	if len(gotCallback) != 1 || gotCallback[0] != returned {
		t.Errorf("state callbacks = %q, want [%q]", gotCallback, returned)
	}
}

func TestRoutingReplaysReturnedStateOnCompressionFallback(t *testing.T) {
	resetCompression(t)
	var got []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("x-codex-turn-state"))
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Content-Encoding") != "" {
			w.Header().Set("X-Codex-Turn-State", "after-compression-refusal;v=1")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()

	routing := NewRouting("thread-compress", "before-compression", nil)
	resp, err := Call(WithRouting(context.Background(), routing), backend.URL, testSession(t), bigRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(got) != 2 || got[0] != "before-compression" || got[1] != "after-compression-refusal;v=1" {
		t.Fatalf("turn-state values across compressed/plain POSTs = %q", got)
	}
	if !routing.Sent() || !routing.Returned() {
		t.Errorf("Sent()=%v Returned()=%v, want both true", routing.Sent(), routing.Returned())
	}
}

func TestRoutingReplaysReturnedStateOnOAuthRefresh(t *testing.T) {
	var got []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("x-codex-turn-state"))
		if len(got) == 1 {
			w.Header().Set("X-Codex-Turn-State", "after-401;opaque")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new-token","refresh_token":"rt2"}`))
	}))
	defer issuer.Close()

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "oauth.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "old-token", RefreshToken: "rt1", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	session := &oauth.Session{Store: store, Client: &oauth.Client{Issuer: issuer.URL}}
	routing := NewRouting("thread-oauth", "before-401", nil)
	resp, err := Call(WithRouting(context.Background(), routing), backend.URL, session, &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(got) != 2 || got[0] != "before-401" || got[1] != "after-401;opaque" {
		t.Fatalf("turn-state values across OAuth POSTs = %q", got)
	}
	if !routing.Sent() || !routing.Returned() {
		t.Errorf("Sent()=%v Returned()=%v, want both true", routing.Sent(), routing.Returned())
	}
}

func TestRoutingPreservesStateWhenResponseHeaderIsMissing(t *testing.T) {
	var got []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("x-codex-turn-state"))
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()
	useRoutingTransport(t, nil, "returned-on-first-post")

	routing := NewRouting("thread-preserve", "initial-state", nil)
	ctx := WithRouting(context.Background(), routing)
	for range 3 {
		resp, err := Call(ctx, backend.URL, testSession(t), &Request{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if len(got) != 3 || got[0] != "initial-state" || got[1] != "returned-on-first-post" || got[2] != "returned-on-first-post" {
		t.Fatalf("turn-state values after a response without the header = %q", got)
	}
}

func TestRoutingSentRemainsFalseWithoutOpaqueState(t *testing.T) {
	var threadID, requestID, turnState string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("data: ok\n\n"))
	}))
	defer backend.Close()
	useRoutingTransport(t, func(r *http.Request) {
		threadID = r.Header.Get("thread-id")
		requestID = r.Header.Get("x-client-request-id")
		turnState = r.Header.Get("x-codex-turn-state")
	}, "")

	routing := NewRouting("thread-empty", "", nil)
	resp, err := Call(WithRouting(context.Background(), routing), backend.URL, testSession(t), &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if threadID != "thread-empty" || requestID != "thread-empty" || turnState != "" {
		t.Errorf("routing headers: thread-id=%q x-client-request-id=%q turn-state=%q", threadID, requestID, turnState)
	}
	if routing.Sent() || routing.Returned() {
		t.Errorf("Sent()=%v Returned()=%v, want both false", routing.Sent(), routing.Returned())
	}
}
