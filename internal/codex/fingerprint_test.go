package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
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
	key, system, tools := regexp.MustCompile(`^[0-9a-f]{32}$`), regexp.MustCompile(`^[0-9a-f]{8}$`), regexp.MustCompile(`^[0-9a-f]{8}$`)
	for name, v := range map[string]string{"key": f.Key, "system": f.System, "tools": f.Tools} {
		valid := system.MatchString(v)
		if name == "key" {
			valid = key.MatchString(v)
		}
		if name == "tools" {
			valid = tools.MatchString(v)
		}
		if !valid {
			t.Errorf("%s = %q, want the expected hexadecimal digits", name, v)
		}
	}
	// The system prompt is the first input item, then the three messages.
	if f.NTools != 1 || f.Items != 4 {
		t.Errorf("tools = %d, items = %d; want 1 and 4", f.NTools, f.Items)
	}
	req, _, _ := ConvertRequest([]byte(fpBody("You are Claude Code.", "Read", "user_1_session_2", 3)), "gpt-5", "work")
	if f.Key != req.PromptCacheKey {
		t.Errorf("key = %q, want full cache key %q", f.Key, req.PromptCacheKey)
	}
	want := fmt.Sprintf("key %s, system %s, 1 tools %s, 4 input items", f.Key[:8], f.System, f.Tools)
	if got := f.String(); got != want || strings.Contains(got, f.Key) {
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

func TestFingerprintIncludesBackendModelAndControlsIndependentlyOfHistory(t *testing.T) {
	base := Request{
		Model:      "gpt-5-codex",
		Input:      []any{"first history"},
		Reasoning:  &Reasoning{Effort: "high", Summary: "auto"},
		ToolChoice: map[string]any{"type": "function", "name": "secret_tool"},
	}
	f := NewFingerprint(&base)
	if f.Model != base.Model || f.Reasoning == "" || f.ToolChoice == "" {
		t.Fatalf("fingerprint did not capture backend model and controls: %+v", f)
	}
	if len(f.Reasoning) != 8 || len(f.ToolChoice) != 8 {
		t.Errorf("control hashes are not short hashes: reasoning=%q tool choice=%q", f.Reasoning, f.ToolChoice)
	}
	if got := NewFingerprint(&Request{Model: base.Model, Input: []any{"different history"}, Reasoning: base.Reasoning, ToolChoice: base.ToolChoice}); got.Model != f.Model || got.Reasoning != f.Reasoning || got.ToolChoice != f.ToolChoice {
		t.Errorf("history changed independent settings: %+v vs %+v", got, f)
	}
	line := f.String()
	for _, secret := range []string{"high", "auto", "secret_tool", "function"} {
		if strings.Contains(line, secret) {
			t.Errorf("control content %q is exposed: %s", secret, line)
		}
	}

	cases := []struct {
		name string
		edit func(*Request)
	}{
		{"model", func(r *Request) { r.Model = "gpt-5.1-codex" }},
		{"reasoning effort", func(r *Request) { r.Reasoning = &Reasoning{Effort: "low", Summary: "auto"} }},
		{"reasoning summary", func(r *Request) { r.Reasoning = &Reasoning{Effort: "high", Summary: "detailed"} }},
		{"tool choice", func(r *Request) { r.ToolChoice = map[string]any{"type": "function", "name": "another_secret_tool"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.edit(&changed)
			got := NewFingerprint(&changed)
			if got == f {
				t.Errorf("changing %s did not change the fingerprint", tc.name)
			}
		})
	}
}

func TestFingerprintOfARequestWithoutParts(t *testing.T) {
	f := fingerprintOf(t, fpBody("", "", "", 1))
	if f.Key != "" || f.System != "" || f.Tools != "" || f.Model != "gpt-5" || f.Reasoning == "" || f.ToolChoice == "" || f.NTools != 0 || f.Items != 1 {
		t.Errorf("fingerprint = %+v", f)
	}
	if got, want := f.String(), "no cache key, no system prompt, no tools, 1 input items"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := NewFingerprint(&Request{}); got != (Fingerprint{}) {
		t.Errorf("an empty request has the fingerprint %+v", got)
	}
	key := "01234567same-prefix-but-not-rendered"
	f = NewFingerprint(&Request{PromptCacheKey: key})
	if f.Key != key {
		t.Errorf("fingerprint key = %q, want %q", f.Key, key)
	}
	if got := f.String(); !strings.Contains(got, "key 01234567,") || strings.Contains(got, key) {
		t.Errorf("String() = %q; want only the display prefix", got)
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

func hashesOf(t *testing.T, body string) []uint64 {
	t.Helper()
	req, _, err := ConvertRequest([]byte(body), "gpt-5", "work")
	if err != nil {
		t.Fatal(err)
	}
	return ItemHashes(req)
}

func TestItemHashes(t *testing.T) {
	base := hashesOf(t, fpBody("sys", "Read", "u", 4))
	if len(base) != 5 {
		t.Fatalf("%d hashes, want 5 (system prompt and 4 messages)", len(base))
	}
	again := hashesOf(t, fpBody("sys", "Read", "u", 4))
	for i := range base {
		if base[i] != again[i] {
			t.Errorf("item %d hashes differently for the same request", i)
		}
	}
	seen := map[uint64]bool{}
	for _, h := range base {
		if seen[h] {
			t.Errorf("two different items share the hash %d", h)
		}
		seen[h] = true
	}
	// A longer history keeps the hashes of the items it had.
	grown := hashesOf(t, fpBody("sys", "Read", "u", 6))
	if len(grown) != 7 {
		t.Fatalf("%d hashes, want 7", len(grown))
	}
	for i := range base {
		if grown[i] != base[i] {
			t.Errorf("item %d changed when only the tail grew", i)
		}
	}
	// Another system prompt changes item 0 only.
	other := hashesOf(t, fpBody("sys 2", "Read", "u", 4))
	if other[0] == base[0] {
		t.Error("the system prompt changed but item 0 did not")
	}
	for i := 1; i < len(base); i++ {
		if other[i] != base[i] {
			t.Errorf("item %d changed with the system prompt", i)
		}
	}
}

func TestItemHashesOfAnEmptyRequest(t *testing.T) {
	if got := ItemHashes(&Request{}); got != nil {
		t.Errorf("hashes of an empty request: %#v, want nil", got)
	}
}

type countingMarshaler struct {
	calls *int
}

func (m countingMarshaler) MarshalJSON() ([]byte, error) {
	*m.calls++
	return []byte(`{"value":"marshaled"}`), nil
}

func TestItemHashesWithoutKeyDoesNotMarshal(t *testing.T) {
	calls := 0
	req := &Request{Input: []any{countingMarshaler{calls: &calls}}}
	if got := ItemHashes(req); got != nil {
		t.Errorf("hashes without a cache key: %#v, want nil", got)
	}
	if calls != 0 {
		t.Errorf("MarshalJSON called %d times without a cache key, want 0", calls)
	}
}

type failingMarshaler struct{}

func (failingMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New("deliberate marshal failure")
}

func TestItemHashesMatchMarshalReference(t *testing.T) {
	items := []any{
		`quotes: " slash: \\ newline:\n tab:\t <tag>`,
		json.RawMessage(`{"raw":"quote\\\" and newline\\n","number":1}`),
		map[string]any{"z": "last", "a": []any{true, nil, `/<&`}},
		failingMarshaler{},
		map[string]any{"after_error": `still hashes`, "escaped": "\u2028\u2029"},
	}
	req := &Request{PromptCacheKey: "key", Input: items}
	got := ItemHashes(req)
	if len(got) != len(items) {
		t.Fatalf("got %d hashes, want %d", len(got), len(items))
	}
	for i, item := range items {
		b, err := json.Marshal(item)
		if err != nil {
			if got[i] != 0 {
				t.Errorf("item %d hash = %d after marshal error, want zero", i, got[i])
			}
			continue
		}
		h := fnv.New64a()
		_, _ = h.Write(b)
		if got[i] != h.Sum64() {
			t.Errorf("item %d hash = %d, want json.Marshal reference %d", i, got[i], h.Sum64())
		}
	}
}

func BenchmarkItemHashes(b *testing.B) {
	const itemBytes = 1 << 20
	input := []any{
		messageItem{Type: "message", Role: "user", Content: []contentPart{{Type: "input_text", Text: strings.Repeat("A realistic prompt with escaped \\\" text and ordinary content. ", itemBytes/64)}}},
		messageItem{Type: "message", Role: "assistant", Content: []contentPart{{Type: "input_text", Text: strings.Repeat("A realistic answer with ordinary content and a newline.\\n", itemBytes/58)}}},
	}
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "keyed", key: "benchmark-cache-key"},
		{name: "unkeyed"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			req := &Request{PromptCacheKey: tc.key, Input: input}
			b.ReportAllocs()
			b.SetBytes(int64(2 * itemBytes))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = ItemHashes(req)
			}
		})
	}
}
