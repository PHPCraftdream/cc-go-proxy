package codexh

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
	"github.com/PHPCraftdream/cc-go-proxy/internal/connpool"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/wsh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/gofiber/fiber/v2"
)

func nonStreamBody(t *testing.T) {
	t.Helper()
	codexTestBody = `{"model":"claude-opus-5-5","max_tokens":10,"stream":false,"messages":[{"role":"user","content":"hi"}]}`
	t.Cleanup(func() { codexTestBody = "" })
}

// A page the edge turned the connection away with is asked again; the address
// the page names reaches the log whole, and the retry gets a fresh connection.
func TestBlockedRouteIsAskedAgainOnAFreshConnection(t *testing.T) {
	fastRetries(t, 2)
	var calls, dials atomic.Int32
	ok := overloadBackend(new(atomic.Int32), 0)
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Cf-Ray", "ray-hel")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<html><body><p>Unable to load site Please try again later. If you are using a VPN, try turning it off. `+
				`Check the status page for information on outages. [IP: 203.0.113.7]</p></body></html>`)
			return
		}
		ok.ServeHTTP(w, r)
	})
	router := connpool.NewRouter(0, 1, 1, func() *http.Transport {
		return &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
	})
	status, body, _, stream := codexCallWith(t, backend, &http.Client{Transport: router}, nil)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q: want the answer of the second call", status, body)
	}
	if calls.Load() != 2 {
		t.Errorf("backend calls = %d, want 2", calls.Load())
	}
	// The block was met on the first connection; the retry must not reuse it.
	if dials.Load() != 2 {
		t.Errorf("connections opened = %d, want 2: the retry needs a new one", dials.Load())
	}
	for _, want := range []string{"upstream retry 1/2", "[IP: 203.0.113.7]", "cf-ray=ray-hel | conn new"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream log lacks %q: %q", want, stream)
		}
	}
}

// The backend's own 403 is JSON: it is the client's to see, not to be asked again.
func TestBackendForbiddenIsNotAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, access, stream := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"not allowed for this account"}}`)
	}), time.Minute)
	if status != fiber.StatusForbidden || !strings.Contains(body, "not allowed for this account") {
		t.Fatalf("status %d, body %q: want the backend's 403", status, body)
	}
	if calls.Load() != 1 || stream != "" {
		t.Errorf("backend calls = %d, log %q: a JSON 403 must not be retried", calls.Load(), stream)
	}
	if !strings.Contains(access, " 403 - ") || !strings.Contains(access, "codex: not allowed for this account") {
		t.Errorf("access log = %q", access)
	}
}

func overloadBackend(calls *atomic.Int32, overloadedCalls int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		created := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"
		if calls.Add(1) <= overloadedCalls {
			_, _ = w.Write([]byte(created + "data: {\"type\":\"error\",\"error\":{\"type\":\"service_unavailable_error\",\"code\":\"server_is_overloaded\",\"message\":\"overloaded\"}}\n\n"))
			return
		}
		_, _ = w.Write([]byte(created +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"hi\"}\n\n" +
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"m\",\"type\":\"message\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n"))
	})
}

func fastRetries(t *testing.T, n int) {
	old, oldBroken := OverloadBackoff, BrokenBackoff
	OverloadBackoff = make([]time.Duration, n)
	BrokenBackoff = make([]time.Duration, n)
	for i := 0; i < n; i++ {
		OverloadBackoff[i], BrokenBackoff[i] = time.Millisecond, time.Millisecond
	}
	t.Cleanup(func() { OverloadBackoff, BrokenBackoff = old, oldBroken })
}

func TestOverloadedBackendIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, _, stream := codexCall(t, overloadBackend(&calls, 1), time.Minute)
	if status != fiber.StatusOK || strings.Contains(body, "event: error") || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q: want a complete stream", status, body)
	}
	if strings.Count(body, "event: message_start") != 1 || !strings.Contains(body, "event: ping") {
		t.Errorf("body %q: want one message_start and a ping", body)
	}
	if calls.Load() != 2 || !strings.Contains(stream, "upstream retry 1/2 in 1ms") || !strings.Contains(stream, "server_is_overloaded") {
		t.Errorf("calls %d, log %q", calls.Load(), stream)
	}
	if strings.Contains(stream, "stream failed") {
		t.Errorf("a retried stream is logged as failed: %q", stream)
	}
}

func TestOverloadRetriesRunOut(t *testing.T) {
	fastRetries(t, 1)
	var calls atomic.Int32
	_, body, _, stream := codexCall(t, overloadBackend(&calls, 10), time.Minute)
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("body %q: want the overload error after the retries", body)
	}
	if calls.Load() != 2 || !strings.Contains(stream, "upstream retry 1/1") || !strings.Contains(stream, "stream failed") {
		t.Errorf("calls %d, log %q", calls.Load(), stream)
	}
}

// lastCodexReply is the header of the reply codexCallWith got, for tests that read it.
var lastCodexReply http.Header

func rateLimitBackend(calls *atomic.Int32, retryAfter string, limited int32) http.Handler {
	ok := overloadBackend(calls, 0)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := calls.Load(); n < limited {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.Header().Set("X-RateLimit-Remaining-Requests", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"Rate limit exceeded"}}`)
			return
		}
		ok.ServeHTTP(w, r)
	})
}

func TestShortRateLimitIsWaitedOut(t *testing.T) {
	var calls atomic.Int32
	status, body, _, stream := codexCall(t, rateLimitBackend(&calls, "0", 1), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q: want the answer of the second call", status, body)
	}
	if calls.Load() != 2 {
		t.Errorf("backend calls = %d, want 2", calls.Load())
	}
	for _, want := range []string{"upstream rate limit: retrying in 0s", "retry-after=0", "x-ratelimit-remaining-requests=0"} {
		if !strings.Contains(stream, want) {
			t.Errorf("log %q lacks %q", stream, want)
		}
	}
}

func TestLongRateLimitGoesToTheClient(t *testing.T) {
	var calls atomic.Int32
	status, body, access, _ := codexCall(t, rateLimitBackend(&calls, "120", 100), time.Minute)
	if status != fiber.StatusTooManyRequests || !strings.Contains(body, "Rate limit exceeded") {
		t.Fatalf("status %d, body %q: want the 429", status, body)
	}
	if calls.Load() != 1 {
		t.Errorf("backend calls = %d: a long Retry-After must not be waited out", calls.Load())
	}
	if got := lastCodexReply.Get("Retry-After"); got != "120" {
		t.Errorf("Retry-After for the client = %q, want 120", got)
	}
	if !strings.Contains(access, "retry-after=120") || !strings.Contains(access, "x-ratelimit-remaining-requests=0") {
		t.Errorf("access log %q lacks the limits", access)
	}
	if strings.Contains(body, "x-ratelimit") {
		t.Errorf("the headers leaked into the client's message: %q", body)
	}
}

func TestRateLimitWithoutRetryAfterIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	status, _, _, _ := codexCall(t, rateLimitBackend(&calls, "", 100), time.Minute)
	if status != fiber.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("status %d after %d calls, want a plain 429 after one", status, calls.Load())
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		v    string
		want time.Duration
		ok   bool
	}{
		{"3", 3 * time.Second, true},
		{"0", 0, true},
		{"1.5", 1500 * time.Millisecond, true},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"-1", 0, false},
		{"soon", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.v != "" {
			h.Set("Retry-After", tc.v)
		}
		got, ok := shared.RetryAfter(h, now)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Retry-After %q = %s, %v; want %s, %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
	h := http.Header{"Retry-After": {"2"}, "X-Ratelimit-Remaining": {"0"}, "X-Codex-Primary-Reset-After-Seconds": {"31"}, "Content-Type": {"x"}}
	if got := shared.RateHeaders(h); got != "retry-after=2 x-codex-primary-reset-after-seconds=31 x-ratelimit-remaining=0" {
		t.Errorf("rateHeaders = %q", got)
	}
}

func TestNotRetriedNote(t *testing.T) {
	if got := notRetriedNote("thinking, text"); got != "not retried: content already sent (thinking, text)" {
		t.Errorf("with content = %q", got)
	}
	if got := notRetriedNote(""); got != "" {
		t.Errorf("without content = %q", got)
	}
}

func TestCodexRetryReleasesPreparedAtFirstContentBlock(t *testing.T) {
	oldBackoff := OverloadBackoff
	OverloadBackoff = []time.Duration{0}
	t.Cleanup(func() { OverloadBackoff = oldBackoff })

	for _, mode := range []string{"stream", "collector"} {
		for _, kind := range []string{"thinking", "text", "tool_use"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				prepared := &codex.Prepared{}
				holder := &wsh.CodexRetry{Prepared: prepared}
				var calls int
				holder.Call = func(got *codex.Prepared) (*http.Response, error) {
					calls++
					if got != prepared {
						t.Fatalf("retry got prepared %p, want original %p", got, prepared)
					}
					if holder.Prepared == nil {
						t.Fatal("prepared request was released before content started")
					}
					body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"
					if calls == 1 {
						body += "data: {\"type\":\"error\",\"error\":{\"type\":\"service_unavailable_error\",\"code\":\"server_is_overloaded\",\"message\":\"overloaded\"}}\n\n"
					} else {
						switch kind {
						case "thinking":
							body += "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"i\",\"type\":\"reasoning\"}}\n\n"
						case "text":
							body += "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"i\",\"type\":\"message\"}}\n\n"
						case "tool_use":
							body += "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"i\",\"type\":\"function_call\",\"call_id\":\"c\",\"name\":\"tool\"}}\n\n"
						}
						body += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\"}}\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				}

				var col *sse.Collector
				var emitted []string
				dest := sse.Emit(func(event string, _ any) {
					if event == "content_block_start" && holder.Prepared != nil {
						t.Errorf("prepared request was not released before emitting content_block_start")
					}
					emitted = append(emitted, event)
				})
				if mode == "collector" {
					col = &sse.Collector{}
					dest = sse.Emit(func(event string, data any) {
						if event == "content_block_start" && holder.Prepared != nil {
							t.Errorf("prepared request was not released before emitting content_block_start")
						}
						col.Emit(event, data)
					})
				}
				tr := &sse.Translator{Model: "model", Emit: holder.Emit(dest)}
				final := runUpstream(context.Background(), tr,
					mustPreparedResponse(t, holder),
					func() (*http.Response, error) { return holder.CallPrepared() }, "profile", "model")
				defer final.Body.Close()

				if calls != 2 {
					t.Fatalf("calls = %d, want overload then retry", calls)
				}
				if holder.Prepared != nil {
					t.Fatal("prepared request remains after first content_block_start")
				}
				if mode == "stream" {
					if !containsString(emitted, "content_block_start") || !containsString(emitted, "message_stop") {
						t.Fatalf("stream events = %v", emitted)
					}
				} else {
					result, err := col.Result()
					if err != nil || len(result["content"].([]any)) != 1 {
						t.Fatalf("collector result = %#v, err %v", result, err)
					}
				}
			})
		}
	}
}

func mustPreparedResponse(t *testing.T, holder *wsh.CodexRetry) *http.Response {
	t.Helper()
	resp, err := holder.CallPrepared()
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestCodexPreparedRequestIsSerializedOnceForAttempts(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	path := filepath.Join(filepath.Dir(file), "codex.go")
	fset := token.NewFileSet()
	fileAST, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var handler *ast.FuncDecl
	for _, decl := range fileAST.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "HandleCodexMessages" {
			handler = fn
			break
		}
	}
	if handler == nil {
		t.Fatal("HandleCodexMessages not found")
	}
	prepares, retryCalls, againCalls, preparedCalls := 0, 0, 0, 0
	capturedReq := false
	ast.Inspect(handler.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "PrepareRequest":
					prepares++
				case "CallPrepared":
					// codex.CallPrepared is the direct call, retry.CallPrepared an attempt.
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "retry" {
						retryCalls++
					} else {
						preparedCalls++
					}
				case "CallAgain":
					againCalls++
				}
			}
		case *ast.FuncLit:
			// No closure of the handler may capture the converted request.
			ast.Inspect(node.Body, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok && id.Name == "req" {
					capturedReq = true
				}
				return true
			})
		}
		return true
	})
	// retry.CallPrepared: the first call, the short rate-limit retry and the edge-error loop;
	// the again closure goes through CallAgain (websocket or HTTP).
	if prepares != 1 || retryCalls != 3 || againCalls != 1 || preparedCalls != 1 || capturedReq {
		t.Fatalf("handler prepares=%d retry calls=%d again calls=%d direct prepared calls=%d closure captures req=%v; want 1, 3, 1, 1, false", prepares, retryCalls, againCalls, preparedCalls, capturedReq)
	}

	closures, closureCalls := 0, 0
	ast.Inspect(fileAST, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok || lit.Type.Params == nil || len(lit.Type.Params.List) != 1 || len(lit.Type.Params.List[0].Names) != 1 || lit.Type.Params.List[0].Names[0].Name != "p" {
			return true
		}
		found := false
		ast.Inspect(lit.Body, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CallPrepared" {
					found = len(call.Args) == 4
					if found {
						arg, ok := call.Args[3].(*ast.Ident)
						found = ok && arg.Name == "p"
					}
					closureCalls++
				}
			}
			if ident, ok := inner.(*ast.Ident); ok && ident.Name == "req" {
				found = false
			}
			return true
		})
		if found {
			closures++
		}
		return false
	})
	if closures != 1 || closureCalls != 1 {
		t.Fatalf("prepared call closure matches=%d, CallPrepared calls=%d; want one explicit prepared argument and no req capture", closures, closureCalls)
	}
}

func TestCodexPreparedRequestReusedAcrossEdgeRateAndOverloadRetries(t *testing.T) {
	fastRetries(t, 1)
	oldStates, oldWatch, oldStats := obs.TurnStates, obs.PromptWatch, obs.PromptStats
	obs.TurnStates, obs.PromptWatch, obs.PromptStats = &obs.TurnStateStore{}, &obs.CacheWatch{}, &obs.CacheStats{}
	t.Cleanup(func() { obs.TurnStates, obs.PromptWatch, obs.PromptStats = oldStates, oldWatch, oldStats })
	oldBody, oldSlowTotal := codexTestBody, obs.SlowTotal
	codexTestBody = ""
	obs.SlowTotal = 0
	t.Cleanup(func() { codexTestBody, obs.SlowTotal = oldBody, oldSlowTotal })

	const userID = "prepared-request-session"
	const state = "Prepared-State-opaque/Keep-MiXeD"
	seedBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Codex-Turn-State", state)
		turnStateOK(w, "")
	})
	status, _, _, _ := callTurnState(t, turnStateUserBody(userID, `""`, "null"), seedBackend)
	if status != fiber.StatusOK {
		t.Fatalf("seed status = %d", status)
	}

	var calls atomic.Int32
	var bodies [][]byte
	var headers []http.Header
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		headers = append(headers, r.Header.Clone())
		switch calls.Add(1) {
		case 1:
			w.Header().Set("X-Codex-Turn-State", state)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `gateway`)
		case 2:
			w.Header().Set("X-Codex-Turn-State", state)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"r"}}`+"\n\n"+
				`data: {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"overloaded"}}`+"\n\n")
		default:
			w.Header().Set("X-Codex-Turn-State", state)
			okStream(w)
		}
	})

	continuation := turnStateContinuationWith(userID, `""`, "null")
	status, body, _, logs := callTurnState(t, continuation, backend)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, response %q", status, body)
	}
	if calls.Load() != 3 || len(bodies) != 3 {
		t.Fatalf("upstream calls = %d, captured bodies = %d, want three", calls.Load(), len(bodies))
	}
	for i := 1; i < len(bodies); i++ {
		if !bytes.Equal(bodies[0], bodies[i]) {
			t.Errorf("attempt %d body differs from first body", i+1)
		}
	}
	for i, h := range headers {
		if h.Get("session-id") == "" || h.Get("x-codex-routing-hint") == "" {
			t.Errorf("attempt %d missing session/routing headers: %v", i+1, h)
		}
		for key, want := range map[string]string{
			"session-id":           headers[0].Get("session-id"),
			"x-codex-routing-hint": headers[0].Get("x-codex-routing-hint"),
			"thread-id":            headers[0].Get("thread-id"),
			"x-client-request-id":  headers[0].Get("x-client-request-id"),
			"x-codex-turn-state":   state,
		} {
			if got := h.Get(key); got != want {
				t.Errorf("attempt %d %s = %q, want %q", i+1, key, got, want)
			}
		}
	}
	if !strings.Contains(logs, "upstream retry 1/1") || !strings.Contains(logs, "server_is_overloaded") {
		t.Errorf("retry log = %q", logs)
	}

	var rateCalls atomic.Int32
	var rateBodies [][]byte
	var rateHeaders []http.Header
	rateBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rateBodies = append(rateBodies, body)
		rateHeaders = append(rateHeaders, r.Header.Clone())
		w.Header().Set("X-Codex-Turn-State", state)
		if rateCalls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		okStream(w)
	})
	status, body, _, rateLogs := callTurnState(t, continuation, rateBackend)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") || rateCalls.Load() != 2 {
		t.Fatalf("429 retry status %d, calls %d, response %q", status, rateCalls.Load(), body)
	}
	if len(rateBodies) != 2 || len(rateHeaders) != 2 {
		t.Fatalf("429 retry captured bodies=%d headers=%d; want two", len(rateBodies), len(rateHeaders))
	}
	if !bytes.Equal(rateBodies[0], rateBodies[1]) {
		t.Error("429 retry request body differs")
	}
	if rateHeaders[0].Get("session-id") == "" || rateHeaders[0].Get("x-codex-routing-hint") == "" || rateHeaders[0].Get("x-codex-turn-state") != state {
		t.Errorf("429 initial routing headers = %v", rateHeaders[0])
	}
	for key, want := range map[string]string{
		"session-id":           rateHeaders[0].Get("session-id"),
		"x-codex-routing-hint": rateHeaders[0].Get("x-codex-routing-hint"),
		"thread-id":            rateHeaders[0].Get("thread-id"),
		"x-client-request-id":  rateHeaders[0].Get("x-client-request-id"),
		"x-codex-turn-state":   rateHeaders[0].Get("x-codex-turn-state"),
	} {
		if first, second := rateHeaders[0].Get(key), rateHeaders[1].Get(key); first != want || second != want {
			t.Errorf("429 retry %s headers = %q, %q; want %q", key, first, second, want)
		}
	}
	if !strings.Contains(rateLogs, "upstream rate limit: retrying") {
		t.Errorf("429 retry log = %q", rateLogs)
	}
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

func TestCodexRetryPreparesOnceOnFirstHTTPAttempt(t *testing.T) {
	request := &codex.Request{Model: "gpt-test", Input: []any{"lazy request"}}
	var calls int
	var first *codex.Prepared
	retry := &wsh.CodexRetry{Request: request}
	retry.Call = func(prepared *codex.Prepared) (*http.Response, error) {
		calls++
		if first == nil {
			first = prepared
		} else if first != prepared {
			t.Errorf("HTTP attempt %d got a different prepared request %p, want %p", calls, prepared, first)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}
	if retry.Prepared != nil {
		t.Fatal("prepared request created before the first attempt")
	}
	for range 2 {
		resp, err := retry.CallPrepared()
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if first == nil || retry.Prepared != first || calls != 2 {
		t.Fatalf("prepared=%p retained=%p calls=%d; want lazy single snapshot used twice", first, retry.Prepared, calls)
	}
	response, err := retry.CallAgain(context.Background(), obs.NewUpstreamTrace(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := response.Body.(*obs.StampedBody); !ok {
		t.Fatalf("HTTP retry body type = %T, want timing-stamped body", response.Body)
	}
	_ = response.Body.Close()
}

// A gateway error before any content is asked again; the client sees the
// answer of the second try.
func TestGatewayErrorIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, body, _, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("upstream connect error or disconnect/reset before headers. reset reason: connection termination"))
			return
		}
		okStream(w)
	}), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("status %d, body %q", status, body)
	}
	if calls.Load() != 2 {
		t.Errorf("backend calls = %d, want 2", calls.Load())
	}
	for _, want := range []string{"upstream retry 1/2 in 1ms [codex] ", "HTTP 503: upstream connect error or disconnect/reset"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q: %q", want, log)
		}
	}
}

// After the retries the gateway's error goes to the client as it is, and the
// log shows the title of an HTML error page, not its markup.
func TestGatewayErrorGivesUpAfterTheRetries(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, _, access, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`<html><head><style>body{font-family:Arial}</style><title>chatgpt.com | 504: Gateway time-out</title></head><body></body></html>`))
	}), time.Minute)
	if status != fiber.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", status)
	}
	if calls.Load() != 3 {
		t.Errorf("backend calls = %d, want 3 (the request and two retries)", calls.Load())
	}
	if !strings.Contains(access, " 504 - ") || !strings.Contains(access, "html error page: chatgpt.com | 504: Gateway time-out") || strings.Contains(access, "font-family") {
		t.Errorf("access log = %q", access)
	}
	if !strings.Contains(log, "upstream retry 1/2") || !strings.Contains(log, "upstream retry 2/2") {
		t.Errorf("log = %q", log)
	}
}

// Only gateway errors are asked again: a 500 is the backend's own answer.
func TestOtherServerErrorsAreNotAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	var calls atomic.Int32
	status, _, _, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}), time.Minute)
	if status != fiber.StatusInternalServerError || calls.Load() != 1 || strings.Contains(log, "upstream retry") {
		t.Errorf("status %d after %d calls, log %q", status, calls.Load(), log)
	}
}

func TestEdgeError(t *testing.T) {
	for status, want := range map[int]bool{200: false, 429: false, 500: false, 501: false, 502: true, 503: true, 504: true} {
		if got := edgeError(status); got != want {
			t.Errorf("edgeError(%d) = %v, want %v", status, got, want)
		}
	}
}

// A reply read to its end (no streaming asked for) is asked again like a
// stream: an overloaded backend before any content is not the client's error.
func TestNonStreamOverloadIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	nonStreamBody(t)
	var calls atomic.Int32
	status, body, _, log := codexCall(t, overloadBackend(&calls, 1), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "\"hi\"") {
		t.Fatalf("status %d, body %q: want the answer of the second try", status, body)
	}
	if calls.Load() != 2 || !strings.Contains(log, "upstream retry 1/2 in 1ms") || !strings.Contains(log, "server_is_overloaded") {
		t.Errorf("calls %d, log %q", calls.Load(), log)
	}
	if strings.Contains(log, "stream failed") {
		t.Errorf("a retried reply is logged as failed: %q", log)
	}
}

// A broken connection before any content is asked again for a reply without
// streaming, too.
func TestNonStreamBrokenReplyIsAskedAgain(t *testing.T) {
	fastRetries(t, 2)
	nonStreamBody(t)
	var calls atomic.Int32
	status, body, _, log := codexCall(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")) // ends early
			return
		}
		overloadBackend(new(atomic.Int32), 0).ServeHTTP(w, r)
	}), time.Minute)
	if status != fiber.StatusOK || !strings.Contains(body, "\"hi\"") || calls.Load() != 2 {
		t.Fatalf("status %d, calls %d, body %q", status, calls.Load(), body)
	}
	if !strings.Contains(log, "upstream retry 1/2") || !strings.Contains(log, "ended before completion") {
		t.Errorf("log %q", log)
	}
}

// When the retries run out the client gets the 502 and the failure is logged
// like a stream's, with the reason and the timing.
func TestNonStreamOverloadRunsOut(t *testing.T) {
	fastRetries(t, 1)
	nonStreamBody(t)
	var calls atomic.Int32
	status, _, access, log := codexCall(t, overloadBackend(&calls, 10), time.Minute)
	if status != fiber.StatusBadGateway || !strings.Contains(access, "server_is_overloaded") || !strings.Contains(access, "| non-stream") {
		t.Fatalf("status %d, access %q: want 502 with the reason and the non-stream note", status, access)
	}
	if calls.Load() != 2 || !strings.Contains(log, "upstream retry 1/1") || !strings.Contains(log, "non-stream reply failed after") {
		t.Errorf("calls %d, log %q", calls.Load(), log)
	}
}
