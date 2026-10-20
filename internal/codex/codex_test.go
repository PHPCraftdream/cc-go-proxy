package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claude-code-proxy/proxy/internal/oauth"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestConvertRequest(t *testing.T) {
	body := `{
		"model": "claude-sonnet-4-5",
		"stream": true,
		"system": [
			{"type":"text","text":"x-anthropic-billing-header: abc"},
			{"type":"text","text":"You are Claude Code."}
		],
		"thinking": {"type":"enabled","budget_tokens": 32000},
		"metadata": {"user_id": "user_1_session_2"},
		"tools": [
			{"name":"Read","description":"read a file","input_schema":{"type":"object"}},
			{"type":"web_search_20250305","name":"web_search"}
		],
		"messages": [
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"plan","signature":"ccgp1:ENC"},
				{"type":"thinking","thinking":"foreign","signature":"anthropic-sig"},
				{"type":"text","text":"reading"},
				{"type":"tool_use","id":"call_1","name":"Read","input":{"path":"a.go"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"package a"}]},
				{"type":"text","text":"now explain"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}
			]}
		]
	}`
	req, stream, err := ConvertRequest([]byte(body), "gpt-5", "work")
	if err != nil {
		t.Fatal(err)
	}
	if !stream || !req.Stream || req.Store {
		t.Errorf("stream=%v req.Stream=%v store=%v", stream, req.Stream, req.Store)
	}
	if req.Model != "gpt-5" || req.Reasoning.Effort != "high" || req.PromptCacheKey == "" {
		t.Errorf("model=%q effort=%q cache=%q", req.Model, req.Reasoning.Effort, req.PromptCacheKey)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "Read" {
		t.Errorf("tools = %+v", req.Tools)
	}

	// Round-trip through any: struct field order vs sorted map keys.
	var norm any
	if err := json.Unmarshal([]byte(mustJSON(t, req.Input)), &norm); err != nil {
		t.Fatal(err)
	}
	got := mustJSON(t, norm)
	want := mustJSON(t, []any{
		map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "You are Claude Code."}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "plan"}}, "encrypted_content": "ENC"},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "reading"}}},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "Read", "arguments": `{"path":"a.go"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "package a"},
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "now explain"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAA"},
		}},
	})
	if got != want {
		t.Errorf("input =\n%s\nwant\n%s", got, want)
	}
}

func TestConvertRequestToolChoice(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"x"}],
		"tools":[{"name":"T","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"T"}}`
	req, stream, err := ConvertRequest([]byte(body), "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		t.Error("stream defaulted to true")
	}
	if mustJSON(t, req.ToolChoice) != `{"name":"T","type":"function"}` {
		t.Errorf("tool_choice = %s", mustJSON(t, req.ToolChoice))
	}
	if req.Reasoning.Effort != "medium" {
		t.Errorf("default effort = %q", req.Reasoning.Effort)
	}
}

// sseBody builds an upstream Responses stream.
func sseBody(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: " + e + "\n\n")
	}
	return b.String()
}

var upstreamStream = sseBody(
	`{"type":"response.created","response":{"id":"resp_1"}}`,
	`{"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
	`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"think"}`,
	`{"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"ENC"}}`,
	`{"type":"response.output_item.added","item":{"id":"msg_1","type":"message"}}`,
	`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hel"}`,
	`{"type":"response.output_text.delta","item_id":"msg_1","delta":"lo"}`,
	`{"type":"response.output_item.done","item":{"id":"msg_1","type":"message"}}`,
	`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_9","name":"Read"}}`,
	`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","arguments":"{\"path\":\"a\"}"}}`,
	`{"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":40}}}}`,
)

func TestTranslatorStream(t *testing.T) {
	var names []string
	var last map[string]any
	tr := &Translator{Model: "claude-sonnet-4-5", Emit: func(ev string, data any) {
		names = append(names, ev)
		if ev == "message_delta" {
			last = data.(map[string]any)
		}
	}}
	if err := tr.Run(strings.NewReader(upstreamStream)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", // thinking + signature
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", // text
		"content_block_start", "content_block_delta", "content_block_stop", // tool_use (args from done)
		"message_delta", "message_stop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("events =\n%v\nwant\n%v", names, want)
	}
	if d := last["delta"].(map[string]any); d["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", d["stop_reason"])
	}
	if u := last["usage"].(map[string]any); u["input_tokens"] != 60 || u["cache_read_input_tokens"] != 40 || u["output_tokens"] != 7 {
		t.Errorf("usage = %v", u)
	}
}

func TestCollector(t *testing.T) {
	col := &Collector{}
	tr := &Translator{Model: "claude-x", Emit: col.Emit}
	_ = tr.Run(strings.NewReader(upstreamStream))
	out, err := col.Result()
	if err != nil {
		t.Fatal(err)
	}
	content := out["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %v", content)
	}
	th, text, tool := content[0].(map[string]any), content[1].(map[string]any), content[2].(map[string]any)
	if th["thinking"] != "think" || th["signature"] != SignaturePrefix+"ENC" {
		t.Errorf("thinking = %v", th)
	}
	if text["text"] != "Hello" {
		t.Errorf("text = %v", text)
	}
	if tool["id"] != "call_9" || mustJSON(t, tool["input"]) != `{"path":"a"}` {
		t.Errorf("tool = %v", tool)
	}
	if out["model"] != "claude-x" || out["stop_reason"] != "tool_use" {
		t.Errorf("msg = %v", out)
	}
}

func TestTranslatorTruncatedStream(t *testing.T) {
	var gotErr bool
	tr := &Translator{Model: "m", Emit: func(ev string, _ any) { gotErr = gotErr || ev == "error" }}
	_ = tr.Run(strings.NewReader(sseBody(`{"type":"response.created","response":{"id":"r"}}`)))
	if !gotErr {
		t.Error("no error event for truncated stream")
	}
}

func TestCallRetriesAfter401(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("ChatGPT-Account-ID") != "acc" || r.Header.Get("originator") != originator {
			t.Errorf("headers = %v", r.Header)
		}
		if n == 1 {
			if r.Header.Get("Authorization") != "Bearer old" {
				t.Errorf("first auth = %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new" {
			t.Errorf("retry auth = %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte("data: {}\n\n"))
	}))
	defer backend.Close()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"new","refresh_token":"rt2"}`))
	}))
	defer issuer.Close()

	store := oauth.Store{Path: filepath.Join(t.TempDir(), "o.json")}
	if err := store.Save(&oauth.Tokens{AccessToken: "old", RefreshToken: "rt1", AccountID: "acc", LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s := &oauth.Session{Store: store, Client: &oauth.Client{Issuer: issuer.URL}}
	resp, err := Call(context.Background(), backend.URL, s, &Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 2 {
		t.Errorf("status=%d calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestErrorMessage(t *testing.T) {
	tests := map[string]string{
		`{"error":{"type":"usage_limit_reached","message":"limit hit"}}`: "limit hit",
		`{"error":"bad"}`:   "bad",
		`{"detail":"nope"}`: "nope",
		`plain text`:        "plain text",
	}
	for in, want := range tests {
		if got := ErrorMessage([]byte(in)); got != want {
			t.Errorf("ErrorMessage(%s) = %q, want %q", in, got, want)
		}
	}
}
