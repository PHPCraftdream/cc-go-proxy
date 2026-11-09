package server

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/gofiber/fiber/v2"
)

func TestCodexRetryReleasesPreparedAtFirstContentBlock(t *testing.T) {
	oldBackoff := overloadBackoff
	overloadBackoff = []time.Duration{0}
	t.Cleanup(func() { overloadBackoff = oldBackoff })

	for _, mode := range []string{"stream", "collector"} {
		for _, kind := range []string{"thinking", "text", "tool_use"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				prepared := &codex.Prepared{}
				holder := &codexRetry{prepared: prepared}
				var calls int
				holder.call = func(got *codex.Prepared) (*http.Response, error) {
					calls++
					if got != prepared {
						t.Fatalf("retry got prepared %p, want original %p", got, prepared)
					}
					if holder.prepared == nil {
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

				var col *codex.Collector
				var emitted []string
				dest := codex.Emit(func(event string, _ any) {
					if event == "content_block_start" && holder.prepared != nil {
						t.Errorf("prepared request was not released before emitting content_block_start")
					}
					emitted = append(emitted, event)
				})
				if mode == "collector" {
					col = &codex.Collector{}
					dest = codex.Emit(func(event string, data any) {
						if event == "content_block_start" && holder.prepared != nil {
							t.Errorf("prepared request was not released before emitting content_block_start")
						}
						col.Emit(event, data)
					})
				}
				tr := &codex.Translator{Model: "model", Emit: holder.emit(dest)}
				final := runUpstream(context.Background(), tr,
					mustPreparedResponse(t, holder),
					func() (*http.Response, error) { return holder.callPrepared() }, "profile", "model")
				defer final.Body.Close()

				if calls != 2 {
					t.Fatalf("calls = %d, want overload then retry", calls)
				}
				if holder.prepared != nil {
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

func mustPreparedResponse(t *testing.T, holder *codexRetry) *http.Response {
	t.Helper()
	resp, err := holder.callPrepared()
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
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "handleCodexMessages" {
			handler = fn
			break
		}
	}
	if handler == nil {
		t.Fatal("handleCodexMessages not found")
	}
	prepares, retryCalls, preparedCalls := 0, 0, 0
	var preparePos, nilReqPos token.Pos
	ast.Inspect(handler.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "PrepareRequest":
					prepares++
					preparePos = node.Pos()
				case "CallPrepared":
					preparedCalls++
				}
				if sel.Sel.Name == "callPrepared" {
					retryCalls++
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name != "req" || i >= len(node.Rhs) {
					continue
				}
				if value, ok := node.Rhs[i].(*ast.Ident); ok && value.Name == "nil" {
					nilReqPos = node.Pos()
				}
			}
		}
		return true
	})
	if prepares != 1 || retryCalls != 4 || preparedCalls != 1 || nilReqPos <= preparePos {
		t.Fatalf("handler prepares=%d retry calls=%d direct prepared calls=%d req-nil position=%v; want 1, 4, 1, and after preparation", prepares, retryCalls, preparedCalls, nilReqPos > preparePos)
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
	oldStates, oldWatch, oldStats := turnStates, promptWatch, promptStats
	turnStates, promptWatch, promptStats = &turnStateStore{}, &cacheWatch{}, &cacheStats{}
	t.Cleanup(func() { turnStates, promptWatch, promptStats = oldStates, oldWatch, oldStats })
	oldBody, oldSlowTotal := codexTestBody, slowTotal
	codexTestBody = ""
	slowTotal = 0
	t.Cleanup(func() { codexTestBody, slowTotal = oldBody, oldSlowTotal })

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
