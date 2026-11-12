package server

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDropShownMessage(t *testing.T) {
	for _, tt := range []struct {
		name, detail, msg, want string
		dropped                 bool
	}{
		{"error event",
			`{"type":"error","error":{"code":"cyber_policy","message":"flagged"}}`, "cyber_policy: flagged",
			`{"type":"error","error":{"code":"cyber_policy","message":"(shown before)"}}`, true},
		{"response.failed, spaces",
			`{"type":"response.failed","response":{"error":{"message" : "boom ’x\""}}}`, "server_error: boom ’x\"",
			`{"type":"response.failed","response":{"error":{"message" : "(shown before)"}}}`, true},
		{"another message is kept",
			`{"error":{"message":"other"}}`, "boom", `{"error":{"message":"other"}}`, false},
		{"a value named message is kept",
			`{"type":"message","error":{"message":"boom"}}`, "boom", `{"type":"message","error":{"message":"(shown before)"}}`, true},
		{"empty message is kept",
			`{"error":{"message":""}}`, "boom", `{"error":{"message":""}}`, false},
		{"not a string",
			`{"message":null,"error":{"message":"boom"}}`, "boom", `{"message":null,"error":{"message":"(shown before)"}}`, true},
		{"broken json", `{"message":"bo`, "boom", `{"message":"bo`, false},
		{"found inside the reason is another text", `{"error":{"message":"a"}}`, "bad", `{"error":{"message":"a"}}`, false},
		{"the end of a word is not the message", `{"error":{"message":"boom"}}`, "kaboom", `{"error":{"message":"boom"}}`, false},
		{"no detail", "", "boom", "", false},
	} {
		got, dropped := dropShownMessage(tt.detail, tt.msg)
		if got != tt.want || dropped != tt.dropped {
			t.Errorf("%s: got %q, %v; want %q, %v", tt.name, got, dropped, tt.want, tt.dropped)
		}
	}
}

// The live cyber_policy refusal: its long message is written once, whole.
func TestStreamFailureWritesARepeatedMessageOnce(t *testing.T) {
	text := "This content was flagged for possible cybersecurity risk. " + strings.Repeat("If this seems wrong, try rephrasing your request. ", 8) + "END"
	ev := `{"type":"error","error":{"type":"invalid_request_error","code":"cyber_policy","message":"` + text + `","param":null}}`
	var b bytes.Buffer
	logStreamFailure(&b, time.Now(), "stream", "codex", "a -> b", "cyber_policy: "+text, ev, "cf-ray=1")
	got := b.String()
	if n := strings.Count(got, "This content was flagged"); n != 1 {
		t.Errorf("message written %d times: %q", n, got)
	}
	want := ": cyber_policy: " + text + ` | upstream event: {"type":"error","error":{"type":"invalid_request_error","code":"cyber_policy","message":"(shown before)","param":null}} | cf-ray=1` + "\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("line %q\nwant suffix %q", got, want)
	}
}

func TestUpstreamRetryDropsTheShownMessage(t *testing.T) {
	var b bytes.Buffer
	logUpstreamRetry(&b, "codex", "a -> b", 1, 2, 2*time.Second, "server_is_overloaded: overloaded",
		`{"type":"error","error":{"code":"server_is_overloaded","message":"overloaded"}}`, "cf-ray=1")
	want := `server_is_overloaded: overloaded | upstream event: {"type":"error","error":{"code":"server_is_overloaded","message":"(shown before)"}} | cf-ray=1` + "\n"
	if got := b.String(); !strings.HasSuffix(got, want) {
		t.Errorf("line %q\nwant suffix %q", got, want)
	}
}

// Without a repeat the reason keeps its usual cap.
func TestEventTextCapsAReasonNotRepeated(t *testing.T) {
	long := strings.Repeat("x", maxLogErr+50)
	if got := eventText(long, `{"error":{"message":"other"}}`); !strings.HasPrefix(got, strings.Repeat("x", maxLogErr)+"… | upstream event: ") {
		t.Errorf("got %q", got)
	}
}
