package sse

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
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
	if th["thinking"] != "think" || th["signature"] != codex.SignaturePrefix+"ENC" {
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
	if !strings.Contains(tr.Failed, "ended before completion") {
		t.Errorf("Failed = %q", tr.Failed)
	}
}

// Every way a stream can fail sends the client an error event and records why.
func TestTranslatorFailureReasons(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"r"}}`
	tests := []struct {
		name, body, want string
	}{
		{"response.failed", sseBody(created, `{"type":"response.failed","response":{"error":{"message":"model overloaded"}}}`), "model overloaded"},
		{"response.failed without text", sseBody(created, `{"type":"response.failed","response":{}}`), "response failed"},
		{"error event", sseBody(created, `{"type":"error","message":"rate limited"}`), "rate limited"},
		{"error event with code only", sseBody(created, `{"type":"error","code":"E42"}`), "upstream error E42"},
		{"error event with an error object", sseBody(created, `{"type":"error","error":{"type":"server_error","message":"boom"}}`), "server_error: boom"},
		{"empty error event", sseBody(created, `{"type":"error"}`), "upstream error"},
		{"response.failed with code", sseBody(created, `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"slow down"}}}`), "rate_limit_exceeded: slow down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotErr bool
			tr := &Translator{Model: "m", Emit: func(ev string, _ any) { gotErr = gotErr || ev == "error" }}
			_ = tr.Run(strings.NewReader(tt.body))
			if !gotErr || tr.Failed != tt.want {
				t.Errorf("error event %v, Failed = %q; want %q", gotErr, tr.Failed, tt.want)
			}
			if tr.Detail == "" || !strings.Contains(tt.body, tr.Detail) {
				t.Errorf("Detail = %q, want the raw failing event", tr.Detail)
			}
		})
	}
}

func TestTranslatorReadErrorIsRecorded(t *testing.T) {
	var gotErr bool
	tr := &Translator{Model: "m", Emit: func(ev string, _ any) { gotErr = gotErr || ev == "error" }}
	if err := tr.Run(failingReader{}); err == nil {
		t.Error("Run swallowed the read error")
	}
	if !gotErr || !strings.Contains(tr.Failed, "stream read error") {
		t.Errorf("error event %v, Failed = %q", gotErr, tr.Failed)
	}
}

func TestTranslatorSuccessLeavesFailedEmpty(t *testing.T) {
	tr := &Translator{Model: "m", Emit: func(string, any) {}}
	_ = tr.Run(strings.NewReader(upstreamStream))
	if tr.Failed != "" {
		t.Errorf("Failed = %q after a complete stream", tr.Failed)
	}
}

func TestTranslatorRetriesOverloadBeforeOutput(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"r"}}`
	var events []string
	tr := &Translator{Model: "m", AllowRetry: true, Emit: func(ev string, _ any) { events = append(events, ev) }}
	if err := tr.Run(strings.NewReader(sseBody(created, overloadedEvent))); !errors.Is(err, ErrRetry) {
		t.Fatalf("Run = %v, want ErrRetry", err)
	}
	if tr.Failed != "" || !strings.Contains(tr.Retry, "server_is_overloaded") || !strings.Contains(tr.Detail, "overloaded") {
		t.Fatalf("Failed %q, Retry %q, Detail %q", tr.Failed, tr.Retry, tr.Detail)
	}
	if !tr.Started() || strings.Join(events, ",") != "message_start" {
		t.Fatalf("events before the retry = %v", events)
	}
	if err := tr.Run(strings.NewReader(upstreamStream)); err != nil || tr.Failed != "" {
		t.Fatalf("second Run = %v, Failed %q", err, tr.Failed)
	}
	if n := strings.Count(strings.Join(events, ","), "message_start"); n != 1 {
		t.Errorf("message_start sent %d times", n)
	}
	if events[len(events)-1] != "message_stop" {
		t.Errorf("stream did not complete: %v", events)
	}
}

// Sent tells how far a stream got, which decides what the client does with a
// failure: after thinking only it asks again, after text or a call it keeps them.
func TestTranslatorSentNamesTheKindsOfBlocks(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"r"}}`
	think := `{"type":"response.output_item.added","item":{"id":"rs","type":"reasoning"}}`
	text := `{"type":"response.output_item.added","item":{"id":"m","type":"message"}}`
	text2 := `{"type":"response.output_item.added","item":{"id":"m2","type":"message"}}`
	call := `{"type":"response.output_item.added","item":{"id":"f","type":"function_call","call_id":"c","name":"Read"}}`
	tests := []struct{ name, body, want string }{
		{"nothing", sseBody(created, overloadedEvent), ""},
		{"thinking only", sseBody(created, think, overloadedEvent), "thinking"},
		{"thinking, text", sseBody(created, think, text, overloadedEvent), "thinking, text"},
		{"a tool call", sseBody(created, think, call, overloadedEvent), "thinking, tool_use"},
		{"a kind twice in a row is named once", sseBody(created, text, text2, overloadedEvent), "text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &Translator{Model: "m", Emit: func(string, any) {}}
			_ = tr.Run(strings.NewReader(tt.body))
			if got := tr.Sent(); got != tt.want {
				t.Errorf("Sent = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTranslatorDoesNotRetry(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"r"}}`
	text := `{"type":"response.output_item.added","item":{"id":"m","type":"message"}}`
	tests := []struct {
		name  string
		allow bool
		body  string
	}{
		{"retries not allowed", false, sseBody(created, overloadedEvent)},
		{"content already sent", true, sseBody(created, text, overloadedEvent)},
		{"not an overload", true, sseBody(created, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &Translator{Model: "m", AllowRetry: tt.allow, Emit: func(string, any) {}}
			if err := tr.Run(strings.NewReader(tt.body)); errors.Is(err, ErrRetry) || tr.Failed == "" {
				t.Fatalf("Run = %v, Failed %q: want a failed stream", err, tr.Failed)
			}
		})
	}
}

// Which upstream errors are worth asking again (nothing was sent to the client
// yet) and which are answers: a wrong guess either hides a real failure or
// hammers a backend that said no.
func TestOverloadedClassification(t *testing.T) {
	again := [][2]string{
		{"server_is_overloaded", "service_unavailable_error"}, // seen at 15:59 and 16:52
		{"server_is_overloaded", ""},
		{"service_unavailable", ""},
		{"server_error", ""},
		{"", "service_unavailable_error"},
		{"", "server_error"},
	}
	for _, c := range again {
		if !overloaded(c[0], c[1]) {
			t.Errorf("code %q type %q: not retried", c[0], c[1])
		}
	}
	answers := [][2]string{
		{"", ""},
		{"context_length_exceeded", "invalid_request_error"},
		{"invalid_prompt", "invalid_request_error"},
		{"rate_limit_exceeded", "rate_limit_error"},
		{"insufficient_quota", "insufficient_quota"},
		{"model_not_found", ""},
	}
	for _, c := range answers {
		if overloaded(c[0], c[1]) {
			t.Errorf("code %q type %q: retried, but it is an answer", c[0], c[1])
		}
	}
}

// A connection that breaks before the client has seen any content is worth
// another try; once content went out it is not, and a canceled request never is.
func TestBrokenStreamRetry(t *testing.T) {
	created := sseBody(`{"type":"response.created","response":{"id":"r"}}`)
	withText := sseBody(
		`{"type":"response.created","response":{"id":"r"}}`,
		`{"type":"response.output_item.added","item":{"id":"m","type":"message"}}`,
		`{"type":"response.output_text.delta","item_id":"m","delta":"half"}`)
	cut := func(prefix string, err error) io.Reader {
		return io.MultiReader(strings.NewReader(prefix), errReader{err})
	}
	tests := []struct {
		name      string
		allow     bool
		body      io.Reader
		wantRetry bool
	}{
		{"read error before content", true, cut(created, io.ErrUnexpectedEOF), true},
		{"connection lost before content", true, cut(created, errors.New("http2: client connection lost")), true},
		{"read error with nothing at all", true, cut("", io.ErrUnexpectedEOF), true},
		{"stream ends before completion", true, strings.NewReader(created), true},
		{"not allowed", false, cut(created, io.ErrUnexpectedEOF), false},
		{"read error after content", true, cut(withText, io.ErrUnexpectedEOF), false},
		{"ends early after content", true, strings.NewReader(withText), false},
		{"client left", true, cut(created, context.Canceled), false},
		{"client left, wrapped", true, cut(created, errors.New("Post x: context canceled")), false},
		{"deadline", true, cut(created, context.DeadlineExceeded), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotErr bool
			tr := &Translator{Model: "m", AllowTransportRetry: tt.allow, Emit: func(ev string, _ any) { gotErr = gotErr || ev == "error" }}
			err := tr.Run(tt.body)
			if got := errors.Is(err, ErrRetry); got != tt.wantRetry {
				t.Fatalf("retry = %v (err %v), want %v", got, err, tt.wantRetry)
			}
			if tt.wantRetry {
				if gotErr || tr.Failed != "" || !tr.RetryTransport || tr.Retry == "" {
					t.Errorf("a retried stream failed the client: error event %v, Failed %q, transport %v, reason %q", gotErr, tr.Failed, tr.RetryTransport, tr.Retry)
				}
			} else if !gotErr || tr.Failed == "" {
				t.Errorf("a stream that is not retried must fail the client: error event %v, Failed %q", gotErr, tr.Failed)
			}
		})
	}
}

// The same Translator runs the next stream: an overload after a broken stream is
// reported as an overload, and the retry flags do not stick.
func TestRetryKindFollowsTheLastFailure(t *testing.T) {
	tr := &Translator{Model: "m", AllowRetry: true, AllowTransportRetry: true, Emit: func(string, any) {}}
	created := `{"type":"response.created","response":{"id":"r"}}`
	if err := tr.Run(io.MultiReader(strings.NewReader(sseBody(created)), errReader{io.ErrUnexpectedEOF})); !errors.Is(err, ErrRetry) || !tr.RetryTransport {
		t.Fatalf("first run: %v, transport %v", err, tr.RetryTransport)
	}
	overload := `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"o"}}`
	if err := tr.Run(strings.NewReader(sseBody(created, overload))); !errors.Is(err, ErrRetry) || tr.RetryTransport {
		t.Fatalf("second run: %v, transport %v; want an overload retry", err, tr.RetryTransport)
	}
}
