package obs

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/sse"
)

func TestCacheStatsCountsBigPrompts(t *testing.T) {
	s := &CacheStats{}
	if got, _ := s.Take(); got != "" {
		t.Errorf("nothing counted, line %q", got)
	}
	s.Add(sse.Usage{Input: 100000, Cached: 100000})
	s.Add(sse.Usage{Input: 100000, Cached: 50000})
	s.Add(sse.Usage{Input: 100000, Cached: 0})
	s.Add(sse.Usage{Input: cacheMissMin - 1})      // too small to be cached
	s.Add(sse.Usage{})                             // no usage
	s.Add(sse.Usage{Input: 100000, Cached: 90000}) // exactly the share: whole
	want := "4 big prompts, 1 read from the cache in part (under 90%), 1 not at all; 60% of 400k input tokens came from the cache"
	if got, _ := s.Take(); got != want {
		t.Errorf("line %q, want %q", got, want)
	}
	// Taking starts the counts again.
	if got, _ := s.Take(); got != "" {
		t.Errorf("counts survived Take: %q", got)
	}
}

func TestCacheStatsIsSafeForConcurrentUse(t *testing.T) {
	s := &CacheStats{}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				s.Add(sse.Usage{Input: 20000, Cached: 10000})
			}
		}()
	}
	wg.Wait()
	if got, _ := s.Take(); !strings.HasPrefix(got, "3200 big prompts, 3200 read from the cache in part") {
		t.Errorf("line %q", got)
	}
}

func TestMillions(t *testing.T) {
	for in, want := range map[int64]string{0: "0k", 999: "0k", 812345: "812k", 999999: "999k", 1_000_000: "1.0M", 23_400_000: "23.4M"} {
		if got := millions(in); got != want {
			t.Errorf("millions(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteCacheStats(t *testing.T) {
	old := PromptStats
	PromptStats = &CacheStats{}
	t.Cleanup(func() { PromptStats = old })
	var b bytes.Buffer
	WriteCacheStats(&b, time.Unix(0, 0))
	if b.Len() != 0 {
		t.Errorf("a quiet period logged %q", b.String())
	}
	PromptStats.Add(sse.Usage{Input: 640000, Cached: 600000})
	WriteCacheStats(&b, time.Date(2026, 11, 8, 9, 10, 11, 0, time.Local))
	if got := b.String(); !strings.HasPrefix(got, "[09:10:11] upstream cache [codex]: 1 big prompts, 0 read from the cache in part") || !strings.HasSuffix(got, "93% of 640k input tokens came from the cache\n") {
		t.Errorf("line %q", got)
	}
	b.Reset()
	WriteCacheStats(&b, time.Now())
	if b.Len() != 0 {
		t.Errorf("the counts were not reset: %q", b.String())
	}
}

func TestEdgeOf(t *testing.T) {
	for ray, want := range map[string]string{
		"a56de7373f3d69dc-DFW": "DFW",
		"abc-def-LAX":          "LAX",
		"":                     "",
		"nodash":               "",
		"trailing-":            "",
	} {
		h := http.Header{}
		if ray != "" {
			h.Set("Cf-Ray", ray)
		}
		if got := EdgeOf(h); got != want {
			t.Errorf("edgeOf(%q) = %q, want %q", ray, got, want)
		}
	}
}

func TestEdgeText(t *testing.T) {
	for _, c := range []struct{ prev, cur, want string }{
		{"LAX", "PIT", "edge LAX -> PIT"},
		{"LAX", "LAX", "edge same (LAX)"},
		{"", "LAX", ""},
		{"LAX", "", ""},
		{"", "", ""},
	} {
		if got := edgeText(c.prev, c.cur); got != c.want {
			t.Errorf("edgeText(%q, %q) = %q, want %q", c.prev, c.cur, got, c.want)
		}
	}
}

func TestCacheWatchKeepsTheEdge(t *testing.T) {
	w := &CacheWatch{}
	f := fp("k", "s", "t", 3)
	reg := w.See(f, nil)
	if got := reg.Prev.Edge; got != "" {
		t.Errorf("edge before the reply: %q", got)
	}
	w.Done(f.Key, reg.Generation, 0, "LAX") // an edge alone is kept, without tokens
	if got := w.Last[f.Key].edge; got != "LAX" {
		t.Errorf("edge = %q", got)
	}
	w.Done(f.Key, reg.Generation, 5000, "") // tokens do not wipe the edge
	if got := w.Last[f.Key].edge; got != "LAX" {
		t.Errorf("edge after tokens = %q", got)
	}
	// A later request sees the completed request's edge as its predecessor,
	// while the newly registered request starts without its own edge.
	next := w.See(fp("k", "s", "t", 4), nil)
	if got := next.Prev.Edge; got != "LAX" {
		t.Errorf("predecessor edge = %q, want LAX", got)
	}
	if got := w.Last[f.Key].edge; got != "" {
		t.Errorf("new request inherited the edge: %q", got)
	}
	if got := w.See(fp("other", "s", "t", 1), nil).Prev.Edge; got != "" {
		t.Errorf("unknown key: %q", got)
	}
	// An older request's edge does not land on a newer one.
	w.Done(f.Key, reg.Generation, 0, "PIT")
	if got := w.Last[f.Key].edge; got != "" {
		t.Errorf("an older reply set the current edge: %q", got)
	}
}

func TestSummaryShowsTheEdge(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, c := range []struct{ prev, cur, want string }{
		{"LAX", "PIT", "; edge LAX -> PIT"},
		{"LAX", "LAX", "; edge same (LAX)"},
		{"", "LAX", ""},
	} {
		u := &UpstreamTrace{Start: start, Prompt: "key abcd1234, no system prompt, no tools, 9 input items", Prev: prevInfo{Edge: c.prev}, Edge: c.cur}
		got := u.Summary(&sse.Translator{}, start.Add(time.Second), 10)
		if c.want == "" {
			if strings.Contains(got, "edge") {
				t.Errorf("%+v: %q", c, got)
			}
			continue
		}
		if !strings.HasSuffix(got, "| prompt key abcd1234, no system prompt, no tools, 9 input items"+c.want) {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

func TestTimingText(t *testing.T) {
	now := time.Unix(10000, 0)
	for _, c := range []struct {
		name string
		p    prevInfo
		want string
	}{
		{"none", prevInfo{}, ""},
		{"ended", prevInfo{start: now.Add(-499 * time.Second), ended: now.Add(-12 * time.Second)}, "the last request started 8m19s ago, ended 12s ago"},
		{"still running", prevInfo{start: now.Add(-90 * time.Second)}, "the last request started 1m30s ago and had not ended"},
		{"rounded", prevInfo{start: now.Add(-1400 * time.Millisecond), ended: now.Add(-400 * time.Millisecond)}, "the last request started 1s ago, ended 0s ago"},
	} {
		if got := timingText(c.p, now); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCacheWatchRemembersWhenTheLastRequestRan(t *testing.T) {
	w := &CacheWatch{}
	f := fp("k", "s", "t", 3)
	before := time.Now()
	if p := w.See(f, nil).Prev; !p.start.IsZero() || !p.ended.IsZero() || p.Edge != "" {
		t.Errorf("an unknown key has a past: %+v", p)
	}
	reg := w.See(f, nil)
	if reg.Prev.start.Before(before) || reg.Prev.start.After(time.Now()) || !reg.Prev.ended.IsZero() {
		t.Errorf("after See: %+v", reg.Prev)
	}
	w.Done(f.Key, reg.Generation, 0, "")
	if p := w.Last["k"]; !p.ended.IsZero() {
		t.Errorf("a Done without data ended the request: %+v", p)
	}
	w.Done(f.Key, reg.Generation, 5000, "LAX")
	p := w.Last["k"]
	if p.ended.Before(p.start) || p.edge != "LAX" {
		t.Errorf("after Done: %+v", p)
	}
	if p := w.See(fp("k", "s", "t", 4), nil).Prev; p.ended.IsZero() || p.Edge != "LAX" {
		t.Errorf("new request's predecessor lost the end or edge: %+v", p)
	}
	if p := w.Last["k"]; !p.ended.IsZero() || p.edge != "" {
		t.Errorf("new request inherited the predecessor's end: %+v", p)
	}
}

func promptForCacheStatsTest(f codex.Fingerprint) string {
	reg := PromptWatch.See(f, nil)
	return PromptText(f, reg, time.Now())
}

// The prompt note of the next request says when the last one ran.
func TestPromptTextSaysWhenTheLastRequestRan(t *testing.T) {
	PromptWatch = &CacheWatch{}
	t.Cleanup(func() { PromptWatch = &CacheWatch{} })
	f := fp("abcd1234", "11111111", "22222222", 2)
	if first := promptForCacheStatsTest(f); strings.Contains(first, "the last request") && !strings.Contains(first, "first request of this key") {
		t.Errorf("a first request has a past: %q", first)
	}
	if running := promptForCacheStatsTest(f); !strings.Contains(running, "; the last request started ") || !strings.HasSuffix(running, " ago and had not ended") {
		t.Errorf("running: %q", running)
	}
	reg := PromptWatch.See(f, nil)
	PromptWatch.Done(f.Key, reg.Generation, 1000, "")
	if ended := promptForCacheStatsTest(f); !strings.Contains(ended, "; the last request started ") || !strings.Contains(ended, " ago, ended ") || strings.HasSuffix(ended, "had not ended") {
		t.Errorf("ended: %q", ended)
	}
}

func TestGapGroup(t *testing.T) {
	start := time.Unix(100000, 0)
	for _, c := range []struct {
		name string
		prev prevInfo
		want string
	}{
		{"no past", prevInfo{}, ""},
		{"just now", prevInfo{start: start}, groupGapShort},
		{"4m59s", prevInfo{start: start.Add(-299 * time.Second)}, groupGapShort},
		{"5m", prevInfo{start: start.Add(-5 * time.Minute)}, groupGapMiddle},
		{"9m59s", prevInfo{start: start.Add(-599 * time.Second)}, groupGapMiddle},
		{"10m", prevInfo{start: start.Add(-10 * time.Minute)}, groupGapLong},
		{"13m40s", prevInfo{start: start.Add(-820 * time.Second)}, groupGapLong},
	} {
		if got := GapGroup(c.prev, start); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEdgeGroup(t *testing.T) {
	for _, c := range []struct{ prev, cur, want string }{
		{"LAX", "LAX", groupEdgeSame},
		{"LAX", "PIT", groupEdgeOther},
		{"", "PIT", ""},
		{"LAX", "", ""},
	} {
		if got := EdgeGroup(c.prev, c.cur); got != c.want {
			t.Errorf("edgeGroup(%q, %q) = %q, want %q", c.prev, c.cur, got, c.want)
		}
	}
}

func TestCacheStatsGroups(t *testing.T) {
	s := &CacheStats{}
	s.Add(sse.Usage{Input: 100000, Cached: 100000}, groupGapShort, groupEdgeSame)
	s.Add(sse.Usage{Input: 100000, Cached: 50000}, groupGapShort, "")
	s.Add(sse.Usage{Input: 200000, Cached: 20000}, groupGapLong, groupEdgeOther)
	s.Add(sse.Usage{Input: 100}, groupGapLong, groupEdgeOther) // too small: nowhere
	total, by := s.Take()
	if !strings.HasPrefix(total, "3 big prompts, 2 read from the cache in part (under 90%), 0 not at all; 42% of 400k") {
		t.Errorf("total %q", total)
	}
	// Fixed order: gaps, then edges; a group nobody was in is left out.
	want := "started <5m before: 2 prompts, 75% cached; started >10m before: 1 prompts, 10% cached; same edge: 1 prompts, 100% cached; other edge: 1 prompts, 10% cached"
	if by != want {
		t.Errorf("groups %q, want %q", by, want)
	}
	if total, by = s.Take(); total != "" || by != "" {
		t.Errorf("counts survived Take: %q %q", total, by)
	}
	// Prompts without any group still give a total and no group text.
	s.Add(sse.Usage{Input: 100000, Cached: 100000})
	if total, by = s.Take(); total == "" || by != "" {
		t.Errorf("no groups: %q %q", total, by)
	}
}

func TestCacheStatsTurnStateGroups(t *testing.T) {
	for sent, want := range map[bool]string{
		true:  groupTurnStateSent,
		false: groupTurnStateNotSent,
	} {
		if got := TurnStateGroup(sent); got != want {
			t.Errorf("turnStateGroup(%t) = %q, want %q", sent, got, want)
		}
	}

	s := &CacheStats{}
	s.Add(sse.Usage{Input: 100000, Cached: 100000}, groupGapShort, groupEdgeSame, TurnStateGroup(true))
	s.Add(sse.Usage{Input: 200000, Cached: 50000}, groupGapLong, groupEdgeOther, TurnStateGroup(false))
	s.Add(sse.Usage{Input: 200000, Cached: 100000}, groupGapMiddle)
	_, by := s.Take()
	want := "started <5m before: 1 prompts, 100% cached; started 5-10m before: 1 prompts, 50% cached; started >10m before: 1 prompts, 25% cached; same edge: 1 prompts, 100% cached; other edge: 1 prompts, 25% cached; turn state sent: 1 prompts, 100% cached; turn state not sent: 1 prompts, 25% cached"
	if by != want {
		t.Errorf("groups %q, want %q", by, want)
	}
}

func TestWriteCacheStatsPrintsTheGroups(t *testing.T) {
	old := PromptStats
	PromptStats = &CacheStats{}
	t.Cleanup(func() { PromptStats = old })
	PromptStats.Add(sse.Usage{Input: 640000, Cached: 600000}, groupGapShort)
	var b bytes.Buffer
	WriteCacheStats(&b, time.Date(2026, 11, 8, 9, 10, 11, 0, time.Local))
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "[09:10:11] upstream cache [codex]: 1 big prompts") ||
		lines[1] != "[09:10:11] upstream cache by [codex]: started <5m before: 1 prompts, 93% cached" {
		t.Errorf("lines %q", lines)
	}
}

func fp(key, system, tools string, items int) codex.Fingerprint {
	return codex.Fingerprint{Key: key, System: system, Tools: tools, NTools: 3, Items: items}
}

func promptForTest(f codex.Fingerprint, items []uint64) string {
	reg := PromptWatch.See(f, items)
	return PromptText(f, reg, time.Now())
}

func TestCacheWatchSaysWhatChangedSinceTheKeysLastRequest(t *testing.T) {
	w := &CacheWatch{}
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
			w := &CacheWatch{}
			w.See(base, nil)
			changed := base
			tt.edit(&changed)
			if got := w.See(changed, nil).text; got != tt.want {
				t.Errorf("note = %q, want %q", got, tt.want)
			}
		})
	}
	w := &CacheWatch{}
	w.See(base, nil)
	if got, want := w.See(base, nil).text, "same system prompt and tools as the last request of this key"; got != want {
		t.Errorf("unchanged settings note = %q, want legacy text %q", got, want)
	}
}

func TestCacheWatchKeysAreIndependent(t *testing.T) {
	w := &CacheWatch{}
	w.See(fp("a", "s1", "t1", 5), nil)
	if got := w.See(fp("b", "s9", "t9", 5), nil).text; !strings.HasPrefix(got, "first request") {
		t.Errorf("another key is not new: %q", got)
	}
	if got := w.See(fp("a", "s1", "t1", 6), nil).text; !strings.HasPrefix(got, "same system prompt") {
		t.Errorf("a's own history was disturbed by b: %q", got)
	}
}

func TestCacheWatchIgnoresRequestsWithoutAKey(t *testing.T) {
	w := &CacheWatch{}
	if got := w.See(fp("", "s", "t", 3), nil).text; got != "" {
		t.Errorf("no key gave %q", got)
	}
	if len(w.Last) != 0 || len(w.order) != 0 {
		t.Errorf("something was kept for a request without a key: %v %v", w.Last, w.order)
	}
}

func TestCacheWatchIsBounded(t *testing.T) {
	w := &CacheWatch{}
	for i := range maxWatchedKeys + 100 {
		w.See(fp(fmt.Sprintf("key%05d", i), "s", "t", 1), nil)
	}
	if len(w.Last) != maxWatchedKeys || len(w.order) != maxWatchedKeys {
		t.Fatalf("remembers %d keys (%d in order), want %d", len(w.Last), len(w.order), maxWatchedKeys)
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
	w := &CacheWatch{}
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
	if len(w.Last) > maxWatchedKeys || len(w.Last) != len(w.order) {
		t.Errorf("last = %d, order = %d", len(w.Last), len(w.order))
	}
}

func TestPromptTextJoinsTheFingerprintAndTheNote(t *testing.T) {
	PromptWatch = &CacheWatch{}
	t.Cleanup(func() { PromptWatch = &CacheWatch{} })
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
	u := &UpstreamTrace{Start: start, Prompt: "key abcd1234, system 11111111, no tools, 9 input items; system prompt changed since the last request of this key"}
	got := u.Summary(&sse.Translator{Usage: sse.Usage{Input: 500000, Output: 10}}, start.Add(time.Second), 10)
	miss := strings.Index(got, "| cache miss")
	prompt := strings.Index(got, "| prompt key abcd1234")
	if miss < 0 || prompt < miss || !strings.HasSuffix(got, "system prompt changed since the last request of this key") {
		t.Errorf("summary = %q", got)
	}
	u.Prompt = ""
	if got = u.Summary(&sse.Translator{}, start.Add(time.Second), 10); strings.Contains(got, "prompt") {
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
	w := &CacheWatch{}
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
	PromptWatch = &CacheWatch{}
	t.Cleanup(func() { PromptWatch = &CacheWatch{} })
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
	w := &CacheWatch{}
	w.See(fp("k", "s", "t", 3), []uint64{1, 2, 3})
	second := w.See(fp("k", "s", "t", 5), []uint64{1, 2, 3, 4, 5})
	if strings.Contains(second.text, "tokens in") {
		t.Errorf("tokens before the reply ended: %q", second.text)
	}
	w.Done("k", second.Generation, 711757, "")
	got := w.See(fp("k", "s", "t", 6), []uint64{1, 2, 3, 4, 5, 6}).text
	if !strings.HasSuffix(got, "; history extends the last request by 1 items; the last request had 711757 tokens in") {
		t.Errorf("note = %q", got)
	}
	if got := w.See(fp("k", "s", "t", 7), []uint64{1, 2, 3, 4, 5, 6, 7}).text; strings.Contains(got, "tokens in") {
		t.Errorf("the figure of an older request is kept: %q", got)
	}
}

func TestCacheWatchDoneUsesRequestGeneration(t *testing.T) {
	w := &CacheWatch{}
	f := fp("k", "s", "t", 4)
	a := w.See(f, []uint64{1, 2, 3, 4})
	b := w.See(f, []uint64{1, 2, 3, 4})
	bStart := w.Last[f.Key].start
	if a.Generation == b.Generation {
		t.Fatal("equal fingerprints got the same generation")
	}
	w.Done(f.Key, a.Generation, 9000, "LAX")
	third := w.See(f, []uint64{1, 2, 3, 4})
	if !third.Prev.ended.IsZero() || third.Prev.Edge != "" || strings.Contains(third.text, "tokens in") {
		t.Fatalf("A's late completion changed running B: prev=%+v text=%q", third.Prev, third.text)
	}
	if third.Prev.start != bStart {
		t.Errorf("third predecessor is not B: got %v, want %v", third.Prev.start, bStart)
	}
	if text := PromptText(f, third, time.Now()); !strings.HasSuffix(text, "ago and had not ended") {
		t.Errorf("third request's timing: %q", text)
	}
}

func TestCacheWatchRegistrationReturnsCoherentPredecessor(t *testing.T) {
	w := &CacheWatch{}
	first := w.See(fp("k", "s", "t", 2), []uint64{1, 2})
	w.Done("k", first.Generation, 640000, "LAX")
	firstWatched := w.Last["k"]
	second := w.See(fp("k", "s", "t", 4), []uint64{1, 2, 3, 4})
	wantPrev := prevInfo{start: firstWatched.start, ended: firstWatched.ended, Edge: firstWatched.edge}
	if second.Prev != wantPrev {
		t.Fatalf("registration predecessor = %+v, want complete snapshot %+v", second.Prev, wantPrev)
	}
	if !strings.Contains(second.text, "history extends the last request by 2 items; the last request had 640000 tokens in") {
		t.Errorf("comparison does not describe that predecessor: %q", second.text)
	}
	// A later registration before formatting must not replace B's atomic snapshot.
	snapshot := second.Prev
	later := w.See(fp("k", "s", "t", 5), []uint64{1, 2, 3, 4, 5})
	if second.Prev != snapshot {
		t.Errorf("later registration changed B's returned snapshot: got %+v, want %+v", second.Prev, snapshot)
	}
	// A late completion racing after later registration must not mutate it.
	w.Done("k", first.Generation, 700000, "PIT")
	if second.Prev != snapshot {
		t.Errorf("late A completion changed B's returned snapshot: got %+v, want %+v", second.Prev, snapshot)
	}
	if cur := w.Last["k"]; cur.generation != later.Generation || !cur.ended.IsZero() || cur.edge != "" {
		t.Errorf("late A completion changed later request: %+v", cur)
	}
	text := PromptText(fp("k", "s", "t", 4), second, time.Now())
	if !strings.Contains(text, "ago, ended ") {
		t.Errorf("formatted text does not use returned predecessor: %q", text)
	}
}

func TestCacheWatchGenerationSurvivesEvictionAndReAdd(t *testing.T) {
	w := &CacheWatch{}
	first := w.See(fp("k", "s", "t", 1), nil)
	for i := range maxWatchedKeys {
		w.See(fp(fmt.Sprintf("other-%d", i), "s", "t", 1), nil)
	}
	readded := w.See(fp("k", "s", "t", 1), nil)
	if first.Generation == readded.Generation {
		t.Fatal("eviction and re-add reused a generation")
	}
	w.Done("k", first.Generation, 1000, "LAX")
	third := w.See(fp("k", "s", "t", 2), nil)
	if !third.Prev.ended.IsZero() || third.Prev.Edge != "" || strings.Contains(third.text, "tokens in") {
		t.Errorf("stale completion changed re-added key: prev=%+v text=%q", third.Prev, third.text)
	}
}

func TestCacheWatchFullKeysAreIndependent(t *testing.T) {
	w := &CacheWatch{}
	keyA := codex.NewFingerprint(&codex.Request{PromptCacheKey: "01234567aaaaaaaaaaaaaaaaaaaaaaaa"})
	keyB := codex.NewFingerprint(&codex.Request{PromptCacheKey: "01234567bbbbbbbbbbbbbbbbbbbbbbbb"})
	keyA.System, keyA.Tools = "s", "t"
	keyB.System, keyB.Tools = "s", "t"
	w.See(keyA, nil)
	w.See(keyB, nil)
	if len(w.Last) != 2 {
		t.Fatalf("prefix-colliding keys map to %d watched entries", len(w.Last))
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
	w := &CacheWatch{}
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
		if got := PromptText(f, check.reg, time.Now()); !strings.Contains(got, check.want) {
			t.Errorf("prompt %q lacks %q", got, check.want)
		}
	}

	w.Done(key, a.Generation, 700, "LAX")
	w.Done(key, a.Generation, 700, "LAX")
	d := w.See(f, []uint64{1, 2})
	if d.running != 2 {
		t.Fatalf("after A's duplicate Done, running = %d; want B and C", d.running)
	}
	if _, ok := w.Active[key][a.Generation]; ok {
		t.Fatal("A remained active after Done")
	}
	if _, ok := w.Active[key][b.Generation]; !ok {
		t.Fatal("B was removed by A's Done")
	}
	if _, ok := w.Active[key][c.Generation]; !ok {
		t.Fatal("C was removed by A's Done")
	}

	w.Done(key, b.Generation, 0, "")
	if _, ok := w.Active[key][b.Generation]; ok {
		t.Fatal("data-free Done did not remove B")
	}
	if !w.Last[key].ended.IsZero() {
		t.Fatal("data-free Done marked the current request ended")
	}
	e := w.See(f, []uint64{1, 2})
	if e.running != 2 {
		t.Fatalf("after data-free Done, running = %d; want C and D", e.running)
	}
	w.Finish(key, c.Generation)
	if _, ok := w.Active[key][c.Generation]; ok {
		t.Fatal("Finish did not remove older C")
	}
	if !w.Last[key].ended.IsZero() {
		t.Fatal("Finishing older C marked current D ended")
	}
	w.Finish(key, e.Generation)
	if _, ok := w.Active[key][e.Generation]; ok {
		t.Fatal("Finish did not remove D")
	}
	if w.Last[key].ended.IsZero() {
		t.Fatal("Finish did not mark current D ended")
	}
	ended := w.Last[key].ended
	w.Finish(key, e.Generation)
	if !w.Last[key].ended.Equal(ended) {
		t.Fatal("duplicate Finish changed the completion time")
	}

	prefixA := "01234567bbbbbbbbbbbbbbbbbbbbbbbb"
	prefixB := "01234567cccccccccccccccccccccccc"
	firstA := w.See(fp(prefixA, "s", "t", 1), nil)
	firstB := w.See(fp(prefixB, "s", "t", 1), nil)
	if firstA.running != 0 || firstB.running != 0 {
		t.Fatalf("prefix-sharing keys saw each other's active requests: %d, %d", firstA.running, firstB.running)
	}
	if len(w.Active[prefixA]) != 1 || len(w.Active[prefixB]) != 1 {
		t.Fatalf("prefix-sharing active counts = %d, %d", len(w.Active[prefixA]), len(w.Active[prefixB]))
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
	w.Done(evictedKey, old.Generation, 10, "LAX")
	afterOldDone := w.See(fp(evictedKey, "s", "t", 1), nil)
	if afterOldDone.running != 1 {
		t.Fatalf("old generation cleanup left %d active; want re-added generation only", afterOldDone.running)
	}
}
