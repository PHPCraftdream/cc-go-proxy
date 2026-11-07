package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

func fp(key, system, tools string, items int) codex.Fingerprint {
	return codex.Fingerprint{Key: key, System: system, Tools: tools, NTools: 3, Items: items}
}

func TestCacheWatchSaysWhatChangedSinceTheKeysLastRequest(t *testing.T) {
	w := &cacheWatch{}
	tests := []struct {
		name string
		f    codex.Fingerprint
		want string
	}{
		{"first", fp("k1", "s1", "t1", 10), "first request of this key since the server started"},
		{"nothing changed", fp("k1", "s1", "t1", 12), "same system prompt and tools as the last request of this key"},
		{"system changed", fp("k1", "s2", "t1", 14), "system prompt changed since the last request of this key"},
		{"tools changed", fp("k1", "s2", "t2", 16), "tools changed since the last request of this key"},
		{"both changed", fp("k1", "s3", "t3", 18), "system prompt and tools changed since the last request of this key"},
		{"history shrank", fp("k1", "s3", "t3", 4), "same system prompt and tools as the last request of this key; history shorter (18 -> 4 items)"},
		{"shrank and changed", fp("k1", "s4", "t3", 2), "system prompt changed since the last request of this key; history shorter (4 -> 2 items)"},
	}
	for _, tt := range tests {
		if got := w.See(tt.f); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCacheWatchKeysAreIndependent(t *testing.T) {
	w := &cacheWatch{}
	w.See(fp("a", "s1", "t1", 5))
	if got := w.See(fp("b", "s9", "t9", 5)); !strings.HasPrefix(got, "first request") {
		t.Errorf("another key is not new: %q", got)
	}
	if got := w.See(fp("a", "s1", "t1", 6)); !strings.HasPrefix(got, "same system prompt") {
		t.Errorf("a's own history was disturbed by b: %q", got)
	}
}

// A request without a key has nothing to be compared to, and nothing is kept for it.
func TestCacheWatchIgnoresRequestsWithoutAKey(t *testing.T) {
	w := &cacheWatch{}
	if got := w.See(fp("", "s", "t", 3)); got != "" {
		t.Errorf("no key gave %q", got)
	}
	if len(w.last) != 0 || len(w.order) != 0 {
		t.Errorf("something was kept for a request without a key: %v %v", w.last, w.order)
	}
}

// P0: the memory is bounded, and the oldest key goes first.
func TestCacheWatchIsBounded(t *testing.T) {
	w := &cacheWatch{}
	for i := range maxWatchedKeys + 100 {
		w.See(fp(fmt.Sprintf("key%05d", i), "s", "t", 1))
	}
	if len(w.last) != maxWatchedKeys || len(w.order) != maxWatchedKeys {
		t.Fatalf("remembers %d keys (%d in order), want %d", len(w.last), len(w.order), maxWatchedKeys)
	}
	if got := w.See(fp("key00000", "s", "t", 1)); !strings.HasPrefix(got, "first request") {
		t.Errorf("the oldest key was kept: %q", got)
	}
	last := fmt.Sprintf("key%05d", maxWatchedKeys+99)
	if got := w.See(fp(last, "s", "t", 1)); !strings.HasPrefix(got, "same system prompt") {
		t.Errorf("the newest key was forgotten: %q", got)
	}
	// Seeing a known key again does not grow the list.
	before := len(w.order)
	for range 50 {
		w.See(fp(last, "s", "t", 1))
	}
	if len(w.order) != before {
		t.Errorf("the order list grew from %d to %d on a known key", before, len(w.order))
	}
}

// P0: requests come from many goroutines.
func TestCacheWatchIsSafeForConcurrentUse(t *testing.T) {
	w := &cacheWatch{}
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				w.See(fp(fmt.Sprintf("k%d", (g*7+i)%40), fmt.Sprintf("s%d", i%3), "t", i))
			}
		}()
	}
	wg.Wait()
	if len(w.last) > maxWatchedKeys || len(w.last) != len(w.order) {
		t.Errorf("last = %d, order = %d", len(w.last), len(w.order))
	}
}

func TestPromptTextJoinsTheFingerprintAndTheNote(t *testing.T) {
	promptWatch = &cacheWatch{}
	t.Cleanup(func() { promptWatch = &cacheWatch{} })
	f := fp("abcd1234", "11111111", "22222222", 7)
	first := promptText(f)
	if want := "key abcd1234, system 11111111, 3 tools 22222222, 7 input items; first request of this key since the server started"; first != want {
		t.Errorf("first = %q, want %q", first, want)
	}
	if second := promptText(f); !strings.HasSuffix(second, "; same system prompt and tools as the last request of this key") {
		t.Errorf("second = %q", second)
	}
	// Without a key there is no note and no trailing separator.
	if got, want := promptText(fp("", "", "", 1)), "no cache key, no system prompt, no tools, 1 input items"; got != want {
		t.Errorf("no key: %q, want %q", got, want)
	}
}

// The line of a slow, failed or cancelled call ends with the prompt's fingerprint, after
// the tokens and the cache-miss mark; a trace without one is as it was.
func TestSummaryEndsWithThePrompt(t *testing.T) {
	start := time.Unix(1000, 0)
	u := &upstreamTrace{start: start, prompt: "key abcd1234, system 11111111, no tools, 9 input items; system prompt changed since the last request of this key"}
	got := u.summary(&codex.Translator{Usage: codex.Usage{Input: 500000, Output: 10}}, start.Add(time.Second), 10)
	miss := strings.Index(got, "| cache miss")
	prompt := strings.Index(got, "| prompt key abcd1234")
	if miss < 0 || prompt < miss || !strings.HasSuffix(got, "system prompt changed since the last request of this key") {
		t.Errorf("summary = %q", got)
	}
	u.prompt = ""
	if got = u.summary(&codex.Translator{}, start.Add(time.Second), 10); strings.Contains(got, "prompt") {
		t.Errorf("a trace without a prompt says one: %q", got)
	}
}
