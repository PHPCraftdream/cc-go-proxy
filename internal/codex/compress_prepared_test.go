package codex

import (
	"bytes"
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
