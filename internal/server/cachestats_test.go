package server

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

func TestCacheStatsCountsBigPrompts(t *testing.T) {
	s := &cacheStats{}
	if got, _ := s.Take(); got != "" {
		t.Errorf("nothing counted, line %q", got)
	}
	s.Add(codex.Usage{Input: 100000, Cached: 100000})
	s.Add(codex.Usage{Input: 100000, Cached: 50000})
	s.Add(codex.Usage{Input: 100000, Cached: 0})
	s.Add(codex.Usage{Input: cacheMissMin - 1})      // too small to be cached
	s.Add(codex.Usage{})                             // no usage
	s.Add(codex.Usage{Input: 100000, Cached: 90000}) // exactly the share: whole
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
	s := &cacheStats{}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				s.Add(codex.Usage{Input: 20000, Cached: 10000})
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
	old := promptStats
	promptStats = &cacheStats{}
	t.Cleanup(func() { promptStats = old })
	var b bytes.Buffer
	writeCacheStats(&b, time.Unix(0, 0))
	if b.Len() != 0 {
		t.Errorf("a quiet period logged %q", b.String())
	}
	promptStats.Add(codex.Usage{Input: 640000, Cached: 600000})
	writeCacheStats(&b, time.Date(2026, 11, 8, 9, 10, 11, 0, time.Local))
	if got := b.String(); !strings.HasPrefix(got, "[09:10:11] upstream cache [codex]: 1 big prompts, 0 read from the cache in part") || !strings.HasSuffix(got, "93% of 640k input tokens came from the cache\n") {
		t.Errorf("line %q", got)
	}
	b.Reset()
	writeCacheStats(&b, time.Now())
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
		if got := edgeOf(h); got != want {
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
	w := &cacheWatch{}
	f := fp("k", "s", "t", 3)
	reg := w.See(f, nil)
	if got := reg.prev.edge; got != "" {
		t.Errorf("edge before the reply: %q", got)
	}
	w.Done(f.Key, reg.generation, 0, "LAX") // an edge alone is kept, without tokens
	if got := w.last[f.Key].edge; got != "LAX" {
		t.Errorf("edge = %q", got)
	}
	w.Done(f.Key, reg.generation, 5000, "") // tokens do not wipe the edge
	if got := w.last[f.Key].edge; got != "LAX" {
		t.Errorf("edge after tokens = %q", got)
	}
	// A later request sees the completed request's edge as its predecessor,
	// while the newly registered request starts without its own edge.
	next := w.See(fp("k", "s", "t", 4), nil)
	if got := next.prev.edge; got != "LAX" {
		t.Errorf("predecessor edge = %q, want LAX", got)
	}
	if got := w.last[f.Key].edge; got != "" {
		t.Errorf("new request inherited the edge: %q", got)
	}
	if got := w.See(fp("other", "s", "t", 1), nil).prev.edge; got != "" {
		t.Errorf("unknown key: %q", got)
	}
	// An older request's edge does not land on a newer one.
	w.Done(f.Key, reg.generation, 0, "PIT")
	if got := w.last[f.Key].edge; got != "" {
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
		u := &upstreamTrace{start: start, prompt: "key abcd1234, no system prompt, no tools, 9 input items", prev: prevInfo{edge: c.prev}, edge: c.cur}
		got := u.summary(&codex.Translator{}, start.Add(time.Second), 10)
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

// A request tells the next one of its key which data centre answered; the next
// failed or slow line then says whether the edge changed.
func TestEdgeFlowsFromOneRequestToTheNext(t *testing.T) {
	promptWatch = &cacheWatch{}
	promptStats = &cacheStats{}
	t.Cleanup(func() { promptWatch, promptStats = &cacheWatch{}, &cacheStats{} })
	var buf bytes.Buffer
	old := logOut
	logOut = &buf
	t.Cleanup(func() { logOut = old })

	f1 := codex.Fingerprint{Key: "abcd1234", System: "s", Tools: "t", NTools: 1, Items: 4}
	t1 := newUpstreamTrace()
	t1.fp = f1
	reg1 := promptWatch.See(f1, []uint64{1, 2, 3, 4})
	t1.prev = reg1.prev
	t1.promptGeneration = reg1.generation
	t1.prompt = promptText(f1, reg1, time.Now())
	logUpstreamEnd(context.Background(), &codex.Translator{Usage: codex.Usage{Input: 640000, Cached: 600000}}, t1,
		&http.Response{Header: http.Header{"Cf-Ray": {"r1-LAX"}}}, "codex", "a -> b", 10)
	if buf.Len() != 0 {
		t.Fatalf("a fast reply was logged: %q", buf.String())
	}

	f2 := codex.Fingerprint{Key: "abcd1234", System: "s", Tools: "t", NTools: 1, Items: 6}
	t2 := newUpstreamTrace()
	t2.fp = f2
	reg2 := promptWatch.See(f2, []uint64{1, 2, 3, 4, 5, 6})
	t2.prev = reg2.prev
	t2.promptGeneration = reg2.generation
	t2.prompt = promptText(f2, reg2, time.Now())
	tr := &codex.Translator{Failed: "boom", Usage: codex.Usage{Input: 650000, Cached: 400000}}
	logUpstreamEnd(context.Background(), tr, t2, &http.Response{Header: http.Header{"Cf-Ray": {"r2-PIT"}}}, "codex", "a -> b", 10)
	got := buf.String()
	if !strings.Contains(got, "; edge LAX -> PIT") || !strings.Contains(got, "cache partial 61% (400000 of 650000)") {
		t.Errorf("line %q", got)
	}
	// Both replies went into the counters.
	line, by := promptStats.Take()
	if !strings.HasPrefix(line, "2 big prompts, 1 read from the cache in part") {
		t.Errorf("counters: %q", line)
	}
	// The second reply has known gap/edge groups and both replies are not sent with turn state.
	if by != "started <5m before: 1 prompts, 61% cached; other edge: 1 prompts, 61% cached; turn state not sent: 2 prompts, 77% cached" {
		t.Errorf("groups: %q", by)
	}
	if !strings.Contains(got, "; the last request started ") || !strings.Contains(got, " ago, ended ") {
		t.Errorf("no timing of the last request: %q", got)
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
	w := &cacheWatch{}
	f := fp("k", "s", "t", 3)
	before := time.Now()
	if p := w.See(f, nil).prev; !p.start.IsZero() || !p.ended.IsZero() || p.edge != "" {
		t.Errorf("an unknown key has a past: %+v", p)
	}
	reg := w.See(f, nil)
	if reg.prev.start.Before(before) || reg.prev.start.After(time.Now()) || !reg.prev.ended.IsZero() {
		t.Errorf("after See: %+v", reg.prev)
	}
	w.Done(f.Key, reg.generation, 0, "")
	if p := w.last["k"]; !p.ended.IsZero() {
		t.Errorf("a Done without data ended the request: %+v", p)
	}
	w.Done(f.Key, reg.generation, 5000, "LAX")
	p := w.last["k"]
	if p.ended.Before(p.start) || p.edge != "LAX" {
		t.Errorf("after Done: %+v", p)
	}
	if p := w.See(fp("k", "s", "t", 4), nil).prev; p.ended.IsZero() || p.edge != "LAX" {
		t.Errorf("new request's predecessor lost the end or edge: %+v", p)
	}
	if p := w.last["k"]; !p.ended.IsZero() || p.edge != "" {
		t.Errorf("new request inherited the predecessor's end: %+v", p)
	}
}

func promptForCacheStatsTest(f codex.Fingerprint) string {
	reg := promptWatch.See(f, nil)
	return promptText(f, reg, time.Now())
}

// The prompt note of the next request says when the last one ran.
func TestPromptTextSaysWhenTheLastRequestRan(t *testing.T) {
	promptWatch = &cacheWatch{}
	t.Cleanup(func() { promptWatch = &cacheWatch{} })
	f := fp("abcd1234", "11111111", "22222222", 2)
	if first := promptForCacheStatsTest(f); strings.Contains(first, "the last request") && !strings.Contains(first, "first request of this key") {
		t.Errorf("a first request has a past: %q", first)
	}
	if running := promptForCacheStatsTest(f); !strings.Contains(running, "; the last request started ") || !strings.HasSuffix(running, " ago and had not ended") {
		t.Errorf("running: %q", running)
	}
	reg := promptWatch.See(f, nil)
	promptWatch.Done(f.Key, reg.generation, 1000, "")
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
		if got := gapGroup(c.prev, start); got != c.want {
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
		if got := edgeGroup(c.prev, c.cur); got != c.want {
			t.Errorf("edgeGroup(%q, %q) = %q, want %q", c.prev, c.cur, got, c.want)
		}
	}
}

func TestCacheStatsGroups(t *testing.T) {
	s := &cacheStats{}
	s.Add(codex.Usage{Input: 100000, Cached: 100000}, groupGapShort, groupEdgeSame)
	s.Add(codex.Usage{Input: 100000, Cached: 50000}, groupGapShort, "")
	s.Add(codex.Usage{Input: 200000, Cached: 20000}, groupGapLong, groupEdgeOther)
	s.Add(codex.Usage{Input: 100}, groupGapLong, groupEdgeOther) // too small: nowhere
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
	s.Add(codex.Usage{Input: 100000, Cached: 100000})
	if total, by = s.Take(); total == "" || by != "" {
		t.Errorf("no groups: %q %q", total, by)
	}
}

func TestCacheStatsTurnStateGroups(t *testing.T) {
	for sent, want := range map[bool]string{
		true:  groupTurnStateSent,
		false: groupTurnStateNotSent,
	} {
		if got := turnStateGroup(sent); got != want {
			t.Errorf("turnStateGroup(%t) = %q, want %q", sent, got, want)
		}
	}

	s := &cacheStats{}
	s.Add(codex.Usage{Input: 100000, Cached: 100000}, groupGapShort, groupEdgeSame, turnStateGroup(true))
	s.Add(codex.Usage{Input: 200000, Cached: 50000}, groupGapLong, groupEdgeOther, turnStateGroup(false))
	s.Add(codex.Usage{Input: 200000, Cached: 100000}, groupGapMiddle)
	_, by := s.Take()
	want := "started <5m before: 1 prompts, 100% cached; started 5-10m before: 1 prompts, 50% cached; started >10m before: 1 prompts, 25% cached; same edge: 1 prompts, 100% cached; other edge: 1 prompts, 25% cached; turn state sent: 1 prompts, 100% cached; turn state not sent: 1 prompts, 25% cached"
	if by != want {
		t.Errorf("groups %q, want %q", by, want)
	}
}

func TestWriteCacheStatsPrintsTheGroups(t *testing.T) {
	old := promptStats
	promptStats = &cacheStats{}
	t.Cleanup(func() { promptStats = old })
	promptStats.Add(codex.Usage{Input: 640000, Cached: 600000}, groupGapShort)
	var b bytes.Buffer
	writeCacheStats(&b, time.Date(2026, 11, 8, 9, 10, 11, 0, time.Local))
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "[09:10:11] upstream cache [codex]: 1 big prompts") ||
		lines[1] != "[09:10:11] upstream cache by [codex]: started <5m before: 1 prompts, 93% cached" {
		t.Errorf("lines %q", lines)
	}
}
