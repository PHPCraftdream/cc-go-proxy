package codex

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func fpBody(system, tools, userID string, messages int) string {
	var msgs []string
	for i := range messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, fmt.Sprintf(`{"role":%q,"content":"secret message %d"}`, role, i))
	}
	sys := ""
	if system != "" {
		sys = fmt.Sprintf(`"system":[{"type":"text","text":%q}],`, system)
	}
	tl := ""
	if tools != "" {
		tl = fmt.Sprintf(`"tools":[{"name":%q,"description":"secret tool description","input_schema":{"type":"object"}}],`, tools)
	}
	meta := ""
	if userID != "" {
		meta = fmt.Sprintf(`"metadata":{"user_id":%q},`, userID)
	}
	return fmt.Sprintf(`{"model":"claude-sonnet-5-5",%s%s%s"messages":[%s]}`, sys, tl, meta, strings.Join(msgs, ","))
}

func fingerprintOf(t *testing.T, body string) Fingerprint {
	t.Helper()
	req, _, err := ConvertRequest([]byte(body), "gpt-5", "work")
	if err != nil {
		t.Fatal(err)
	}
	return NewFingerprint(req)
}

func TestFingerprintOfARequest(t *testing.T) {
	f := fingerprintOf(t, fpBody("You are Claude Code.", "Read", "user_1_session_2", 3))
	hex8 := regexp.MustCompile(`^[0-9a-f]{8}$`)
	for name, v := range map[string]string{"key": f.Key, "system": f.System, "tools": f.Tools} {
		if !hex8.MatchString(v) {
			t.Errorf("%s = %q, want 8 hex digits", name, v)
		}
	}
	// The system prompt is the first input item, then the three messages.
	if f.NTools != 1 || f.Items != 4 {
		t.Errorf("tools = %d, items = %d; want 1 and 4", f.NTools, f.Items)
	}
	req, _, _ := ConvertRequest([]byte(fpBody("You are Claude Code.", "Read", "user_1_session_2", 3)), "gpt-5", "work")
	if !strings.HasPrefix(req.PromptCacheKey, f.Key) {
		t.Errorf("key %q is not the start of the cache key %q", f.Key, req.PromptCacheKey)
	}
	want := fmt.Sprintf("key %s, system %s, 1 tools %s, 4 input items", f.Key, f.System, f.Tools)
	if got := f.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestFingerprintIsStableAndSeesWhatChanges(t *testing.T) {
	base := fingerprintOf(t, fpBody("sys A", "Read", "u", 3))
	if again := fingerprintOf(t, fpBody("sys A", "Read", "u", 3)); again != base {
		t.Errorf("the same request has two fingerprints: %+v and %+v", base, again)
	}
	grown := fingerprintOf(t, fpBody("sys A", "Read", "u", 5))
	if grown.System != base.System || grown.Tools != base.Tools || grown.Key != base.Key || grown.Items != base.Items+2 {
		t.Errorf("a longer history changes only the count: %+v vs %+v", grown, base)
	}
	if other := fingerprintOf(t, fpBody("sys B", "Read", "u", 3)); other.System == base.System || other.Tools != base.Tools || other.Key != base.Key {
		t.Errorf("another system prompt: %+v vs %+v", other, base)
	}
	if other := fingerprintOf(t, fpBody("sys A", "Write", "u", 3)); other.Tools == base.Tools || other.System != base.System || other.Key != base.Key {
		t.Errorf("another tool: %+v vs %+v", other, base)
	}
	if other := fingerprintOf(t, fpBody("sys A", "Read", "v", 3)); other.Key == base.Key || other.System != base.System {
		t.Errorf("another session: %+v vs %+v", other, base)
	}
}

func TestFingerprintOfARequestWithoutParts(t *testing.T) {
	f := fingerprintOf(t, fpBody("", "", "", 1))
	if f.Key != "" || f.System != "" || f.Tools != "" || f.NTools != 0 || f.Items != 1 {
		t.Errorf("fingerprint = %+v", f)
	}
	if got, want := f.String(), "no cache key, no system prompt, no tools, 1 input items"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := NewFingerprint(&Request{}); got != (Fingerprint{}) {
		t.Errorf("an empty request has the fingerprint %+v", got)
	}
}

// P0: the log gets hashes and counts. Not a word of the prompt, the tools or the
// messages, and not the id the cache key comes from.
func TestFingerprintHoldsNothingOfThePrompt(t *testing.T) {
	body := fpBody("You are Claude Code. TOP-SECRET-SYSTEM-TEXT", "SecretToolName", "user_account_SECRET_ID", 4)
	text := fingerprintOf(t, body).String()
	for _, secret := range []string{"TOP-SECRET-SYSTEM-TEXT", "SecretToolName", "secret tool description", "secret message", "SECRET_ID", "user_account", "Claude Code"} {
		if strings.Contains(text, secret) {
			t.Errorf("the fingerprint line holds %q: %s", secret, text)
		}
	}
	req, _, _ := ConvertRequest([]byte(body), "gpt-5", "work")
	if strings.Contains(text, req.PromptCacheKey) {
		t.Errorf("the whole cache key is in the line: %s", text)
	}
}

// A system prompt is the first input item only when the request has one: a
// first message of the user must not be taken for it.
func TestFingerprintSystemIsNotTheFirstMessage(t *testing.T) {
	f := fingerprintOf(t, fpBody("", "Read", "u", 2))
	if f.System != "" {
		t.Errorf("System = %q for a request without a system prompt", f.System)
	}
	if f.Items != 2 {
		t.Errorf("items = %d, want 2", f.Items)
	}
}
