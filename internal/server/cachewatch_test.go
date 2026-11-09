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

func promptForTest(f codex.Fingerprint, items []uint64) string {
	reg := promptWatch.See(f, items)
	return promptText(f, reg, time.Now())
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
		if got := w.See(tt.f, nil).text; got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCacheWatchNamesModelAndControlChanges(t *testing.T) {
	base := fp("settings-key", "system", "tools", 3)
	base.Model = "gpt-5-codex"
	base.Reasoning = "reasoning-hash"
	base.ToolChoice = "choice-hash"
	tests := []struct {
		name string
		edit func(*codex.Fingerprint)
		want string
	}{
		{"model", func(f *codex.Fingerprint) { f.Model = "gpt-5.1-codex" }, "same system prompt and tools as the last request of this key; model changed gpt-5-codex -> gpt-5.1-codex"},
		{"reasoning", func(f *codex.Fingerprint) { f.Reasoning = "new-reasoning-hash" }, "same system prompt and tools as the last request of this key; reasoning changed"},
		{"tool choice", func(f *codex.Fingerprint) { f.ToolChoice = "new-choice-hash" }, "same system prompt and tools as the last request of this key; tool choice changed"},
		{"all settings", func(f *codex.Fingerprint) {
			f.Model = "gpt-5.1-codex"
			f.Reasoning = "new-reasoning-hash"
			f.ToolChoice = "new-choice-hash"
		}, "same system prompt and tools as the last request of this key; model changed gpt-5-codex -> gpt-5.1-codex; reasoning changed; tool choice changed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &cacheWatch{}
			w.See(base, nil)
			changed := base
			tt.edit(&changed)
			if got := w.See(changed, nil).text; got != tt.want {
				t.Errorf("note = %q, want %q", got, tt.want)
			}
		})
	}
	w := &cacheWatch{}
	w.See(base, nil)
	if got, want := w.See(base, nil).text, "same system prompt and tools as the last request of this key"; got != want {
		t.Errorf("unchanged settings note = %q, want legacy text %q", got, want)
	}
}

func TestCacheWatchKeysAreIndependent(t *testing.T) {
	w := &cacheWatch{}
	w.See(fp("a", "s1", "t1", 5), nil)
	if got := w.See(fp("b", "s9", "t9", 5), nil).text; !strings.HasPrefix(got, "first request") {
		t.Errorf("another key is not new: %q", got)
	}
	if got := w.See(fp("a", "s1", "t1", 6), nil).text; !strings.HasPrefix(got, "same system prompt") {
		t.Errorf("a's own history was disturbed by b: %q", got)
	}
}

func TestCacheWatchIgnoresRequestsWithoutAKey(t *testing.T) {
	w := &cacheWatch{}
	if got := w.See(fp("", "s", "t", 3), nil).text; got != "" {
		t.Errorf("no key gave %q", got)
	}
	if len(w.last) != 0 || len(w.order) != 0 {
		t.Errorf("something was kept for a request without a key: %v %v", w.last, w.order)
	}
}

func TestCacheWatchIsBounded(t *testing.T) {
	w := &cacheWatch{}
	for i := range maxWatchedKeys + 100 {
		w.See(fp(fmt.Sprintf("key%05d", i), "s", "t", 1), nil)
	}
	if len(w.last) != maxWatchedKeys || len(w.order) != maxWatchedKeys {
		t.Fatalf("remembers %d keys (%d in order), want %d", len(w.last), len(w.order), maxWatchedKeys)
	}
	if got := w.See(fp("key00000", "s", "t", 1), nil).text; !strings.HasPrefix(got, "first request") {
		t.Errorf("the oldest key was kept: %q", got)
	}
	last := fmt.Sprintf("key%05d", maxWatchedKeys+99)
	if got := w.See(fp(last, "s", "t", 1), nil).text; !strings.HasPrefix(got, "same system prompt") {
		t.Errorf("the newest key was forgotten: %q", got)
	}
	before := len(w.order)
	for range 50 {
		w.See(fp(last, "s", "t", 1), nil)
	}
	if len(w.order) != before {
		t.Errorf("the order list grew from %d to %d on a known key", before, len(w.order))
	}
}

func TestCacheWatchIsSafeForConcurrentUse(t *testing.T) {
	w := &cacheWatch{}
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				w.See(fp(fmt.Sprintf("k%d", (g*7+i)%40), fmt.Sprintf("s%d", i%3), "t", i), nil)
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
	first := promptForTest(f, nil)
	if want := "key abcd1234, system 11111111, 3 tools 22222222, 7 input items; first request of this key since the server started"; first != want {
		t.Errorf("first = %q, want %q", first, want)
	}
	if second := promptForTest(f, nil); !strings.Contains(second, "; same system prompt and tools as the last request of this key") {
		t.Errorf("second = %q", second)
	}
	if got, want := promptForTest(fp("", "", "", 1), nil), "no cache key, no system prompt, no tools, 1 input items"; got != want {
		t.Errorf("no key: %q, want %q", got, want)
	}
}

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

func TestHistoryNote(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur []uint64
		want      string
	}{
		{"no hashes before", nil, []uint64{1}, ""},
		{"no hashes now", []uint64{1}, nil, ""},
		{"identical", []uint64{1, 2}, []uint64{1, 2}, "history identical to the last request"},
		{"extended", []uint64{1, 2}, []uint64{1, 2, 3, 4}, "history extends the last request by 2 items"},
		{"changed in the middle", []uint64{1, 2, 3}, []uint64{1, 9, 3, 4}, "history changed at item 1 of 4 (the last request had 3)"},
		{"changed first item", []uint64{1, 2}, []uint64{7, 2}, "history changed at item 0 of 2 (the last request had 2)"},
		{"lost its tail", []uint64{1, 2, 3}, []uint64{1, 2}, ""},
		{"shorter and changed", []uint64{1, 2, 3}, []uint64{1, 5}, "history changed at item 1 of 2 (the last request had 3)"},
		{"both empty", []uint64{}, []uint64{}, "history identical to the last request"},
	}
	for _, tt := range tests {
		if got := historyNote(tt.prev, tt.cur); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCacheWatchNamesTheFirstChangedItem(t *testing.T) {
	w := &cacheWatch{}
	w.See(fp("k", "s", "t", 3), []uint64{1, 2, 3})
	if got, want := w.See(fp("k", "s", "t", 5), []uint64{1, 2, 3, 4, 5}).text, "same system prompt and tools as the last request of this key; history extends the last request by 2 items"; got != want {
		t.Errorf("grown: %q, want %q", got, want)
	}
	if got, want := w.See(fp("k", "s", "t", 6), []uint64{1, 8, 3, 4, 5, 6}).text, "same system prompt and tools as the last request of this key; history changed at item 1 of 6 (the last request had 5)"; got != want {
		t.Errorf("changed: %q, want %q", got, want)
	}
	if got := w.See(fp("k", "s", "t", 7), []uint64{1, 8, 3, 4, 5, 6, 7}).text; !strings.HasSuffix(got, "history extends the last request by 1 items") {
		t.Errorf("after the change: %q", got)
	}
	if got, want := w.See(fp("k", "s", "t", 2), []uint64{1, 8}).text, "same system prompt and tools as the last request of this key; history shorter (7 -> 2 items)"; got != want {
		t.Errorf("shorter: %q, want %q", got, want)
	}
}

func TestPromptTextSaysWhichItemChanged(t *testing.T) {
	promptWatch = &cacheWatch{}
	t.Cleanup(func() { promptWatch = &cacheWatch{} })
	f := fp("abcd1234", "11111111", "22222222", 2)
	promptForTest(f, []uint64{4711, 8123})
	got := promptForTest(f, []uint64{4711, 9391})
	if want := "; same system prompt and tools as the last request of this key; history changed at item 1 of 2 (the last request had 2)"; !strings.Contains(got, want) {
		t.Errorf("line %q lacks %q", got, want)
	}
	if strings.Contains(got, "9391") || strings.Contains(got, "8123") || strings.Contains(got, "4711") {
		t.Errorf("a hash is in the line: %q", got)
	}
}

func TestCacheWatchSaysWhatTheLastRequestHad(t *testing.T) {
	w := &cacheWatch{}
	w.See(fp("k", "s", "t", 3), []uint64{1, 2, 3})
	second := w.See(fp("k", "s", "t", 5), []uint64{1, 2, 3, 4, 5})
	if strings.Contains(second.text, "tokens in") {
		t.Errorf("tokens before the reply ended: %q", second.text)
	}
	w.Done("k", second.generation, 711757, "")
	got := w.See(fp("k", "s", "t", 6), []uint64{1, 2, 3, 4, 5, 6}).text
	if !strings.HasSuffix(got, "; history extends the last request by 1 items; the last request had 711757 tokens in") {
		t.Errorf("note = %q", got)
	}
	if got := w.See(fp("k", "s", "t", 7), []uint64{1, 2, 3, 4, 5, 6, 7}).text; strings.Contains(got, "tokens in") {
		t.Errorf("the figure of an older request is kept: %q", got)
	}
}

func TestCacheWatchDoneUsesRequestGeneration(t *testing.T) {
	w := &cacheWatch{}
	f := fp("k", "s", "t", 4)
	a := w.See(f, []uint64{1, 2, 3, 4})
	b := w.See(f, []uint64{1, 2, 3, 4})
	bStart := w.last[f.Key].start
	if a.generation == b.generation {
		t.Fatal("equal fingerprints got the same generation")
	}
	w.Done(f.Key, a.generation, 9000, "LAX")
	third := w.See(f, []uint64{1, 2, 3, 4})
	if !third.prev.ended.IsZero() || third.prev.edge != "" || strings.Contains(third.text, "tokens in") {
		t.Fatalf("A's late completion changed running B: prev=%+v text=%q", third.prev, third.text)
	}
	if third.prev.start != bStart {
		t.Errorf("third predecessor is not B: got %v, want %v", third.prev.start, bStart)
	}
	if text := promptText(f, third, time.Now()); !strings.HasSuffix(text, "ago and had not ended") {
		t.Errorf("third request's timing: %q", text)
	}
}

func TestCacheWatchRegistrationReturnsCoherentPredecessor(t *testing.T) {
	w := &cacheWatch{}
	first := w.See(fp("k", "s", "t", 2), []uint64{1, 2})
	w.Done("k", first.generation, 640000, "LAX")
	firstWatched := w.last["k"]
	second := w.See(fp("k", "s", "t", 4), []uint64{1, 2, 3, 4})
	wantPrev := prevInfo{start: firstWatched.start, ended: firstWatched.ended, edge: firstWatched.edge}
	if second.prev != wantPrev {
		t.Fatalf("registration predecessor = %+v, want complete snapshot %+v", second.prev, wantPrev)
	}
	if !strings.Contains(second.text, "history extends the last request by 2 items; the last request had 640000 tokens in") {
		t.Errorf("comparison does not describe that predecessor: %q", second.text)
	}
	// A later registration before formatting must not replace B's atomic snapshot.
	snapshot := second.prev
	later := w.See(fp("k", "s", "t", 5), []uint64{1, 2, 3, 4, 5})
	if second.prev != snapshot {
		t.Errorf("later registration changed B's returned snapshot: got %+v, want %+v", second.prev, snapshot)
	}
	// A late completion racing after later registration must not mutate it.
	w.Done("k", first.generation, 700000, "PIT")
	if second.prev != snapshot {
		t.Errorf("late A completion changed B's returned snapshot: got %+v, want %+v", second.prev, snapshot)
	}
	if cur := w.last["k"]; cur.generation != later.generation || !cur.ended.IsZero() || cur.edge != "" {
		t.Errorf("late A completion changed later request: %+v", cur)
	}
	text := promptText(fp("k", "s", "t", 4), second, time.Now())
	if !strings.Contains(text, "ago, ended ") {
		t.Errorf("formatted text does not use returned predecessor: %q", text)
	}
}

func TestCacheWatchGenerationSurvivesEvictionAndReAdd(t *testing.T) {
	w := &cacheWatch{}
	first := w.See(fp("k", "s", "t", 1), nil)
	for i := range maxWatchedKeys {
		w.See(fp(fmt.Sprintf("other-%d", i), "s", "t", 1), nil)
	}
	readded := w.See(fp("k", "s", "t", 1), nil)
	if first.generation == readded.generation {
		t.Fatal("eviction and re-add reused a generation")
	}
	w.Done("k", first.generation, 1000, "LAX")
	third := w.See(fp("k", "s", "t", 2), nil)
	if !third.prev.ended.IsZero() || third.prev.edge != "" || strings.Contains(third.text, "tokens in") {
		t.Errorf("stale completion changed re-added key: prev=%+v text=%q", third.prev, third.text)
	}
}

func TestCacheWatchFullKeysAreIndependent(t *testing.T) {
	w := &cacheWatch{}
	keyA := codex.NewFingerprint(&codex.Request{PromptCacheKey: "01234567aaaaaaaaaaaaaaaaaaaaaaaa"})
	keyB := codex.NewFingerprint(&codex.Request{PromptCacheKey: "01234567bbbbbbbbbbbbbbbbbbbbbbbb"})
	keyA.System, keyA.Tools = "s", "t"
	keyB.System, keyB.Tools = "s", "t"
	w.See(keyA, nil)
	w.See(keyB, nil)
	if len(w.last) != 2 {
		t.Fatalf("prefix-colliding keys map to %d watched entries", len(w.last))
	}
	reg := w.See(fp(keyA.Key, "s", "t", 2), nil)
	if !strings.HasPrefix(reg.text, "same system prompt") {
		t.Errorf("key A was not independently found: %q", reg.text)
	}
	if got := w.See(fp(keyB.Key, "s", "t", 2), nil).text; !strings.HasPrefix(got, "same system prompt") {
		t.Errorf("key B was not independently found: %q", got)
	}
}

func TestCacheWatchTracksConcurrentGenerations(t *testing.T) {
	w := &cacheWatch{}
	key := "01234567aaaaaaaaaaaaaaaaaaaaaaaa"
	f := fp(key, "s", "t", 2)
	a := w.See(f, []uint64{1, 2})
	b := w.See(f, []uint64{1, 2})
	c := w.See(f, []uint64{1, 2})
	if a.running != 0 || b.running != 1 || c.running != 2 {
		t.Fatalf("running counts = %d, %d, %d; want 0, 1, 2", a.running, b.running, c.running)
	}
	for _, check := range []struct {
		reg  cacheRegistration
		want string
	}{
		{b, "1 other request of this key running"},
		{c, "2 other requests of this key running"},
	} {
		if got := promptText(f, check.reg, time.Now()); !strings.Contains(got, check.want) {
			t.Errorf("prompt %q lacks %q", got, check.want)
		}
	}

	w.Done(key, a.generation, 700, "LAX")
	w.Done(key, a.generation, 700, "LAX")
	d := w.See(f, []uint64{1, 2})
	if d.running != 2 {
		t.Fatalf("after A's duplicate Done, running = %d; want B and C", d.running)
	}
	if _, ok := w.active[key][a.generation]; ok {
		t.Fatal("A remained active after Done")
	}
	if _, ok := w.active[key][b.generation]; !ok {
		t.Fatal("B was removed by A's Done")
	}
	if _, ok := w.active[key][c.generation]; !ok {
		t.Fatal("C was removed by A's Done")
	}

	w.Done(key, b.generation, 0, "")
	if _, ok := w.active[key][b.generation]; ok {
		t.Fatal("data-free Done did not remove B")
	}
	if !w.last[key].ended.IsZero() {
		t.Fatal("data-free Done marked the current request ended")
	}
	e := w.See(f, []uint64{1, 2})
	if e.running != 2 {
		t.Fatalf("after data-free Done, running = %d; want C and D", e.running)
	}
	w.Finish(key, c.generation)
	if _, ok := w.active[key][c.generation]; ok {
		t.Fatal("Finish did not remove older C")
	}
	if !w.last[key].ended.IsZero() {
		t.Fatal("Finishing older C marked current D ended")
	}
	w.Finish(key, e.generation)
	if _, ok := w.active[key][e.generation]; ok {
		t.Fatal("Finish did not remove D")
	}
	if w.last[key].ended.IsZero() {
		t.Fatal("Finish did not mark current D ended")
	}
	ended := w.last[key].ended
	w.Finish(key, e.generation)
	if !w.last[key].ended.Equal(ended) {
		t.Fatal("duplicate Finish changed the completion time")
	}

	prefixA := "01234567bbbbbbbbbbbbbbbbbbbbbbbb"
	prefixB := "01234567cccccccccccccccccccccccc"
	firstA := w.See(fp(prefixA, "s", "t", 1), nil)
	firstB := w.See(fp(prefixB, "s", "t", 1), nil)
	if firstA.running != 0 || firstB.running != 0 {
		t.Fatalf("prefix-sharing keys saw each other's active requests: %d, %d", firstA.running, firstB.running)
	}
	if len(w.active[prefixA]) != 1 || len(w.active[prefixB]) != 1 {
		t.Fatalf("prefix-sharing active counts = %d, %d", len(w.active[prefixA]), len(w.active[prefixB]))
	}

	evictedKey := "eviction-key"
	old := w.See(fp(evictedKey, "s", "t", 1), nil)
	for i := range maxWatchedKeys {
		w.See(fp(fmt.Sprintf("filler-%d", i), "s", "t", 1), nil)
	}
	readded := w.See(fp(evictedKey, "s", "t", 1), nil)
	if readded.running != 1 {
		t.Fatalf("re-added key saw %d running requests; want old generation retained", readded.running)
	}
	w.Done(evictedKey, old.generation, 10, "LAX")
	afterOldDone := w.See(fp(evictedKey, "s", "t", 1), nil)
	if afterOldDone.running != 1 {
		t.Fatalf("old generation cleanup left %d active; want re-added generation only", afterOldDone.running)
	}
}

func TestCacheWatchMarksRunningHistoryBranches(t *testing.T) {
	tests := []struct {
		name       string
		prev, cur  []uint64
		complete   bool
		wantBranch bool
	}{
		{"active mismatch", []uint64{1, 2, 3}, []uint64{1, 9, 3}, false, true},
		{"identical", []uint64{1, 2}, []uint64{1, 2}, false, false},
		{"extension", []uint64{1, 2}, []uint64{1, 2, 3}, false, false},
		{"truncation", []uint64{1, 2, 3}, []uint64{1, 2}, false, false},
		{"no previous hashes", nil, []uint64{1}, false, false},
		{"no current hashes", []uint64{1}, nil, false, false},
		{"completed mismatch", []uint64{1, 2}, []uint64{1, 9}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &cacheWatch{}
			f := fp("branch-key", "s", "t", len(tt.prev))
			prev := w.See(f, tt.prev)
			if tt.complete {
				w.Finish(f.Key, prev.generation)
			}
			current := fp(f.Key, "s", "t", len(tt.cur))
			reg := w.See(current, tt.cur)
			if reg.branch != tt.wantBranch {
				t.Errorf("branch = %t, want %t", reg.branch, tt.wantBranch)
			}
			text := promptText(current, reg, time.Now())
			marker := "branch: history diverges from the last request while it is running"
			if strings.Contains(text, marker) != tt.wantBranch {
				t.Errorf("formatted branch marker in %q, want present=%t", text, tt.wantBranch)
			}
		})
	}
}
