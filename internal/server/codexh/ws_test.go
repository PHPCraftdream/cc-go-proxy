package codexh

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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	codexws "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/wsh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/rt"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v2"
)

// syncBuf is a log sink the stream-writer goroutine and the test share.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const (
	codexWSTestAccessToken = "synthetic-ws-access-token-never-log"
	codexWSTestRefresh     = "synthetic-ws-refresh-never-log"
	codexWSTestAccount     = "synthetic-ws-account-never-log"
	codexWSTestTurn        = "synthetic-ws-turn-state-never-log"
)

type codexWSTestHarness struct {
	backend       *httptest.Server
	app           *fiber.App
	cfg           *config.Config
	access        *syncBuf
	stream        *syncBuf
	waitForStream func()
}

func newCodexWSTestHarness(t *testing.T, backend http.Handler, transport string) *codexWSTestHarness {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)

	oldManager, oldFallbacks := obs.CodexWSManager, obs.CodexWSFallbacks.Load()
	manager := wsh.NewCodexWSManager()
	obs.CodexWSManagerMu.Lock()
	obs.CodexWSManager = manager
	obs.CodexWSManagerMu.Unlock()
	obs.CodexWSFallbacks.Store(0)

	oldClient, oldLog := codex.HTTPClient, shared.LogOut
	oldTurnStates, oldPromptWatch, oldPromptStats, oldLinkHealth := obs.TurnStates, obs.PromptWatch, obs.PromptStats, obs.LinkHealth
	obs.TurnStates, obs.PromptWatch, obs.PromptStats, obs.LinkHealth = &obs.TurnStateStore{}, &obs.CacheWatch{}, &obs.CacheStats{}, &obs.LinkStats{}
	codex.HTTPClient = &http.Client{Transport: &http.Transport{}}
	streamLog := &syncBuf{}
	shared.LogOut = streamLog
	var harness *codexWSTestHarness
	t.Cleanup(func() {
		if harness != nil && harness.waitForStream != nil {
			harness.waitForStream()
		}
		manager.Close()
		obs.CodexWSManagerMu.Lock()
		obs.CodexWSManager = oldManager
		obs.CodexWSManagerMu.Unlock()
		obs.CodexWSFallbacks.Store(oldFallbacks)
		codex.HTTPClient.CloseIdleConnections()
		codex.HTTPClient, shared.LogOut = oldClient, oldLog
		obs.TurnStates, obs.PromptWatch, obs.PromptStats, obs.LinkHealth = oldTurnStates, oldPromptWatch, oldPromptStats, oldLinkHealth
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
	app.Use(rt.AccessLog(accessLogWriter))
	app.Post("/call/:token/v1/messages", func(c *fiber.Ctx) error {
		return HandleCodexMessages(c, cfg, session)
	})
	harness = &codexWSTestHarness{backend: server, app: app, cfg: cfg, access: accessLogWriter, stream: streamLog}
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
	if got := wsh.CurrentCodexWSManager().TakeStats(); got != (codexws.WSStats{}) {
		t.Errorf("websocket stats after HTTP request = %+v, want zero", got)
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
	if !regexp.MustCompile(`fallback to HTTP \(connection busy\)[^\n]*\| conv [0-9a-f]{6}\.[0-9a-f]{4} \| request \d`).MatchString(h.access.String()) {
		t.Errorf("busy fallback line lacks conv and size: %q", h.access.String())
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
	oldSlowTotal := obs.SlowTotal
	obs.SlowTotal = 0
	t.Cleanup(func() { obs.SlowTotal = oldSlowTotal })
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
	oldBreaks := BrokenBackoff
	BrokenBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { BrokenBackoff = oldBreaks })
	h := newCodexWSTestHarness(t, backend, "websocket")
	obs.LinkHealth.Take() // start from nothing
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
	// A break the retry hid from the client still shows in the link line.
	if link := obs.LinkHealth.Take(); !strings.Contains(link, "1 broken call (1 sent again by ccgp, 0 reached the client): 1 "+obs.CauseWSEnded) {
		t.Errorf("link line after a retried websocket break = %q", link)
	}
}

// Once content reached the client, a broken websocket ends the stream with an
// error: no second inference, neither over the websocket nor over HTTP.
func TestCodexWebsocketBrokenAfterContentIsNotAskedAgain(t *testing.T) {
	oldSlowTotal := obs.SlowTotal
	obs.SlowTotal = 0
	t.Cleanup(func() { obs.SlowTotal = oldSlowTotal })
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
	oldBreaks := BrokenBackoff
	BrokenBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { BrokenBackoff = oldBreaks })
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
		obs.PromptWatch.Mu.Lock()
		active := len(obs.PromptWatch.Active)
		obs.PromptWatch.Mu.Unlock()
		if active == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	obs.PromptWatch.Mu.Lock()
	active := len(obs.PromptWatch.Active)
	obs.PromptWatch.Mu.Unlock()
	if active != 0 {
		t.Errorf("stream cancellation did not finish cache-watch cleanup: %d active", active)
	}
}

func TestCodexRetryRequestFallsBackToHTTPAfterWebsocketRetryFails(t *testing.T) {
	var prepared atomic.Int32
	var websocketCalls atomic.Int32
	request := &codex.Request{Model: "gpt-test", Input: []any{"original"}}
	retry := &wsh.CodexRetry{
		Request: request, Websocket: true,
		Call: func(*codex.Prepared) (*http.Response, error) {
			prepared.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("HTTP fallback"))}, nil
		},
		CallWS: func(*codex.Request) (*http.Response, codexws.WSInfo, error) {
			websocketCalls.Add(1)
			return nil, codexws.WSInfo{}, io.ErrUnexpectedEOF
		},
	}
	trace := obs.NewUpstreamTrace()
	trace.Wire = &codex.Wire{}
	response, err := retry.RequestCall(context.Background(), trace, 100)
	if err != nil || response == nil {
		t.Fatalf("requestCall response=%v err=%v, want HTTP fallback", response, err)
	}
	_ = response.Body.Close()
	second, err := retry.CallAgain(context.Background(), trace, 100)
	if err != nil || second == nil {
		t.Fatalf("HTTP-mode retry response=%v err=%v", second, err)
	}
	_ = second.Body.Close()
	if websocketCalls.Load() != 1 || prepared.Load() != 2 || retry.Websocket {
		t.Errorf("websocket calls=%d prepared HTTP calls=%d websocket mode=%v; want 1, 2, false", websocketCalls.Load(), prepared.Load(), retry.Websocket)
	}
	if got := trace.Transport; !strings.Contains(got, "fallback to HTTP") {
		t.Errorf("transport note = %q, want websocket fallback", got)
	}
	if summary := trace.Summary(&sse.Translator{}, time.Now(), 100); strings.Count(summary, "fallback to HTTP") != 1 {
		t.Errorf("fallback transport was duplicated in summary: %q", summary)
	}
	if tls, sent, wrote := trace.LinkSample(); tls != 0 || sent != 0 || wrote != 0 {
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
	oldBreaks := BrokenBackoff
	oldSlowTotal := obs.SlowTotal
	BrokenBackoff = []time.Duration{time.Millisecond}
	obs.SlowTotal = 0
	t.Cleanup(func() { BrokenBackoff, obs.SlowTotal = oldBreaks, oldSlowTotal })
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
	retry := &wsh.CodexRetry{
		Request: request, Websocket: true,
		Call: func(*codex.Prepared) (*http.Response, error) {
			prepared.Add(1)
			return nil, nil
		},
		CallWS: func(*codex.Request) (*http.Response, codexws.WSInfo, error) {
			websocketCalls.Add(1)
			return nil, codexws.WSInfo{}, context.Canceled
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	trace := obs.NewUpstreamTrace()
	trace.Wire = &codex.Wire{}
	response, err := retry.RequestCall(ctx, trace, 100)
	if !errors.Is(err, context.Canceled) || response != nil {
		t.Fatalf("requestCall response=%v err=%v, want cancellation", response, err)
	}
	if prepared.Load() != 0 || websocketCalls.Load() != 1 || !retry.Websocket {
		t.Errorf("HTTP calls=%d websocket calls=%d websocket mode=%v", prepared.Load(), websocketCalls.Load(), retry.Websocket)
	}
	if got := trace.Transport; !strings.Contains(got, "request canceled") {
		t.Errorf("cancellation transport note = %q", got)
	}
	if summary := trace.Summary(&sse.Translator{}, time.Now(), 10); strings.Count(summary, "request canceled") != 1 {
		t.Errorf("cancellation transport duplicated in summary: %q", summary)
	}
}

func codexWSSideBody(userID string, messages string) string {
	return `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"` + userID + `"},"messages":[` + messages + `]}`
}

func codexWSSideUser(text string) string { return `{"role":"user","content":"` + text + `"}` }

func codexWSSideAssistant(text string) string { return `{"role":"assistant","content":"` + text + `"}` }

// A background request of the client (a short fixed message after the same
// history) answered on the websocket replaces its chain; once ccgp has seen
// that, requests ending in the message go over HTTP and the conversation's
// chain survives them.
func TestCodexWebsocketBackgroundRequestsGoOverHTTPOnceLearned(t *testing.T) {
	var frames, posts atomic.Int32
	answers := []string{"a1", "s1", "a2", "a3"}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			posts.Add(1)
			okStream(w)
			return
		}
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		for _, answer := range answers {
			readCodexWSTestFrame(t, conn)
			id := "side-response-" + answer
			frames.Add(1)
			writeCodexWSTestEvents(t, conn,
				`{"type":"response.created","response":{"id":"`+id+`"}}`,
				`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`+answer+`"}]}}`,
				`{"type":"response.completed","response":{"id":"`+id+`"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	post := func(messages string) {
		t.Helper()
		if status, body := h.post(t, codexWSSideBody("side-session", messages)); status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
			t.Fatalf("status %d, body %q", status, body)
		}
	}
	main, a1, side := codexWSSideUser("main question"), codexWSSideAssistant("a1"), codexWSSideUser("background template")

	post(main)
	post(strings.Join([]string{main, a1, side}, ","))
	post(strings.Join([]string{main, a1, codexWSSideUser("real next")}, ","))
	if frames.Load() != 3 || posts.Load() != 0 {
		t.Fatalf("before learning: %d websocket frames and %d HTTP posts, want 3 and 0", frames.Load(), posts.Load())
	}
	if !strings.Contains(h.stream.String(), "requests ending in it go over HTTP from now on") {
		t.Errorf("the learned message was not announced: %q", h.stream.String())
	}

	post(strings.Join([]string{main, a1, codexWSSideUser("real next"), codexWSSideAssistant("a2"), side}, ","))
	if frames.Load() != 3 || posts.Load() != 1 {
		t.Fatalf("background request: %d websocket frames and %d HTTP posts, want 3 and 1", frames.Load(), posts.Load())
	}
	if !strings.Contains(h.access.String(), "transport http (side request)") {
		t.Errorf("access log lacks the side request note: %q", h.access.String())
	}
	// The line names the conversation and agent and the size, so the cost is told per agent.
	if !regexp.MustCompile(`transport http \(side request\)[^\n]*\| conv [0-9a-f]{6}\.[0-9a-f]{4} \| request \d`).MatchString(h.access.String()) {
		t.Errorf("side request line lacks conv and size: %q", h.access.String())
	}
	var stats strings.Builder
	wsh.WriteCodexWebsocketStats(&stats, time.Now())
	if !strings.Contains(stats.String(), "1 background request sent over HTTP") {
		t.Errorf("periodic line = %q", stats.String())
	}

	// The next request keeps the message, with a reply: it was an ordinary turn,
	// so it is forgotten and the request goes on the websocket.
	post(strings.Join([]string{main, a1, codexWSSideUser("real next"), codexWSSideAssistant("a2"), side, codexWSSideAssistant("r"), codexWSSideUser("go on")}, ","))
	if frames.Load() != 4 || posts.Load() != 1 {
		t.Fatalf("after the ordinary turn: %d websocket frames and %d HTTP posts, want 4 and 1", frames.Load(), posts.Load())
	}
	if !regexp.MustCompile(`upstream websocket \[codex\] conv [0-9a-f]{6}\.[0-9a-f]{4}: a user message of \d+ B taken for a background request's stayed in the next request, with its reply \(an ordinary turn\); it is no longer taken for one \(1 request had gone over HTTP for it\)`).MatchString(h.stream.String()) {
		t.Errorf("the forgotten message was not announced: %q", h.stream.String())
	}
	stats.Reset()
	wsh.WriteCodexWebsocketStats(&stats, time.Now())
	if !strings.Contains(stats.String(), "1 message taken for a background request's found to be an ordinary turn's") {
		t.Errorf("periodic line = %q", stats.String())
	}
}

// The subagents of a conversation share its cache key, system prompt and tools;
// each keeps its own websocket, so one does not replace the other's chain.
func TestCodexWebsocketAgentsOfOneConversationHaveTheirOwnConnection(t *testing.T) {
	var mu sync.Mutex
	var conns atomic.Int32
	var previous []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn := acceptCodexWSTest(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "done")
		id := conns.Add(1)
		for exchange := 0; exchange < 2; exchange++ {
			frame := readCodexWSTestFrame(t, conn)
			var payload struct {
				Previous string `json:"previous_response_id"`
			}
			_ = json.Unmarshal(frame, &payload)
			mu.Lock()
			previous = append(previous, payload.Previous)
			mu.Unlock()
			responseID := fmt.Sprintf("agent-%d-%d", id, exchange)
			writeCodexWSTestEvents(t, conn,
				`{"type":"response.created","response":{"id":"`+responseID+`"}}`,
				`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"r"}]}}`,
				`{"type":"response.completed","response":{"id":"`+responseID+`"}}`)
		}
	})
	h := newCodexWSTestHarness(t, backend, "websocket")
	post := func(messages ...string) {
		t.Helper()
		if status, body := h.post(t, codexWSSideBody("agents-session", strings.Join(messages, ","))); status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
			t.Fatalf("status %d, body %q", status, body)
		}
	}
	taskA, taskB, reply := codexWSSideUser("task of agent A"), codexWSSideUser("task of agent B"), codexWSSideAssistant("r")
	post(taskA)
	post(taskB)
	post(taskA, reply, codexWSSideUser("more for A"))
	post(taskB, reply, codexWSSideUser("more for B"))

	mu.Lock()
	got := append([]string(nil), previous...)
	mu.Unlock()
	if want := []string{"", "", "agent-1-0", "agent-2-0"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("previous_response_id of the frames = %q, want %q (full, full, then a delta on each agent's own connection)", got, want)
	}
	if conns.Load() != 2 {
		t.Errorf("handshakes = %d, want one per agent", conns.Load())
	}
	tags := regexp.MustCompile(`conv ([0-9a-f]{6})\.([0-9a-f]{4})`).FindAllStringSubmatch(h.access.String(), -1)
	if len(tags) != 4 || tags[0][1] != tags[1][1] || tags[0][2] == tags[1][2] || tags[0][2] != tags[2][2] || tags[1][2] != tags[3][2] {
		t.Errorf("conversation tags in the access log = %q, want one thread and two agent tags", tags)
	}
}

func okStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range []string{
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.completed","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":1}}}`,
	} {
		_, _ = w.Write([]byte("data: " + e + "\n\n"))
	}
}

func parseStreamEvents(body string) []streamEvent {
	var events []streamEvent
	for _, eventText := range strings.Split(body, "\n\n") {
		var event streamEvent
		var data string
		for _, line := range strings.Split(eventText, "\n") {
			if strings.HasPrefix(line, "event: ") {
				event.name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event.name != "" && data != "" {
			if err := json.Unmarshal([]byte(data), &event.data); err == nil {
				events = append(events, event)
			}
		}
	}
	return events
}

type streamEvent struct {
	name string
	data map[string]interface{}
}
