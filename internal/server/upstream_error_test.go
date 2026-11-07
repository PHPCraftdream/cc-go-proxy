package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamErrorParts(t *testing.T) {
	tests := []struct {
		name, body, label, msg string
	}{
		{"anthropic envelope", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "rate_limit_error", "slow down"},
		{"z.ai: string code in error", `{"error":{"code":"1302","message":"Rate limit reached for requests"}}`, "1302", "Rate limit reached for requests"},
		{"type and code together", `{"error":{"type":"overloaded_error","code":529,"message":"busy"}}`, "overloaded_error 529", "busy"},
		{"numeric code, msg at the top", `{"code":1113,"msg":"insufficient balance","success":false}`, "1113", "insufficient balance"},
		{"message at the top", `{"message":"nope"}`, "", "nope"},
		{"error as a string", `{"error":"forbidden here"}`, "", "forbidden here"},
		{"detail", `{"detail":"model not found"}`, "", "model not found"},
		{"envelope only", `{"type":"error"}`, "", `{"type":"error"}`},
		{"html page", `<html><head><title>Attention Required! | Cloudflare</title></head><body>blocked</body></html>`, "", "html error page: Attention Required! | Cloudflare"},
		{"plain text", "upstream connect error", "", "upstream connect error"},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label, msg := upstreamErrorParts([]byte(tt.body))
			if label != tt.label || msg != tt.msg {
				t.Errorf("label = %q, message = %q; want %q, %q", label, msg, tt.label, tt.msg)
			}
		})
	}
}

func TestUpstreamErrorText(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "30")
	h.Set("X-Ratelimit-Remaining-Requests", "0")
	h.Set("X-Request-Id", "req-1")
	body := []byte(`{"error":{"code":"1302","message":"Rate limit reached"}}`)

	got := upstreamErrorText(429, h, body)
	for _, want := range []string{"1302: Rate limit reached", "retry-after=30", "x-ratelimit-remaining-requests=0", "x-request-id=req-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("429 text lacks %q: %q", want, got)
		}
	}
	// The limit headers say something about a limit only; a bad request gets the reason and the id.
	got = upstreamErrorText(400, h, []byte(`{"error":{"type":"invalid_request_error","message":"bad field"}}`))
	if !strings.Contains(got, "invalid_request_error: bad field") || !strings.Contains(got, "x-request-id=req-1") {
		t.Errorf("400 text = %q", got)
	}
	if strings.Contains(got, "retry-after") || strings.Contains(got, "ratelimit") {
		t.Errorf("a 400 carries limit headers: %q", got)
	}
	if got = upstreamErrorText(503, h, []byte(`{"error":{"message":"overloaded"}}`)); !strings.Contains(got, "retry-after=30") {
		t.Errorf("503 text lacks Retry-After: %q", got)
	}
	// Nothing to say is nothing.
	if got = upstreamErrorText(500, http.Header{}, nil); got != "" {
		t.Errorf("empty reply gave %q", got)
	}
}

// P0: whatever the endpoint echoes of the credential, the log does not keep it.
func TestUpstreamErrorTextHidesKeys(t *testing.T) {
	for _, msg := range []string{
		"Incorrect API key provided: sk-proj-abcdef1234567890XYZ",
		"invalid Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
		"bad key 0123456789abcdef0123456789abcdef.AbCdEfGh12345678 for this account",
		`request failed: {"api_key": "supersecretvalue99"}`,
		"token=abcdef123456 rejected",
	} {
		body := []byte(`{"error":{"message":` + jsonString(msg) + `}}`)
		got := upstreamErrorText(401, http.Header{}, body)
		for _, secret := range []string{"sk-proj-abcdef1234567890XYZ", "eyJhbGciOiJIUzI1NiJ9", "0123456789abcdef0123456789abcdef", "AbCdEfGh12345678", "supersecretvalue99", "abcdef123456"} {
			if strings.Contains(got, secret) {
				t.Errorf("%q leaked %q: %q", msg, secret, got)
			}
		}
		if !strings.Contains(got, "***") {
			t.Errorf("nothing was hidden in %q: %q", msg, got)
		}
	}
	// Ordinary words are left as they are.
	plain := "model glm-5.3-flash is not available, token limit reached, try again"
	if got := redactSecrets(plain); got != plain {
		t.Errorf("redactSecrets changed %q to %q", plain, got)
	}
}

func jsonString(s string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// P0: the log line is one line of a bounded size, however the endpoint answers.
func TestUpstreamErrorTextIsOneBoundedLine(t *testing.T) {
	long := strings.Repeat("a long and rambling reason\n\t", 200)
	got := upstreamErrorText(500, http.Header{}, []byte(`{"error":{"message":`+jsonString(long)+`}}`))
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("not one line: %q", got)
	}
	if len(got) > maxUpstreamMsg+len("…")+16 {
		t.Errorf("%d bytes: %q", len(got), got)
	}

	html := "<html><head><title>Blocked</title></head><body>" + strings.Repeat("<p>markup</p>\n", 500) + "</body></html>"
	got = upstreamErrorText(403, http.Header{}, []byte(html))
	if strings.Contains(got, "<p>") || strings.Contains(got, "\n") || !strings.Contains(got, "Blocked") {
		t.Errorf("an HTML page is shown as markup: %q", got)
	}
}

// P0: the reason reaches the request's line for a refusal, and the client still gets the reply as it came.
func TestZaiRefusalLineGivesTheReason(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "42")
		w.Header().Set("X-Request-Id", "req-zai-1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"1302","message":"Rate limit reached for requests, key sk-live-abcdef123456"}}`)
	}))
	defer refusing.Close()

	lines := zaiLines(t, refusing.URL, 1)
	line := lines[0]
	for _, want := range []string{" 429 - ", "claude-sonnet-5-5 -> glm-5.3-flash: 1302: Rate limit reached for requests", "retry-after=42", "x-request-id=req-zai-1", " | conn new (dns "} {
		if !strings.Contains(line, want) {
			t.Errorf("the line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "sk-live-abcdef123456") {
		t.Errorf("a key is in the log:\n%s", line)
	}

	// The client's reply is untouched.
	resp, err := compatApp(compatConfig(refusing.URL, nil)).Test(httptest.NewRequest("POST", "/v1/messages", strings.NewReader(sampleRequest)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "42" || !strings.Contains(string(body), "sk-live-abcdef123456") {
		t.Errorf("the client's reply changed: %d %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

// A successful request has no reason on its line, and a refused count_tokens (answered with an
// estimate, status 200) neither.
func TestZaiLinesWithoutARefusalHaveNoReason(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	defer ok.Close()
	if line := zaiLines(t, ok.URL, 1)[0]; strings.Contains(line, "glm-5.3-flash:") {
		t.Errorf("a reason on a good line: %q", line)
	}
}
