package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v2"
)

const (
	codexWSTestAccessToken = "synthetic-ws-access-token-never-log"
	codexWSTestRefresh     = "synthetic-ws-refresh-never-log"
	codexWSTestAccount     = "synthetic-ws-account-never-log"
	codexWSTestTurn        = "synthetic-ws-turn-state-never-log"
)

type codexWSTestHarness struct {
	backend       *httptest.Server
	app           *fiber.App
	access        *syncBuf
	stream        *syncBuf
	waitForStream func()
}

func newCodexWSTestHarness(t *testing.T, backend http.Handler, transport string) *codexWSTestHarness {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)

	oldManager, oldFallbacks := codexWSManager, codexWSFallbacks.Load()
	manager := codex.NewWSManager()
	codexWSManagerMu.Lock()
	codexWSManager = manager
	codexWSManagerMu.Unlock()
	codexWSFallbacks.Store(0)

	oldClient, oldLog := codex.HTTPClient, logOut
	oldTurnStates, oldPromptWatch, oldPromptStats, oldLinkHealth := turnStates, promptWatch, promptStats, linkHealth
	turnStates, promptWatch, promptStats, linkHealth = &turnStateStore{}, &cacheWatch{}, &cacheStats{}, &linkStats{}
	codex.HTTPClient = &http.Client{Transport: &http.Transport{}}
	streamLog := &syncBuf{}
	logOut = streamLog
	var harness *codexWSTestHarness
	t.Cleanup(func() {
		if harness != nil && harness.waitForStream != nil {
			harness.waitForStream()
		}
		manager.Close()
		codexWSManagerMu.Lock()
		codexWSManager = oldManager
		codexWSManagerMu.Unlock()
		codexWSFallbacks.Store(oldFallbacks)
		codex.HTTPClient.CloseIdleConnections()
		codex.HTTPClient, logOut = oldClient, oldLog
		turnStates, promptWatch, promptStats, linkHealth = oldTurnStates, oldPromptWatch, oldPromptStats, oldLinkHealth
	})

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "synthetic-oauth.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: codexWSTestAccessToken, RefreshToken: codexWSTestRefresh, AccountID: codexWSTestAccount, LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	session := &oauth.Session{Store: store, Client: &oauth.Client{}}
	cfg := &config.Config{
		Profile: "codex", ProviderName: "codex", CodexTransport: transport,
		Providers: map[string]*config.Provider{"codex": {Name: "codex", Type: "codex", BaseURL: server.URL}},
	}
	accessLogWriter := &syncBuf{}
	app := fiber.New()
	app.Use(accessLog(accessLogWriter))
	app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error {
		return handleCodexMessages(c, cfg, session)
	})
	harness = &codexWSTestHarness{backend: server, app: app, access: accessLogWriter, stream: streamLog}
	return harness
}

func (h *codexWSTestHarness) post(t *testing.T, body string) (int, string) {
	t.Helper()
	result := h.postResult(body)
	if result.err != nil {
		t.Fatal(result.err)
	}
	return result.status, result.body
}

type codexWSTestPostResult struct {
	status int
	body   string
	err    error
}

func (h *codexWSTestHarness) postResult(body string) codexWSTestPostResult {
	req := httptest.NewRequest(fiber.MethodPost, "/call/synthetic-token/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.app.Test(req, -1)
	if err != nil {
		return codexWSTestPostResult{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return codexWSTestPostResult{status: resp.StatusCode, body: string(b), err: err}
}

func acceptCodexWSTest(t *testing.T, w http.ResponseWriter, r *http.Request) *websocket.Conn {
	t.Helper()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Errorf("accept synthetic websocket: %v", err)
		return nil
	}
	return conn
}

func readCodexWSTestFrame(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, frame, err := conn.Read(ctx)
	if err != nil {
		t.Errorf("read synthetic websocket frame: %v", err)
		return nil
	}
	if typ != websocket.MessageText {
		t.Errorf("websocket frame type = %v, want text", typ)
	}
	return append([]byte(nil), frame...)
}

func writeCodexWSTestEvents(t *testing.T, conn *websocket.Conn, events ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, event := range events {
		if err := conn.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
			t.Errorf("write synthetic websocket event: %v", err)
			return
		}
	}
}

func decodeCodexWSFrame(t *testing.T, frame []byte) map[string]json.RawMessage {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(frame, &got); err != nil {
		t.Fatalf("decode websocket frame %q: %v", frame, err)
	}
	return got
}

func codexWSFrameInput(t *testing.T, frame map[string]json.RawMessage) []json.RawMessage {
	t.Helper()
	var input []json.RawMessage
	if err := json.Unmarshal(frame["input"], &input); err != nil {
		t.Fatalf("decode frame input: %v", err)
	}
	return input
}

func codexWSInputType(t *testing.T, item json.RawMessage) (typ, role, text, output string) {
	t.Helper()
	var v struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(item, &v); err != nil {
		t.Fatalf("decode input item %s: %v", item, err)
	}
	if len(v.Content) > 0 {
		text = v.Content[0].Text
	}
	return v.Type, v.Role, text, v.Output
}

func codexWSToolLoopFirstBody(userID string) string {
	return `{"model":"claude-opus-5-5","max_tokens":40,"stream":true,"metadata":{"user_id":"` + userID + `"},` +
		`"system":"stay stable","tools":[{"name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],` +
		`"messages":[{"role":"user","content":"find the file"}]}`
}

func TestCodexWebsocketToolLoopSendsDeltaAfterFullFrame(t *testing.T) {
	var mu sync.Mutex
	var frames [][]byte
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for i := 0; i < 2; i++ {
			frame := readCodexWSTestFrame(t, conn)
			mu.Lock()
			frames = append(frames, frame)
			mu.Unlock()
			if i == 0 {
				writeCodexWSTestEvents(t, conn,
					`{"type":"response.created","response":{"id":"ws-response-one"}}`,
					`{"type":"response.output_item.added","item":{"id":"tool-item","type":"function_call","call_id":"call-read","name":"Read"}}`,
					`{"type":"response.output_item.done","item":{"id":"tool-item","type":"function_call","call_id":"call-read","name":"Read","arguments":"{\"path\":\"a.txt\"}"}}`,
					`{"type":"response.completed","response":{"id":"ws-response-one"}}`)
			} else {
				writeCodexWSTestEvents(t, conn,
					`{"type":"response.created","response":{"id":"ws-response-two"}}`,
					`{"type":"response.completed","response":{"id":"ws-response-two"}}`)
			}
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	status, first := h.post(t, codexWSToolLoopFirstBody("delta-loop-session"))
	if status != fiber.StatusOK || !strings.Contains(first, `"name":"Read"`) || !strings.Contains(first, `"id":"call-read"`) {
		t.Fatalf("first response status %d, body %q", status, first)
	}
	secondBody := `{"model":"claude-opus-5-5","max_tokens":40,"stream":true,"metadata":{"user_id":"delta-loop-session"},` +
		`"system":"stay stable","tools":[{"name":"Read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],` +
		`"messages":[{"role":"user","content":"find the file"},{"role":"assistant","content":[{"type":"tool_use","id":"call-read","name":"Read","input":{"path":"a.txt"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-read","content":"file contents"},{"type":"text","text":"continue"}]}]}`
	status, second := h.post(t, secondBody)
	if status != fiber.StatusOK || !strings.Contains(second, "message_stop") {
		t.Fatalf("second response status %d, body %q", status, second)
	}
	mu.Lock()
	gotFrames := append([][]byte(nil), frames...)
	mu.Unlock()
	if len(gotFrames) != 2 {
		t.Fatalf("websocket frames = %d, want full plus delta", len(gotFrames))
	}
	firstFrame, secondFrame := decodeCodexWSFrame(t, gotFrames[0]), decodeCodexWSFrame(t, gotFrames[1])
	var prev string
	_ = json.Unmarshal(secondFrame["previous_response_id"], &prev)
	if prev != "ws-response-one" {
		t.Fatalf("second previous_response_id = %q, want completed first response", prev)
	}
	firstInput, delta := codexWSFrameInput(t, firstFrame), codexWSFrameInput(t, secondFrame)
	if len(firstInput) < 2 || len(delta) != 2 {
		t.Fatalf("full input count %d, delta input count %d; want full history and only tool result + new user item", len(firstInput), len(delta))
	}
	if bytes.Contains(gotFrames[1], []byte("find the file")) {
		t.Fatalf("delta frame replayed old prompt history: %s", gotFrames[1])
	}
	typ0, _, _, output := codexWSInputType(t, delta[0])
	typ1, role1, text, _ := codexWSInputType(t, delta[1])
	if typ0 != "function_call_output" || output != "file contents" || typ1 != "message" || role1 != "user" || text != "continue" {
		t.Fatalf("delta items = %s/output=%q then %s/%s text=%q", typ0, output, typ1, role1, text)
	}
	if !strings.Contains(h.access.String(), "transport websocket (delta:") {
		t.Errorf("delta transport missing from access log: %q", h.access.String())
	}
}

func TestCodexWebsocketOffUsesHTTPWithoutUpgrade(t *testing.T) {
	var upgrades, posts atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			upgrades.Add(1)
			w.WriteHeader(http.StatusSwitchingProtocols)
			return
		}
		posts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		okStream(w)
	})
	h := newCodexWSTestHarness(t, backend, "http")
	status, body := h.post(t, `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"flag-off-session"},"messages":[{"role":"user","content":"plain HTTP"}]}`)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q", status, body)
	}
	if upgrades.Load() != 0 || posts.Load() != 1 {
		t.Errorf("backend upgrades=%d POSTs=%d, want 0 and 1", upgrades.Load(), posts.Load())
	}
	if got := currentCodexWSManager().TakeStats(); got != (codex.WSStats{}) {
		t.Errorf("websocket stats after HTTP request = %+v, want zero", got)
	}
}

func TestCodexHTTPWithoutUserIDOmitsSessionCacheKey(t *testing.T) {
	var upgrades, posts atomic.Int32
	var mu sync.Mutex
	var headers http.Header
	var request map[string]json.RawMessage
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			upgrades.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		posts.Add(1)
		mu.Lock()
		headers = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &request)
		mu.Unlock()
		okStream(w)
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	status, body := h.post(t, `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"no cache key"}]}`)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q", status, body)
	}
	mu.Lock()
	gotHeaders, gotRequest := headers.Clone(), request
	mu.Unlock()
	if gotHeaders.Get("session-id") != "" || upgrades.Load() != 0 || posts.Load() != 1 {
		t.Errorf("HTTP session-id=%q websocket upgrades=%d HTTP posts=%d; want omitted, 0 and 1", gotHeaders.Get("session-id"), upgrades.Load(), posts.Load())
	}
	if _, ok := gotRequest["prompt_cache_key"]; ok {
		t.Errorf("HTTP request contains an invented prompt_cache_key: %s", gotRequest["prompt_cache_key"])
	}
}

func TestCodexWebsocketHandshakeRefusalFallsBackToHTTPWithoutLeakingSecrets(t *testing.T) {
	for _, statusCode := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			var upgrades, posts atomic.Int32
			backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					upgrades.Add(1)
					w.Header().Set("X-Codex-Turn-State", codexWSTestTurn)
					w.Header().Set("X-Synthetic-Leak", codexWSTestAccessToken+"/"+codexWSTestAccount)
					w.WriteHeader(statusCode)
					_, _ = io.WriteString(w, "refused synthetic handshake")
					return
				}
				posts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				okStream(w)
			})
			h := newCodexWSTestHarness(t, backend, "websocket")
			status, body := h.post(t, codexWSToolLoopFirstBody("refusal-session"))
			if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
				t.Fatalf("fallback status %d, body %q", status, body)
			}
			if upgrades.Load() != 1 || posts.Load() != 1 {
				t.Errorf("backend upgrades=%d HTTP POSTs=%d, want one each", upgrades.Load(), posts.Load())
			}
			logs := h.access.String() + h.stream.String()
			for _, secret := range []string{codexWSTestAccessToken, codexWSTestRefresh, codexWSTestAccount, codexWSTestTurn} {
				if strings.Contains(logs, secret) {
					t.Errorf("handshake refusal logs leaked %q: %s", secret, logs)
				}
			}
			if !strings.Contains(logs, fmt.Sprintf("fallback to HTTP (handshake HTTP %d)", statusCode)) {
				t.Errorf("fallback reason for HTTP %d missing from logs: %q", statusCode, logs)
			}
		})
	}
}

func TestCodexWebsocketAndHTTPProduceIdenticalAnthropicEvents(t *testing.T) {
	var upgrades, posts atomic.Int32
	events := []string{
		`{"type":"response.created","response":{"id":"parity-response"}}`,
		`{"type":"response.output_item.added","item":{"id":"parity-text","type":"message"}}`,
		`{"type":"response.output_text.delta","item_id":"parity-text","delta":"same answer"}`,
		`{"type":"response.output_item.done","item":{"id":"parity-text","type":"message"}}`,
		`{"type":"response.completed","response":{"id":"parity-response","usage":{"input_tokens":17,"output_tokens":3}}}`,
	}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			posts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range events {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			}
			return
		}
		upgrades.Add(1)
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		_ = readCodexWSTestFrame(t, conn)
		writeCodexWSTestEvents(t, conn, events...)
	})
	body := `{"model":"claude-opus-5-5","max_tokens":20,"stream":true,"metadata":{"user_id":"sse-parity"},"messages":[{"role":"user","content":"compare transports"}]}`
	ws := newCodexWSTestHarness(t, backend, "websocket")
	wsStatus, wsBody := ws.post(t, body)
	httpHarness := newCodexWSTestHarness(t, backend, "http")
	httpStatus, httpBody := httpHarness.post(t, body)
	if wsStatus != fiber.StatusOK || httpStatus != fiber.StatusOK {
		t.Fatalf("statuses: websocket=%d HTTP=%d", wsStatus, httpStatus)
	}
	if upgrades.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("backend websocket upgrades=%d HTTP POSTs=%d; want one per transport", upgrades.Load(), posts.Load())
	}
	if wsEvents, httpEvents := parseStreamEvents(wsBody), parseStreamEvents(httpBody); !reflect.DeepEqual(wsEvents, httpEvents) {
		t.Fatalf("Anthropic events differ\nwebsocket: %#v\nHTTP:      %#v", wsEvents, httpEvents)
	}
}

func TestCodexWebsocketBusyBucketFallsBackToHTTP(t *testing.T) {
	wsAccepted := make(chan struct{}, 1)
	firstFrame := make(chan struct{}, 1)
	hold := make(chan struct{})
	var httpCalls atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			httpCalls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			okStream(w)
			return
		}
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		wsAccepted <- struct{}{}
		_ = readCodexWSTestFrame(t, conn)
		firstFrame <- struct{}{}
		<-hold
		writeCodexWSTestEvents(t, conn,
			`{"type":"response.created","response":{"id":"busy-response"}}`,
			`{"type":"response.completed","response":{"id":"busy-response"}}`)
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"busy-session"},"messages":[{"role":"user","content":"busy request"}]}`
	firstDone := make(chan codexWSTestPostResult, 1)
	go func() { firstDone <- h.postResult(body) }()
	select {
	case <-wsAccepted:
	case <-time.After(5 * time.Second):
		t.Fatal("websocket handshake did not start")
	}
	select {
	case <-firstFrame:
	case <-time.After(5 * time.Second):
		t.Fatal("first websocket request was not sent")
	}
	second := h.postResult(body)
	if second.err != nil || second.status != fiber.StatusOK || !strings.Contains(second.body, "message_stop") {
		close(hold)
		t.Fatalf("busy fallback status=%d err=%v body=%q", second.status, second.err, second.body)
	}
	if httpCalls.Load() != 1 || !strings.Contains(h.access.String(), "fallback to HTTP (connection busy)") {
		t.Errorf("HTTP fallback calls=%d access=%q", httpCalls.Load(), h.access.String())
	}
	close(hold)
	select {
	case result := <-firstDone:
		if result.err != nil || result.status != fiber.StatusOK || !strings.Contains(result.body, "message_stop") {
			t.Errorf("first response status=%d err=%v body=%q", result.status, result.err, result.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first in-flight request did not finish")
	}
}

func TestCodexWebsocketBrokenBeforeContentRetriesFullFrameWithoutHTTP(t *testing.T) {
	oldSlowTotal := slowTotal
	slowTotal = 0
	t.Cleanup(func() { slowTotal = oldSlowTotal })
	var upgrades, posts atomic.Int32
	var framesMu sync.Mutex
	var frames [][]byte
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			posts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			okStream(w)
			return
		}
		attempt := upgrades.Add(1)
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		frame := readCodexWSTestFrame(t, conn)
		framesMu.Lock()
		frames = append(frames, frame)
		framesMu.Unlock()
		if attempt == 1 {
			writeCodexWSTestEvents(t, conn, `{"type":"response.created","response":{"id":"broken-response"}}`)
			_ = conn.CloseNow()
			return
		}
		writeCodexWSTestEvents(t, conn,
			`{"type":"response.created","response":{"id":"retried-response"}}`,
			`{"type":"response.completed","response":{"id":"retried-response"}}`)
	})
	oldBreaks := brokenBackoff
	brokenBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { brokenBackoff = oldBreaks })
	h := newCodexWSTestHarness(t, backend, "websocket")
	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"ws-retry-session"},"system":"retry system","messages":[{"role":"user","content":"retry whole conversation"}]}`
	status, response := h.post(t, body)
	if status != fiber.StatusOK || !strings.Contains(response, "message_stop") || strings.Contains(response, "event: error") {
		t.Fatalf("retry response status=%d body=%q", status, response)
	}
	if upgrades.Load() != 2 || posts.Load() != 0 {
		t.Fatalf("websocket handshakes=%d HTTP POSTs=%d, want two and zero", upgrades.Load(), posts.Load())
	}
	framesMu.Lock()
	got := append([][]byte(nil), frames...)
	framesMu.Unlock()
	if len(got) != 2 {
		t.Fatalf("captured websocket frames=%d, want two", len(got))
	}
	first, retry := decodeCodexWSFrame(t, got[0]), decodeCodexWSFrame(t, got[1])
	if len(codexWSFrameInput(t, retry)) != len(codexWSFrameInput(t, first)) || len(retry["previous_response_id"]) != 0 {
		t.Errorf("retry frame was not a fresh full request: %s", got[1])
	}
	if strings.Contains(h.access.String(), "fallback to HTTP") || !strings.Contains(h.stream.String(), "upstream retry 1/1") {
		t.Errorf("retry log/access log = %q / %q", h.stream.String(), h.access.String())
	}
}

// Once content reached the client, a broken websocket ends the stream with an
// error: no second inference, neither over the websocket nor over HTTP.
func TestCodexWebsocketBrokenAfterContentIsNotAskedAgain(t *testing.T) {
	oldSlowTotal := slowTotal
	slowTotal = 0
	t.Cleanup(func() { slowTotal = oldSlowTotal })
	var upgrades, posts, frames atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			posts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			okStream(w)
			return
		}
		upgrades.Add(1)
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		_ = readCodexWSTestFrame(t, conn)
		frames.Add(1)
		writeCodexWSTestEvents(t, conn,
			`{"type":"response.created","response":{"id":"cut-response"}}`,
			`{"type":"response.output_item.added","item":{"id":"msg-1","type":"message","role":"assistant"}}`,
			`{"type":"response.output_text.delta","item_id":"msg-1","delta":"partial answer"}`)
		_ = conn.CloseNow()
	})
	oldBreaks := brokenBackoff
	brokenBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { brokenBackoff = oldBreaks })
	h := newCodexWSTestHarness(t, backend, "websocket")
	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"ws-cut-session"},"system":"cut system","messages":[{"role":"user","content":"answer at length"}]}`
	_, response := h.post(t, body)
	if !strings.Contains(response, "partial answer") || !strings.Contains(response, "event: error") {
		t.Fatalf("client did not get the partial content and then an error: %q", response)
	}
	if upgrades.Load() != 1 || frames.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("handshakes=%d frames=%d HTTP POSTs=%d after content; want 1, 1, 0 (no second inference)", upgrades.Load(), frames.Load(), posts.Load())
	}
}

func TestCodexWebsocketDisconnectCancelsUpstreamAndLogsCancellation(t *testing.T) {
	upstreamClosed := make(chan struct{}, 1)
	startFlood := make(chan struct{})
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		conn.SetReadLimit(16 << 20)
		defer conn.Close(websocket.StatusNormalClosure, "done")
		_ = readCodexWSTestFrame(t, conn)
		writeCodexWSTestEvents(t, conn,
			`{"type":"response.created","response":{"id":"cancel-response"}}`,
			`{"type":"response.output_item.added","item":{"id":"cancel-text","type":"message"}}`,
			`{"type":"response.output_text.delta","item_id":"cancel-text","delta":"first piece"}`)
		<-startFlood
		largeDelta := strings.Repeat("x", 8<<20)
		for range 8 {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.output_text.delta","item_id":"cancel-text","delta":"`+largeDelta+`"}`)); err != nil {
				upstreamClosed <- struct{}{}
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, err := conn.Read(ctx)
		if err != nil {
			upstreamClosed <- struct{}{}
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = h.app.Listener(listener)
	}()
	h.waitForStream = func() {
		if err := h.app.Shutdown(); err != nil {
			return
		}
		_ = listener.Close()
	}
	t.Cleanup(func() { h.waitForStream() })

	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"disconnect-session"},"messages":[{"role":"user","content":"disconnect me"}]}`
	req, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/call/synthetic-token/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	var received strings.Builder
	deadline := time.After(5 * time.Second)
	for !strings.Contains(received.String(), "event: message_start") {
		select {
		case <-deadline:
			_ = resp.Body.Close()
			t.Fatalf("did not receive initial SSE event: %q", received.String())
		default:
		}
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			_ = resp.Body.Close()
			t.Fatalf("read initial SSE event: %v (%q)", readErr, received.String())
		}
		received.WriteString(line)
	}
	_ = resp.Body.Close()
	close(startFlood)
	select {
	case <-upstreamClosed:
	case <-time.After(10 * time.Second):
		t.Fatal("client disconnect did not cancel the upstream websocket exchange")
	}
	deadlineAt := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.stream.String(), "stream canceled by client") && time.Now().Before(deadlineAt) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(h.stream.String(), "stream canceled by client") {
		t.Errorf("canceled stream log missing: %q", h.stream.String())
	}
	deadlineAt = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadlineAt) {
		promptWatch.mu.Lock()
		active := len(promptWatch.active)
		promptWatch.mu.Unlock()
		if active == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	promptWatch.mu.Lock()
	active := len(promptWatch.active)
	promptWatch.mu.Unlock()
	if active != 0 {
		t.Errorf("stream cancellation did not finish cache-watch cleanup: %d active", active)
	}
}

func TestCodexWebsocketRoutingStateSurvivesReuseAndReconnect(t *testing.T) {
	type handshake struct{ thread, state string }
	handshakes := make(chan handshake, 4)
	var connectionID atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := connectionID.Add(1)
		handshakes <- handshake{thread: r.Header.Get("thread-id"), state: r.Header.Get("x-codex-turn-state")}
		w.Header().Set("X-Codex-Turn-State", []string{"first-handshake-state", "second-handshake-state", "third-handshake-state"}[id-1])
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for exchange := 0; ; exchange++ {
			typ, requestFrame, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var wsPayload struct {
				Type string `json:"type"`
			}
			if typ != websocket.MessageText || json.Unmarshal(requestFrame, &wsPayload) != nil || wsPayload.Type != "response.create" {
				t.Errorf("unexpected routing frame type=%v payload=%s", typ, requestFrame)
			}
			responseID := fmt.Sprintf("route-%d-%d", id, exchange)
			writeCodexWSTestEvents(t, conn,
				`{"type":"response.created","response":{"id":"`+responseID+`"}}`,
				`{"type":"response.output_item.added","item":{"id":"route-tool","type":"function_call","call_id":"tool-1","name":"Read"}}`,
				`{"type":"response.output_item.done","item":{"id":"route-tool","type":"function_call","call_id":"tool-1","name":"Read","arguments":"{}"}}`,
				`{"type":"response.completed","response":{"id":"`+responseID+`"}}`)
			if (id == 1 && exchange == 1) || (id == 2 && exchange == 1) {
				_ = conn.CloseNow()
				return
			}
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	firstBody := turnStateUserBody("routing-reconnect", `""`, "null")
	status, response := h.post(t, firstBody)
	if status != fiber.StatusOK || !strings.Contains(response, "message_stop") {
		t.Fatalf("first request status=%d body=%q", status, response)
	}
	continuation := turnStateContinuationBody("routing-reconnect")
	post := func() {
		t.Helper()
		status, response := h.post(t, continuation)
		if status != fiber.StatusOK || !strings.Contains(response, "message_stop") {
			t.Fatalf("continuation status=%d body=%q", status, response)
		}
	}
	post()
	post()
	post()
	post()

	var got []handshake
	for len(handshakes) > 0 {
		got = append(got, <-handshakes)
	}
	if len(got) != 3 {
		t.Fatalf("handshakes = %+v, want initial, reconnect, and second reconnect", got)
	}
	if got[0].state != "" || got[1].state != "first-handshake-state" || got[2].state != "second-handshake-state" {
		t.Errorf("turn-state handshake sequence = %q, %q, %q", got[0].state, got[1].state, got[2].state)
	}
	if got[0].thread == "" || got[0].thread != got[1].thread || got[1].thread != got[2].thread {
		t.Errorf("thread IDs changed across same bucket reconnects: %+v", got)
	}
}

func TestCodexWebsocketWireDoesNotCreateUploadSample(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		conn.SetReadLimit(2 << 20)
		defer conn.Close(websocket.StatusNormalClosure, "done")
		_ = readCodexWSTestFrame(t, conn)
		writeCodexWSTestEvents(t, conn,
			`{"type":"response.created","response":{"id":"wire-response"}}`,
			`{"type":"response.completed","response":{"id":"wire-response"}}`)
	})
	oldSlowTotal := slowTotal
	slowTotal = 0
	t.Cleanup(func() { slowTotal = oldSlowTotal })
	h := newCodexWSTestHarness(t, backend, "websocket")
	largeText := strings.Repeat("x", uploadMin+1024)
	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"wire-session"},"messages":[{"role":"user","content":"` + largeText + `"}]}`
	status, response := h.post(t, body)
	if status != fiber.StatusOK || !strings.Contains(response, "message_stop") {
		t.Fatalf("status=%d body=%q", status, response)
	}
	stats := linkHealth.Take()
	if !strings.Contains(stats, "1 reply") || strings.Contains(stats, "big body") || strings.Contains(stats, "upload of") {
		t.Errorf("websocket link sample = %q; want reply, no invented HTTP upload", stats)
	}
	if !strings.Contains(h.access.String(), "transport websocket") {
		t.Errorf("websocket transport missing from access log: %q", h.access.String())
	}
}

func TestCodexWebsocketTraceHasNoSyntheticUploadWireSample(t *testing.T) {
	trace := newUpstreamTrace()
	trace.wire = &codex.Wire{Raw: 512 << 10, Sent: 512 << 10, Encoding: "ws"}
	trace.startWSAttempt()
	gotTLS, gotSent, gotWrite := trace.linkSample()
	if gotTLS != 0 || gotSent != 0 || gotWrite != 0 {
		t.Fatalf("websocket link sample = tls %s, sent %d, write %s; want no synthetic upload samples", gotTLS, gotSent, gotWrite)
	}
}

func TestCodexWebsocketStatsCountFormsAndTakeSilence(t *testing.T) {
	var connection atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := connection.Add(1)
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		limit := 1
		if id == 1 {
			limit = 2
		}
		for exchange := 0; exchange < limit; exchange++ {
			frame := readCodexWSTestFrame(t, conn)
			var payload struct {
				Previous string `json:"previous_response_id"`
			}
			_ = json.Unmarshal(frame, &payload)
			if id == 1 && exchange == 1 && payload.Previous != "stats-response-1-0" {
				t.Errorf("delta stats frame previous_response_id=%q", payload.Previous)
			}
			responseID := fmt.Sprintf("stats-response-%d-%d", id, exchange)
			writeCodexWSTestEvents(t, conn,
				`{"type":"response.created","response":{"id":"`+responseID+`"}}`,
				`{"type":"response.completed","response":{"id":"`+responseID+`"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"stats-one"},"messages":[{"role":"user","content":"first stats request"}]}`
	if status, response := h.post(t, body); status != fiber.StatusOK || !strings.Contains(response, "message_stop") {
		t.Fatalf("first stats request status=%d body=%q", status, response)
	}
	var single bytes.Buffer
	writeCodexWebsocketStats(&single, time.Now())
	if got := single.String(); !strings.Contains(got, "1 full") || !strings.Contains(got, "1 dial") || !strings.Contains(got, "0 busy") || !strings.Contains(got, "0 fallbacks") {
		t.Errorf("singular websocket stats line = %q", got)
	}
	body = `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"stats-one"},"messages":[{"role":"user","content":"first stats request"},{"role":"user","content":"second stats request"}]}`
	if status, response := h.post(t, body); status != fiber.StatusOK || !strings.Contains(response, "message_stop") {
		t.Fatalf("delta stats request status=%d body=%q", status, response)
	}
	for _, key := range []string{"stats-two", "stats-three"} {
		body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"` + key + `"},"messages":[{"role":"user","content":"stats"}]}`
		if status, response := h.post(t, body); status != fiber.StatusOK || !strings.Contains(response, "message_stop") {
			t.Fatalf("status=%d body=%q", status, response)
		}
	}
	var full bytes.Buffer
	writeCodexWebsocketStats(&full, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if got := full.String(); !strings.Contains(got, "2 full") || !strings.Contains(got, "1 delta") || !strings.Contains(got, "2 dials") || !strings.Contains(got, "0 busy") || !strings.Contains(got, "0 fallbacks") {
		t.Errorf("plural websocket stats line = %q", got)
	}
	codexWSFallbacks.Store(1)
	var fallback bytes.Buffer
	writeCodexWebsocketStats(&fallback, time.Now())
	if got := fallback.String(); !strings.Contains(got, "1 fallback") || strings.Contains(got, "1 fallbacks") {
		t.Errorf("singular fallback stats line = %q", got)
	}
	var second bytes.Buffer
	writeCodexWebsocketStats(&second, time.Now())
	if second.Len() != 0 {
		t.Errorf("stats emitted again after TakeStats: %q", second.String())
	}
}

func TestCodexWebsocketThreadBucketsIncludeCacheKeySystemAndTools(t *testing.T) {
	type handshake struct{ thread, state string }
	handshakes := make(chan handshake, 8)
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes <- handshake{thread: r.Header.Get("thread-id"), state: r.Header.Get("x-codex-turn-state")}
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for exchange := 0; ; exchange++ {
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
			responseID := fmt.Sprintf("bucket-response-%d", exchange)
			writeCodexWSTestEvents(t, conn,
				`{"type":"response.created","response":{"id":"`+responseID+`"}}`,
				`{"type":"response.completed","response":{"id":"`+responseID+`"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	request := func(userID, system, tool, text string) {
		body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"` + userID + `"},"system":"` + system + `","tools":[{"name":"` + tool + `","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"` + text + `"}]}`
		if status, response := h.post(t, body); status != fiber.StatusOK || !strings.Contains(response, "message_stop") {
			t.Fatalf("bucket request %q status=%d body=%q", text, status, response)
		}
	}
	request("same-user", "system-one", "ToolOne", "one")
	request("same-user", "system-one", "ToolOne", "two")
	request("same-user", "system-two", "ToolOne", "three")
	request("same-user", "system-one", "ToolTwo", "four")
	request("different-user", "system-one", "ToolOne", "five")
	var got []handshake
	for len(handshakes) > 0 {
		got = append(got, <-handshakes)
	}
	if len(got) != 4 {
		t.Fatalf("handshakes = %+v, want initial plus three distinct buckets (same tuple reuses)", got)
	}
	requestBodies := []string{
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"same-user"},"system":"system-one","tools":[{"name":"ToolOne","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"one"}]}`,
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"same-user"},"system":"system-two","tools":[{"name":"ToolOne","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"three"}]}`,
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"same-user"},"system":"system-one","tools":[{"name":"ToolTwo","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"four"}]}`,
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"different-user"},"system":"system-one","tools":[{"name":"ToolOne","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"five"}]}`,
	}
	for i, raw := range requestBodies {
		parsed, err := codex.ParseRequest([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		mapped := converter.MapModel(parsed.Model(), &config.Config{})
		converted, _, err := parsed.Convert(mapped, "codex")
		if err != nil {
			t.Fatal(err)
		}
		want := turnThreadID(turnKey(codex.NewFingerprint(converted)))
		if got[i].thread != want {
			t.Errorf("bucket %d thread ID = %q, want tuple-derived %q", i, got[i].thread, want)
		}
		if got[i].state != "" {
			t.Errorf("bucket %d handshake replayed state %q", i, got[i].state)
		}
	}
}

func TestCodexRetryRequestFallsBackToHTTPAfterWebsocketRetryFails(t *testing.T) {
	var prepared atomic.Int32
	var websocketCalls atomic.Int32
	request := &codex.Request{Model: "gpt-test", Input: []any{"original"}}
	retry := &codexRetry{
		request: request, websocket: true,
		call: func(*codex.Prepared) (*http.Response, error) {
			prepared.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("HTTP fallback"))}, nil
		},
		callWS: func(*codex.Request) (*http.Response, codex.WSInfo, error) {
			websocketCalls.Add(1)
			return nil, codex.WSInfo{}, io.ErrUnexpectedEOF
		},
	}
	trace := newUpstreamTrace()
	trace.wire = &codex.Wire{}
	response, err := retry.requestCall(context.Background(), trace, 100)
	if err != nil || response == nil {
		t.Fatalf("requestCall response=%v err=%v, want HTTP fallback", response, err)
	}
	_ = response.Body.Close()
	second, err := retry.callAgain(context.Background(), trace, 100)
	if err != nil || second == nil {
		t.Fatalf("HTTP-mode retry response=%v err=%v", second, err)
	}
	_ = second.Body.Close()
	if websocketCalls.Load() != 1 || prepared.Load() != 2 || retry.websocket {
		t.Errorf("websocket calls=%d prepared HTTP calls=%d websocket mode=%v; want 1, 2, false", websocketCalls.Load(), prepared.Load(), retry.websocket)
	}
	if got := trace.transport; !strings.Contains(got, "fallback to HTTP") {
		t.Errorf("transport note = %q, want websocket fallback", got)
	}
	if summary := trace.summary(&codex.Translator{}, time.Now(), 100); strings.Count(summary, "fallback to HTTP") != 1 {
		t.Errorf("fallback transport was duplicated in summary: %q", summary)
	}
	if tls, sent, wrote := trace.linkSample(); tls != 0 || sent != 0 || wrote != 0 {
		t.Errorf("HTTP fallback after websocket has wire sample tls=%s sent=%d write=%s", tls, sent, wrote)
	}
}

func TestCodexWebsocketRetryHandshakeFailureFallsBackToHTTP(t *testing.T) {
	var upgrades, posts atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			posts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			okStream(w)
			return
		}
		attempt := upgrades.Add(1)
		if attempt == 2 {
			w.Header().Set("X-Codex-Turn-State", codexWSTestTurn)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "retry websocket refused")
			return
		}
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		_ = readCodexWSTestFrame(t, conn)
		writeCodexWSTestEvents(t, conn, `{"type":"response.created","response":{"id":"pre-retry-break"}}`)
		_ = conn.CloseNow()
	})
	oldBreaks := brokenBackoff
	oldSlowTotal := slowTotal
	brokenBackoff = []time.Duration{time.Millisecond}
	slowTotal = 0
	t.Cleanup(func() { brokenBackoff, slowTotal = oldBreaks, oldSlowTotal })
	h := newCodexWSTestHarness(t, backend, "websocket")
	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"retry-handshake-fallback"},"messages":[{"role":"user","content":"retry after break"}]}`
	status, response := h.post(t, body)
	if status != fiber.StatusOK || !strings.Contains(response, "message_stop") {
		t.Fatalf("fallback status=%d body=%q", status, response)
	}
	if upgrades.Load() != 2 || posts.Load() != 1 {
		t.Errorf("websocket handshakes=%d HTTP posts=%d; want two then one", upgrades.Load(), posts.Load())
	}
	logs := h.access.String() + h.stream.String()
	if !strings.Contains(logs, "fallback to HTTP (handshake HTTP 500)") {
		t.Errorf("failed retry fallback missing from logs: %q", logs)
	}
	if strings.Contains(logs, codexWSTestTurn) {
		t.Errorf("failed retry exposed returned turn state: %q", logs)
	}
}

func TestCodexWebsocketCancellationDoesNotFallbackToHTTP(t *testing.T) {
	var prepared, websocketCalls atomic.Int32
	request := &codex.Request{Model: "gpt-test", Input: []any{"original"}}
	retry := &codexRetry{
		request: request, websocket: true,
		call: func(*codex.Prepared) (*http.Response, error) {
			prepared.Add(1)
			return nil, nil
		},
		callWS: func(*codex.Request) (*http.Response, codex.WSInfo, error) {
			websocketCalls.Add(1)
			return nil, codex.WSInfo{}, context.Canceled
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	trace := newUpstreamTrace()
	trace.wire = &codex.Wire{}
	response, err := retry.requestCall(ctx, trace, 100)
	if !errors.Is(err, context.Canceled) || response != nil {
		t.Fatalf("requestCall response=%v err=%v, want cancellation", response, err)
	}
	if prepared.Load() != 0 || websocketCalls.Load() != 1 || !retry.websocket {
		t.Errorf("HTTP calls=%d websocket calls=%d websocket mode=%v", prepared.Load(), websocketCalls.Load(), retry.websocket)
	}
	if got := trace.transport; !strings.Contains(got, "request canceled") {
		t.Errorf("cancellation transport note = %q", got)
	}
	if summary := trace.summary(&codex.Translator{}, time.Now(), 10); strings.Count(summary, "request canceled") != 1 {
		t.Errorf("cancellation transport duplicated in summary: %q", summary)
	}
}

func TestCodexRetryPreparesOnceOnFirstHTTPAttempt(t *testing.T) {
	request := &codex.Request{Model: "gpt-test", Input: []any{"lazy request"}}
	var calls int
	var first *codex.Prepared
	retry := &codexRetry{request: request}
	retry.call = func(prepared *codex.Prepared) (*http.Response, error) {
		calls++
		if first == nil {
			first = prepared
		} else if first != prepared {
			t.Errorf("HTTP attempt %d got a different prepared request %p, want %p", calls, prepared, first)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}
	if retry.prepared != nil {
		t.Fatal("prepared request created before the first attempt")
	}
	for range 2 {
		resp, err := retry.callPrepared()
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if first == nil || retry.prepared != first || calls != 2 {
		t.Fatalf("prepared=%p retained=%p calls=%d; want lazy single snapshot used twice", first, retry.prepared, calls)
	}
	response, err := retry.callAgain(context.Background(), newUpstreamTrace(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := response.Body.(*stampedBody); !ok {
		t.Fatalf("HTTP retry body type = %T, want timing-stamped body", response.Body)
	}
	_ = response.Body.Close()
}

func TestCodexWebsocketRetryResponseIsStampedForTiming(t *testing.T) {
	trace := newUpstreamTrace()
	trace.wire = &codex.Wire{}
	callCount := 0
	retry := &codexRetry{request: &codex.Request{Model: "gpt-test", Input: []any{"input"}}, websocket: true}
	retry.callWS = func(*codex.Request) (*http.Response, codex.WSInfo, error) {
		callCount++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ws response"))}, codex.WSInfo{Mode: "full", SentBytes: 10}, nil
	}
	ctx := context.Background()
	resp, err := retry.requestCall(ctx, trace, 100)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = retry.callAgain(ctx, trace, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	if trace.read.IsZero() || callCount != 2 {
		t.Fatalf("last read stamp=%v websocket calls=%d", trace.read, callCount)
	}
	_ = resp.Body.Close()
}

// The note that a request fell back to HTTP reaches the access line also when
// the HTTP connection was reused or warmed (no "conn new" part to carry it).
func TestAccessLineKeepsTheFallbackNoteOnAReusedHTTPConnection(t *testing.T) {
	for name, u := range map[string]*upstreamTrace{
		"reused": {calls: 1, reused: true},
		"warm":   {calls: 1, warm: true},
	} {
		u.transport = "fallback to HTTP (connection busy)"
		if got := u.newConnText(); got != "fallback to HTTP (connection busy)" {
			t.Errorf("%s: newConnText = %q", name, got)
		}
	}
	// Without a websocket in play the line is as before.
	if got := (&upstreamTrace{calls: 1, reused: true}).newConnText(); got != "" {
		t.Errorf("plain reused connection: %q", got)
	}
	if got := (&upstreamTrace{}).newConnText(); got != "" {
		t.Errorf("no attempt: %q", got)
	}
}
