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
	if got := s.Take(); got != "" {
		t.Errorf("nothing counted, line %q", got)
	}
	s.Add(codex.Usage{Input: 100000, Cached: 100000})
	s.Add(codex.Usage{Input: 100000, Cached: 50000})
	s.Add(codex.Usage{Input: 100000, Cached: 0})
	s.Add(codex.Usage{Input: cacheMissMin - 1})      // too small to be cached
	s.Add(codex.Usage{})                             // no usage
	s.Add(codex.Usage{Input: 100000, Cached: 90000}) // exactly the share: whole
	want := "4 big prompts, 1 read from the cache in part (under 90%), 1 not at all; 60% of 400k input tokens came from the cache"
	if got := s.Take(); got != want {
		t.Errorf("line %q, want %q", got, want)
	}
	// Taking starts the counts again.
	if got := s.Take(); got != "" {
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
	if got := s.Take(); !strings.HasPrefix(got, "3200 big prompts, 3200 read from the cache in part") {
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
	w.See(f, nil)
	if got := w.EdgeOf("k"); got != "" {
		t.Errorf("edge before the reply: %q", got)
	}
	w.Done(f, 0, "LAX") // an edge alone is kept, without tokens
	if got := w.EdgeOf("k"); got != "LAX" {
		t.Errorf("edge = %q", got)
	}
	w.Done(f, 5000, "") // tokens do not wipe the edge
	if got := w.EdgeOf("k"); got != "LAX" {
		t.Errorf("edge after tokens = %q", got)
	}
	// The next request starts without an edge of its own; a stranger's key has none.
	w.See(fp("k", "s", "t", 4), nil)
	if got := w.EdgeOf("k"); got != "" {
		t.Errorf("a new request inherited the edge: %q", got)
	}
	if got := w.EdgeOf("other"); got != "" {
		t.Errorf("unknown key: %q", got)
	}
	// An older request's edge does not land on a newer one.
	w.Done(f, 0, "PIT")
	if got := w.EdgeOf("k"); got != "" {
		t.Errorf("an older reply set the edge: %q", got)
	}
}

func TestSummaryShowsTheEdge(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, c := range []struct{ prev, cur, want string }{
		{"LAX", "PIT", "; edge LAX -> PIT"},
		{"LAX", "LAX", "; edge same (LAX)"},
		{"", "LAX", ""},
	} {
		u := &upstreamTrace{start: start, prompt: "key abcd1234, no system prompt, no tools, 9 input items", prevEdge: c.prev, edge: c.cur}
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
	t1.prevEdge = promptWatch.EdgeOf(f1.Key)
	t1.prompt = promptText(f1, []uint64{1, 2, 3, 4})
	logUpstreamEnd(context.Background(), &codex.Translator{Usage: codex.Usage{Input: 640000, Cached: 600000}}, t1,
		&http.Response{Header: http.Header{"Cf-Ray": {"r1-LAX"}}}, "codex", "a -> b", 10)
	if buf.Len() != 0 {
		t.Fatalf("a fast reply was logged: %q", buf.String())
	}

	f2 := codex.Fingerprint{Key: "abcd1234", System: "s", Tools: "t", NTools: 1, Items: 6}
	t2 := newUpstreamTrace()
	t2.fp = f2
	t2.prevEdge = promptWatch.EdgeOf(f2.Key)
	t2.prompt = promptText(f2, []uint64{1, 2, 3, 4, 5, 6})
	tr := &codex.Translator{Failed: "boom", Usage: codex.Usage{Input: 650000, Cached: 400000}}
	logUpstreamEnd(context.Background(), tr, t2, &http.Response{Header: http.Header{"Cf-Ray": {"r2-PIT"}}}, "codex", "a -> b", 10)
	got := buf.String()
	if !strings.Contains(got, "; edge LAX -> PIT") || !strings.Contains(got, "cache partial 61% (400000 of 650000)") {
		t.Errorf("line %q", got)
	}
	// Both replies went into the counters.
	if line := promptStats.Take(); !strings.HasPrefix(line, "2 big prompts, 1 read from the cache in part") {
		t.Errorf("counters: %q", line)
	}
}
