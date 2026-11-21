package codexh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	codexws "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws"
	wschain "github.com/PHPCraftdream/cc-go-proxy/internal/codex/ws/chain"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/codexh/wsh"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/obs"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server/shared"
	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v2"
)

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
	// The conversation's first user message is the agent's key: the continuation keeps it.
	continuation := strings.Replace(turnStateContinuationBody("routing-reconnect"), `"messages":[`, `"messages":[{"role":"user","content":"hello"},`, 1)
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
	oldSlowTotal := obs.SlowTotal
	obs.SlowTotal = 0
	t.Cleanup(func() { obs.SlowTotal = oldSlowTotal })
	h := newCodexWSTestHarness(t, backend, "websocket")
	largeText := strings.Repeat("x", obs.UploadMin+1024)
	body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"wire-session"},"messages":[{"role":"user","content":"` + largeText + `"}]}`
	status, response := h.post(t, body)
	if status != fiber.StatusOK || !strings.Contains(response, "message_stop") {
		t.Fatalf("status=%d body=%q", status, response)
	}
	stats := obs.LinkHealth.Take()
	if !strings.Contains(stats, "1 reply") || strings.Contains(stats, "big body") || strings.Contains(stats, "upload of") {
		t.Errorf("websocket link sample = %q; want reply, no invented HTTP upload", stats)
	}
	if !strings.Contains(h.access.String(), "transport websocket") {
		t.Errorf("websocket transport missing from access log: %q", h.access.String())
	}
}

func TestCodexWebsocketTraceHasNoSyntheticUploadWireSample(t *testing.T) {
	trace := obs.NewUpstreamTrace()
	trace.Wire = &codex.Wire{Raw: 512 << 10, Sent: 512 << 10, Encoding: "ws"}
	trace.StartWSAttempt()
	gotTLS, gotSent, gotWrite := trace.LinkSample()
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
	wsh.WriteCodexWebsocketStats(&single, time.Now())
	if got := single.String(); !strings.Contains(got, "1 full") || !strings.Contains(got, "1 dial") || !strings.Contains(got, "0 busy") || !strings.Contains(got, "0 HTTP fallbacks") {
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
	wsh.WriteCodexWebsocketStats(&full, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if got := full.String(); !strings.Contains(got, "2 full") || !strings.Contains(got, "1 delta") || !strings.Contains(got, "2 dials") || !strings.Contains(got, "0 busy") || !strings.Contains(got, "0 HTTP fallbacks") {
		t.Errorf("plural websocket stats line = %q", got)
	}
	obs.CodexWSFallbacks.Store(1)
	var fallback bytes.Buffer
	wsh.WriteCodexWebsocketStats(&fallback, time.Now())
	if got := fallback.String(); !strings.Contains(got, "1 HTTP fallback,") {
		t.Errorf("singular fallback stats line = %q", got)
	}
	var second bytes.Buffer
	wsh.WriteCodexWebsocketStats(&second, time.Now())
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
		// Every request starts with the same user message (the agent's key); text follows it.
		body := `{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"` + userID + `"},"system":"` + system + `","tools":[{"name":"` + tool + `","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"start"},{"role":"user","content":"` + text + `"}]}`
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
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"same-user"},"system":"system-one","tools":[{"name":"ToolOne","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"start"},{"role":"user","content":"one"}]}`,
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"same-user"},"system":"system-two","tools":[{"name":"ToolOne","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"start"},{"role":"user","content":"three"}]}`,
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"same-user"},"system":"system-one","tools":[{"name":"ToolTwo","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"start"},{"role":"user","content":"four"}]}`,
		`{"model":"claude-opus-5-5","max_tokens":10,"stream":true,"metadata":{"user_id":"different-user"},"system":"system-one","tools":[{"name":"ToolOne","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"start"},{"role":"user","content":"five"}]}`,
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
		want := obs.TurnThreadID(obs.TurnKey(codex.NewFingerprint(converted)))
		if got[i].thread != want {
			t.Errorf("bucket %d thread ID = %q, want tuple-derived %q", i, got[i].thread, want)
		}
		if got[i].state != "" {
			t.Errorf("bucket %d handshake replayed state %q", i, got[i].state)
		}
	}
}

func TestCodexWebsocketRetryResponseIsStampedForTiming(t *testing.T) {
	trace := obs.NewUpstreamTrace()
	trace.Wire = &codex.Wire{}
	callCount := 0
	retry := &wsh.CodexRetry{Request: &codex.Request{Model: "gpt-test", Input: []any{"input"}}, Websocket: true}
	retry.CallWS = func(*codex.Request) (*http.Response, codexws.WSInfo, error) {
		callCount++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ws response"))}, codexws.WSInfo{Mode: "full", SentBytes: 10}, nil
	}
	ctx := context.Background()
	resp, err := retry.RequestCall(ctx, trace, 100)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = retry.CallAgain(ctx, trace, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	if trace.Read.IsZero() || callCount != 2 {
		t.Fatalf("last read stamp=%v websocket calls=%d", trace.Read, callCount)
	}
	_ = resp.Body.Close()
}

// The note that a request fell back to HTTP reaches the access line also when
// the HTTP connection was reused or warmed (no "conn new" part to carry it).
func TestAccessLineKeepsTheFallbackNoteOnAReusedHTTPConnection(t *testing.T) {
	for name, u := range map[string]*obs.UpstreamTrace{
		"reused": {Calls: 1, Reused: true},
		"warm":   {Calls: 1, Warm: true},
	} {
		u.Transport = "fallback to HTTP (connection busy)"
		if got := u.NewConnText(); got != "fallback to HTTP (connection busy)" {
			t.Errorf("%s: newConnText = %q", name, got)
		}
	}
	// The conversation and size of the request follow the fallback note.
	u := &obs.UpstreamTrace{Calls: 1, Reused: true, Conv: "3fa2c1.9b0e"}
	u.NoteWSFallback("connection busy", 2<<20)
	defer obs.CodexWSFallbacks.Add(-1)
	if got, want := u.NewConnText(), "fallback to HTTP (connection busy) | conv 3fa2c1.9b0e | request 2.0 MiB"; got != want {
		t.Errorf("fallback with conv: %q, want %q", got, want)
	}
	u.NoteSideRequest(300)
	if got, want := u.NewConnText(), "transport http (side request) | conv 3fa2c1.9b0e | request 300 B"; got != want {
		t.Errorf("side request with conv: %q, want %q", got, want)
	}
	// Without a conversation (no thread id) only the size is named.
	bare := &obs.UpstreamTrace{Calls: 1, Reused: true}
	bare.NoteSideRequest(300)
	if got, want := bare.NewConnText(), "transport http (side request) | request 300 B"; got != want {
		t.Errorf("side request without conv: %q, want %q", got, want)
	}
	// Without a websocket in play the line is as before.
	if got := (&obs.UpstreamTrace{Calls: 1, Reused: true}).NewConnText(); got != "" {
		t.Errorf("plain reused connection: %q", got)
	}
	if got := (&obs.UpstreamTrace{}).NewConnText(); got != "" {
		t.Errorf("no attempt: %q", got)
	}
}

func TestSafeWSReasonKeepsNumbersAndToolNamesOnly(t *testing.T) {
	for reason, want := range map[string]string{
		"lowered age limit":                    "lowered age limit",
		"history diverged at item 412 of 1079": "history diverged at item 412 of 1079",
		"history diverged at item 0":           "history diverged at item 0",
		"history diverged at item x of y":      "history diverged",
		"tool call changed (arguments, Read)":  "tool call changed (arguments, Read)",
		"tool call changed (arguments, Edit, was 312 B, now 340 B, first 120 B and last 40 B equal)": "tool call changed (arguments, Edit, was 312 B, now 340 B, first 120 B and last 40 B equal)",
		"tool call changed (arguments, Edit, was x B, now 340 B, first 120 B and last 40 B equal)":   "full request",
		"tool call changed (arguments, Edit, was 312 B, now 340 B, first 1 B and last 40 B equal) x": "full request",
		"tool call changed (id, mcp__srv__do-it.v2:x)":                                               "tool call changed (id, mcp__srv__do-it.v2:x)",
		"tool call changed (name, ?)":                    "tool call changed (name, ?)",
		"tool call changed (secret, Read)":               "full request",
		"tool call changed (arguments, rm -rf /)":        "full request",
		"tool call changed (arguments, Bash\nforged)":    "full request",
		"something the backend said, with a token abc1":  "full request",
		"assistant message missing (found user message)": "assistant message missing (found user message)",
		"tool call missing (found reasoning)":            "tool call missing (found reasoning)",
		"new item not sendable (assistant message)":      "new item not sendable (assistant message)",
		"output item not compared (web_search_call)":     "output item not compared (web_search_call)",
		"output item not compared (Secret Text)":         "full request",
		"output unreadable":                              "output unreadable",
		"output tool call without id":                    "output tool call without id",
	} {
		if got := obs.SafeWSReason(reason); got != want {
			t.Errorf("safeWSReason(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestWSReasonTextNamesTheLoweredAge(t *testing.T) {
	info := codexws.WSInfo{Reason: "lowered age limit", Limit: 20 * time.Minute}
	if got, want := obs.WsReasonText(info), "replaced at the lowered age of 20m0s"; got != want {
		t.Errorf("wsReasonText = %q, want %q", got, want)
	}
	if got := obs.WsReasonText(codexws.WSInfo{Reason: "lowered age limit"}); got != "lowered age limit" {
		t.Errorf("without a known age: %q", got)
	}
	if got := obs.WsReasonText(codexws.WSInfo{Reason: "connection broken"}); got != "connection broken" {
		t.Errorf("other reasons are unchanged: %q", got)
	}
}

func TestLogWSLimitLine(t *testing.T) {
	old := shared.LogOut
	var buf bytes.Buffer
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })
	wsh.LogWSLimit(codexws.WSLimit{Age: 20 * time.Minute, Was: 25 * time.Minute, Broke: 12*time.Minute + 5*time.Second, Status: 1001})
	wsh.LogWSLimit(codexws.WSLimit{Age: 15 * time.Minute, Was: 20 * time.Minute, Broke: 11 * time.Minute, Status: -1})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	for i, tail := range []string{
		"upstream websocket [codex]: the backend ended connections before the planned 25m0s (the last at 12m5s, close status 1001); replacing them after 20m0s from now on",
		"upstream websocket [codex]: the backend ended connections before the planned 20m0s (the last at 11m0s, no close frame); replacing them after 15m0s from now on",
	} {
		if !strings.HasSuffix(lines[i], tail) || !strings.HasPrefix(lines[i], "[") {
			t.Errorf("line %d = %q, want it to end with %q", i, lines[i], tail)
		}
	}
}

func TestWSStatsTextCountsBreaksAndEarlyReplacements(t *testing.T) {
	plain := wsh.WsStatsText(codexws.WSStats{Delta: 12, Full: 3, Busy: 1, Dials: 2, SentBytes: 41 << 10}, 0)
	if want := "12 delta, 3 full, 1 busy, 0 HTTP fallbacks, 2 dials; sent 41 KiB"; plain != want {
		t.Errorf("plain = %q, want %q", plain, want)
	}
	full := wsh.WsStatsText(codexws.WSStats{Delta: 1, Full: 2, Fallbacks: 1, Dials: 1, SentBytes: 2048, Lost: 1, Early: 2}, 1)
	for _, want := range []string{"2 full (1 where a delta could not continue),", "1 HTTP fallback,", "; 1 connection ended by the backend or the network", "; 2 connections replaced at the lowered age"} {
		if !strings.Contains(full, want) {
			t.Errorf("%q lacks %q", full, want)
		}
	}
}

func TestLinkCauseNamesAWebsocketEndedByTheBackend(t *testing.T) {
	cause, ok := obs.LinkCause("stream read error: codex websocket: connection failed", false)
	if !ok || cause != obs.CauseWSEnded {
		t.Fatalf("cause = %q, %v", cause, ok)
	}
	// The keepalive's own close is still a lost connection.
	if cause, _ := obs.LinkCause("codex websocket: client connection lost", false); cause != obs.CauseLost {
		t.Fatalf("keepalive close cause = %q", cause)
	}
	var s obs.LinkStats
	s.Break(obs.CauseWSEnded, true)
	if text := s.Take(); !strings.Contains(text, "1 broken call (1 sent again by ccgp, 0 reached the client): 1 "+obs.CauseWSEnded) {
		t.Fatalf("link line = %q", text)
	}
}

func TestWSDivergenceTextSaysWhatChangedNotTheContent(t *testing.T) {
	edited := wschain.WSDivergence{At: 1236, Had: 1237, Has: 1240,
		Was:      wschain.ItemShape{Kind: "user message", Size: 3000, Parts: 2},
		Now:      wschain.ItemShape{Kind: "user message", Size: 3412, Parts: 3},
		SameHead: 2990, SameTail: 4, Reply: true, ReplyAt: 1237, Since: 41 * time.Second}
	want := "history diverged at item 1236 of 1237 (this request has 1240): was user message 3 KiB in 2 parts, " +
		"now user message 3 KiB in 3 parts (+412 B); first 3 KiB and last 4 B equal, between them 6 B -> 418 B; " +
		"the reply chained on is at item 1237; the chained reply ended 41s ago | prompt key abcd1234"
	if got := obs.WsDivergenceText(edited, "prompt key abcd1234"); got != want {
		t.Errorf("edited: got %q, want %q", got, want)
	}

	ended := wschain.WSDivergence{At: 5, Had: 6, Has: 5, Was: wschain.ItemShape{Kind: "function_call_output", Size: 10}, SameHead: -1, SameTail: -1, ReplyAt: -1}
	want = "history diverged at item 5 of 6 (this request has 5): was function_call_output 10 B, now the request ends there; the chain has no reply"
	if got := obs.WsDivergenceText(ended, ""); got != want {
		t.Errorf("ended: got %q, want %q", got, want)
	}

	replaced := wschain.WSDivergence{At: 0, Had: 3, Has: 3, Was: wschain.ItemShape{Kind: "user message", Size: 2048, Parts: 1},
		Now: wschain.ItemShape{Kind: "user message", Size: 1024, Parts: 1}, SameHead: -1, SameTail: -1, Reply: true, ReplyAt: -1}
	want = "history diverged at item 0 of 3 (this request has 3): was user message 2 KiB in 1 part, now user message 1 KiB in 1 part (-1 KiB); " +
		"the reply chained on is not in this request"
	if got := obs.WsDivergenceText(replaced, ""); got != want {
		t.Errorf("replaced: got %q, want %q", got, want)
	}
}

func TestWebsocketLineNamesTheConversationAndLogsTheDivergence(t *testing.T) {
	old := shared.LogOut
	var buf bytes.Buffer
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })

	if got := wsh.WsConvTag("3fa2c1d4e5"); got != "3fa2c1" {
		t.Errorf("wsConvTag = %q", got)
	}
	trace := obs.NewUpstreamTrace()
	trace.Wire = &codex.Wire{}
	trace.Conv = "3fa2c1"
	trace.Prompt = "prompt key abcd1234"
	trace.NoteWebsocket(codexws.WSInfo{Mode: "full", Reused: true, ConnAge: time.Second, Reason: "history diverged at item 0 of 1",
		Divergence: &wschain.WSDivergence{At: 0, Had: 1, Has: 2, Was: wschain.ItemShape{Kind: "user message", Size: 50, Parts: 1},
			Now: wschain.ItemShape{Kind: "user message", Size: 50, Parts: 1}, SameHead: 40, SameTail: 4, ReplyAt: -1}}, 100)
	if trace.WsConn != "conn reused (age 1s, conv 3fa2c1)" {
		t.Errorf("conn text = %q", trace.WsConn)
	}
	line := buf.String()
	if !strings.Contains(line, "upstream websocket [codex] conv 3fa2c1: history diverged at item 0 of 1 (this request has 2)") ||
		!strings.HasSuffix(line, "| prompt key abcd1234\n") {
		t.Errorf("divergence line = %q", line)
	}

	buf.Reset()
	plain := obs.NewUpstreamTrace()
	plain.Wire = &codex.Wire{}
	plain.NoteWebsocket(codexws.WSInfo{Mode: "delta", Reused: true, ConnAge: time.Second}, 100)
	if plain.WsConn != "conn reused (age 1s)" || buf.Len() != 0 {
		t.Errorf("without a conversation: conn %q, log %q", plain.WsConn, buf.String())
	}
}

func TestLogWSLostSaysWhetherTheBreakCountsTowardTheLimit(t *testing.T) {
	old := shared.LogOut
	var buf bytes.Buffer
	shared.LogOut = &buf
	t.Cleanup(func() { shared.LogOut = old })
	wsh.LogWSLost(codexws.WSLost{Age: 12*time.Minute + 5*time.Second, Status: 1001, Planned: 25 * time.Minute, Counts: true})
	wsh.LogWSLost(codexws.WSLost{Age: 2 * time.Minute, Status: -1, Planned: 25 * time.Minute})
	wsh.LogWSLost(codexws.WSLost{Age: 25 * time.Minute, Status: 1006, Planned: 25 * time.Minute})
	wsh.LogWSLost(codexws.WSLost{Age: 5*time.Minute + 35*time.Second, Status: -1, Planned: 20 * time.Minute, Burst: true})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %q", lines)
	}
	for i, tail := range []string{
		"a connection was ended by the backend or the network at 12m5s of age (close status 1001; planned 25m0s): before the planned age, counts toward lowering it",
		"a connection was ended by the backend or the network at 2m0s of age (no close frame; planned 25m0s): too young to say anything about a limit",
		"a connection was ended by the backend or the network at 25m0s of age (close status 1006; planned 25m0s): at or after the planned age",
		"a connection was ended by the backend or the network at 5m35s of age (no close frame; planned 20m0s): before the planned age, but right after another such break: one event, not counted again",
	} {
		if !strings.HasSuffix(lines[i], tail) {
			t.Errorf("line %d = %q, want it to end with %q", i, lines[i], tail)
		}
	}
}

func TestWSAgentTagIsShortAndOptional(t *testing.T) {
	if got := wsh.WsAgentTag("1a2b3c4d"); got != ".1a2b" {
		t.Errorf("wsAgentTag = %q", got)
	}
	if wsh.WsAgentTag("") != "" || wsh.WsAgentTag("abc") != "" {
		t.Error("a missing or short agent key must give no tag")
	}
}
