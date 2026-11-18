package codexh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

const (
	turnStateA = "Opaque-MiXeD-State/AaZz=="
	turnStateB = "New-State-MiXeD/BBcc=="
	turnStateC = "Retry-Returned-State/CcDd=="
)

func turnStateUserBody(userID, system, tools string) string {
	return fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":%q},"system":%s,"tools":%s,"messages":[{"role":"user","content":"hello"}]}`,
		userID, system, tools)
}

func turnStateContinuationBody(userID string) string {
	return turnStateContinuationWith(userID, `""`, "null")
}

func turnStateContinuationWith(userID, system, tools string) string {
	return fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":%q},"system":%s,"tools":%s,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"done"}]}]}`, userID, system, tools)
}

func turnStateOK(w http.ResponseWriter, token string) {
	if token != "" {
		w.Header().Set("X-Codex-Turn-State", token)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"r"}}`+"\n\n"+
		`data: {"type":"response.completed","response":{"id":"r","usage":{"input_tokens":12000,"output_tokens":1,"input_tokens_details":{"cached_tokens":8000}}}}`+"\n\n")
}

type capturedTurnRequest struct {
	thread string
	id     string
	state  string
}

func TestCodexTurnStateRoutingAndRetry(t *testing.T) {
	oldStates, oldWatch, oldStats := obs.TurnStates, obs.PromptWatch, obs.PromptStats
	obs.TurnStates, obs.PromptWatch, obs.PromptStats = &obs.TurnStateStore{}, &obs.CacheWatch{}, &obs.CacheStats{}
	t.Cleanup(func() { obs.TurnStates, obs.PromptWatch, obs.PromptStats = oldStates, oldWatch, oldStats })
	oldBody, oldSlowTotal := codexTestBody, obs.SlowTotal
	codexTestBody = ""
	obs.SlowTotal = 0
	t.Cleanup(func() { codexTestBody, obs.SlowTotal = oldBody, oldSlowTotal })

	var mu sync.Mutex
	var seen []capturedTurnRequest
	capture := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, capturedTurnRequest{
			thread: r.Header.Get("thread-id"),
			id:     r.Header.Get("x-client-request-id"),
			state:  r.Header.Get("x-codex-turn-state"),
		})
	}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		capture(r)
		turnStateOK(w, turnStateA)
	})

	user := turnStateUserBody("stable-session", `""`, "null")
	status, _, _, stream := callTurnState(t, user, backend)
	if status != fiber.StatusOK || !strings.Contains(stream, "new turn") || !strings.Contains(stream, "turn state returned") || strings.Contains(stream, turnStateA) {
		t.Fatalf("initial user status %d, stream log %q", status, stream)
	}
	first := capturedAt(t, &mu, &seen, 0)
	if first.thread == "" || first.id != first.thread || first.state != "" {
		t.Fatalf("initial routing headers = %+v; want stable thread id as request id and no state", first)
	}

	fastRetries(t, 1)
	var retryCalls atomic.Int32
	retryBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		capture(r)
		if retryCalls.Add(1) == 1 {
			w.Header().Set("X-Codex-Turn-State", turnStateB)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"retry"}}`+"\n\n"+
				`data: {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"overloaded"}}`+"\n\n")
			return
		}
		turnStateOK(w, turnStateC)
	})
	status, _, _, retryLog := callTurnState(t, turnStateContinuationBody("stable-session"), retryBackend)
	if status != fiber.StatusOK || !strings.Contains(retryLog, "upstream retry 1/1") || !strings.Contains(retryLog, "turn state sent") || !strings.Contains(retryLog, "turn state returned") || strings.Contains(retryLog, turnStateA) || strings.Contains(retryLog, turnStateB) || strings.Contains(retryLog, turnStateC) {
		t.Fatalf("continuation with retry status %d, stream log %q", status, retryLog)
	}
	second, third := capturedAt(t, &mu, &seen, 1), capturedAt(t, &mu, &seen, 2)
	for i, got := range []capturedTurnRequest{second, third} {
		if got.thread != first.thread || got.id != first.thread {
			t.Errorf("retry request %d routing ids = %+v, want stable thread %q", i+1, got, first.thread)
		}
	}
	if second.state != turnStateA || third.state != turnStateB {
		t.Fatalf("continuation/retry states = %q, %q; want first and freshly returned state", second.state, third.state)
	}

	changed := []struct {
		label string
		body  string
	}{
		{"key", turnStateContinuationBody("other-session")},
		{"system", turnStateContinuationWith("stable-session", `"different"`, "null")},
		{"tools", turnStateContinuationWith("stable-session", `""`, `[{"name":"Read","input_schema":{"type":"object"}}]`)},
	}
	for i, tc := range changed {
		status, _, _, _ = callTurnState(t, tc.body, backend)
		got := capturedAt(t, &mu, &seen, 3+i)
		if status != fiber.StatusOK || got.state != "" || got.thread == first.thread || got.id != got.thread {
			t.Errorf("changed %s status=%d headers=%+v; want isolated thread and no state", tc.label, status, got)
		}
	}
	status, _, _, _ = callTurnState(t, turnStateContinuationBody("stable-session"), backend)
	if status != fiber.StatusOK || capturedAt(t, &mu, &seen, 6).state != turnStateC {
		t.Errorf("isolated continuation did not preserve original bucket state")
	}

	missingHeaderBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		capture(r)
		turnStateOK(w, "")
	})
	status, _, _, stream = callTurnState(t, user, missingHeaderBackend)
	if status != fiber.StatusOK || !strings.Contains(stream, "new turn") || !strings.Contains(stream, "no turn state returned") || strings.Contains(stream, turnStateA) || strings.Contains(stream, turnStateB) || strings.Contains(stream, turnStateC) {
		t.Errorf("new user status %d, stream log %q", status, stream)
	}
	fourth := capturedAt(t, &mu, &seen, 7)
	if fourth.thread != first.thread || fourth.state != "" {
		t.Errorf("new-turn headers = %+v, want same thread and no stale state", fourth)
	}

	_, by := obs.PromptStats.Take()
	if !strings.Contains(by, "turn state sent: ") || !strings.Contains(by, "turn state not sent: ") {
		t.Errorf("integration cache stats = %q, want sent and not-sent groups", by)
	}
}

func TestCodexTurnStateIsRedactedFrom429Logs(t *testing.T) {
	oldStates := obs.TurnStates
	obs.TurnStates = &obs.TurnStateStore{}
	t.Cleanup(func() { obs.TurnStates = oldStates })
	oldBody, oldSlowTotal := codexTestBody, obs.SlowTotal
	codexTestBody = ""
	obs.SlowTotal = 0
	t.Cleanup(func() { codexTestBody, obs.SlowTotal = oldBody, oldSlowTotal })

	const secret = "Very-Secret-Opaque-Turn-State/DO-NOT-LOG"
	var calls atomic.Int32
	var shortRetryState string
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("X-Codex-Turn-State", secret)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"wait"}}`)
			return
		}
		shortRetryState = r.Header.Get("x-codex-turn-state")
		turnStateOK(w, "")
	})
	status, _, _, stream := callTurnState(t, turnStateUserBody("redaction-session", `""`, "null"), backend)
	if status != fiber.StatusOK || !strings.Contains(stream, "upstream rate limit: retrying") || !strings.Contains(stream, "new turn") || !strings.Contains(stream, "turn state sent") || shortRetryState != secret {
		t.Fatalf("short 429 status %d, retry state %q, stream %q", status, shortRetryState, stream)
	}
	if strings.Contains(stream, secret) {
		t.Fatalf("short-429 log exposed turn state: %q", stream)
	}

	// A later continuation must still use the value returned on the short 429.
	var continuationState string
	var finalAccess, finalStream string
	continuation := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		continuationState = r.Header.Get("x-codex-turn-state")
		w.Header().Set("Retry-After", "120")
		w.Header().Set("X-Codex-Turn-State", secret)
		w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limit"}}`)
	})
	status, _, finalAccess, finalStream = callTurnState(t, turnStateContinuationBody("redaction-session"), continuation)
	if status != fiber.StatusTooManyRequests || continuationState != secret {
		t.Fatalf("final 429 status %d, continuation sent %q; want state from prior attempt", status, continuationState)
	}
	if strings.Contains(finalAccess+finalStream, secret) || !strings.Contains(finalAccess, "x-ratelimit-remaining-requests=0") {
		t.Errorf("final-429 logs access=%q stream=%q; state must be redacted and normal limits retained", finalAccess, finalStream)
	}
}

func callTurnState(t *testing.T, body string, backend http.Handler) (int, string, string, string) {
	t.Helper()
	codexTestBody = body
	status, response, access, stream := codexCall(t, backend, time.Minute)
	return status, response, access, stream
}

func capturedAt(t *testing.T, mu *sync.Mutex, seen *[]capturedTurnRequest, index int) capturedTurnRequest {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if index >= len(*seen) {
		t.Fatalf("captured %d upstream attempts, want attempt %d", len(*seen), index+1)
	}
	return (*seen)[index]
}

// usageLimitEvent is how the backend ends a reply when the plan's limit is used up.
const usageLimitEvent = `{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":324230},"status_code":429}`

func sseBackend(events ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			_, _ = w.Write([]byte("data: " + ev + "\n\n"))
		}
	})
}

// A used-up limit in a non-stream reply is a 429 Claude Code does not send again.
func TestNonStreamUsageLimitIs429WithoutRetry(t *testing.T) {
	nonStreamBody(t)
	status, body, access, _ := codexCall(t, sseBackend(usageLimitEvent), time.Minute)
	if status != fiber.StatusTooManyRequests || !strings.Contains(body, `"rate_limit_error"`) ||
		!strings.Contains(body, "The usage limit has been reached") {
		t.Fatalf("status %d, body %q", status, body)
	}
	if got := lastCodexReply.Get("x-should-retry"); got != "false" {
		t.Errorf("x-should-retry = %q, want false", got)
	}
	if !strings.Contains(access, " 429 - ") {
		t.Errorf("access log = %q", access)
	}
}

// Any other failure of a non-stream reply stays a 502 the client may retry.
func TestNonStreamOtherErrorStays502(t *testing.T) {
	nonStreamBody(t)
	status, body, _, _ := codexCall(t, sseBackend(`{"type":"error","error":{"type":"invalid_request","code":"cyber_policy","message":"flagged"}}`), time.Minute)
	if status != fiber.StatusBadGateway || !strings.Contains(body, "cyber_policy: flagged") {
		t.Fatalf("status %d, body %q", status, body)
	}
	if got := lastCodexReply.Get("x-should-retry"); got != "" {
		t.Errorf("x-should-retry = %q, want none", got)
	}
}

// The limit over HTTP is already a 429; it also tells Claude Code not to retry.
func TestHTTPUsageLimitTellsNotToRetry(t *testing.T) {
	status, _, _, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`))
	}), time.Minute)
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if got := lastCodexReply.Get("x-should-retry"); got != "false" {
		t.Errorf("x-should-retry = %q, want false", got)
	}
}

// A short rate limit stays retryable.
func TestHTTPRateLimitStaysRetryable(t *testing.T) {
	status, _, _, _ := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_exceeded","message":"Rate limit exceeded"}}`))
	}), time.Minute)
	if status != fiber.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if got := lastCodexReply.Get("x-should-retry"); got != "" {
		t.Errorf("x-should-retry = %q, want none", got)
	}
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
