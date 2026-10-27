package codex

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// Regression tests: each one pins something that went wrong in real use.

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

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
