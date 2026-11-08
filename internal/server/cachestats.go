package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
)

// Groups a big prompt is counted in besides the total: how long before it the
// last request of its key started (a cache entry is kept for a few minutes) and
// whether that request's reply came through the same data centre.
const (
	groupGapShort  = "started <5m before"
	groupGapMiddle = "started 5-10m before"
	groupGapLong   = "started >10m before"
	groupEdgeSame  = "same edge"
	groupEdgeOther = "other edge"
)

// cacheGroups is the order the groups are printed in.
var cacheGroups = []string{groupGapShort, groupGapMiddle, groupGapLong, groupEdgeSame, groupEdgeOther}

type cacheCount struct {
	n             int
	input, cached int64
}

// cacheStats counts how much of the big prompts the backend read from its cache
// since the last Take; it covers every request, not only the slow ones that
// get a line of their own.
type cacheStats struct {
	mu                sync.Mutex
	requests, partial int
	miss              int
	input, cached     int64
	groups            map[string]*cacheCount
}

var promptStats = &cacheStats{}

// Add counts the usage of one finished reply, in the total and in each of the
// groups ("" is skipped: nothing was known); prompts under cacheMissMin are
// never cached anyway and not counted.
func (s *cacheStats) Add(us codex.Usage, groups ...string) {
	if us.Input < cacheMissMin {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	s.input += int64(us.Input)
	s.cached += int64(us.Cached)
	switch {
	case us.Cached == 0:
		s.miss++
	case us.Cached*100 < us.Input*cachePartialPct:
		s.partial++
	}
	for _, g := range groups {
		if g == "" {
			continue
		}
		if s.groups == nil {
			s.groups = map[string]*cacheCount{}
		}
		c := s.groups[g]
		if c == nil {
			c = &cacheCount{}
			s.groups[g] = c
		}
		c.n++
		c.input += int64(us.Input)
		c.cached += int64(us.Cached)
	}
}

// Take is the counts as two log texts, the total and the groups, and starts them
// again; both "" when nothing was counted.
func (s *cacheStats) Take() (total, by string) {
	s.mu.Lock()
	r, p, m, in, c, groups := s.requests, s.partial, s.miss, s.input, s.cached, s.groups
	s.requests, s.partial, s.miss, s.input, s.cached, s.groups = 0, 0, 0, 0, 0, nil
	s.mu.Unlock()
	if r == 0 {
		return "", ""
	}
	total = fmt.Sprintf("%d big prompts, %d read from the cache in part (under %d%%), %d not at all; %d%% of %s input tokens came from the cache",
		r, p, cachePartialPct, m, c*100/in, millions(in))
	var parts []string
	for _, g := range cacheGroups {
		if gc := groups[g]; gc != nil {
			parts = append(parts, fmt.Sprintf("%s: %d prompts, %d%% cached", g, gc.n, gc.cached*100/gc.input))
		}
	}
	return total, strings.Join(parts, "; ")
}

// gapGroup puts a request by how long after the start of the key's last one it
// started; "" when there was none.
func gapGroup(prev prevInfo, start time.Time) string {
	if prev.start.IsZero() {
		return ""
	}
	switch d := start.Sub(prev.start); {
	case d < 5*time.Minute:
		return groupGapShort
	case d < 10*time.Minute:
		return groupGapMiddle
	}
	return groupGapLong
}

// edgeGroup tells whether the reply came through the data centre of the key's
// last one; "" when either is unknown.
func edgeGroup(prev, cur string) string {
	switch {
	case prev == "" || cur == "":
		return ""
	case prev == cur:
		return groupEdgeSame
	}
	return groupEdgeOther
}

// millions writes n tokens as 23.4M, or 812k under a million.
func millions(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	return fmt.Sprintf("%dk", n/1000)
}

// writeCacheStats prints the cache lines of the Codex prompts since the last call.
func writeCacheStats(w io.Writer, now time.Time) {
	total, by := promptStats.Take()
	if total == "" {
		return
	}
	stamp := now.Format("15:04:05")
	_, _ = fmt.Fprintf(w, "[%s] upstream cache [codex]: %s\n", stamp, total)
	if by != "" {
		_, _ = fmt.Fprintf(w, "[%s] upstream cache by [codex]: %s\n", stamp, by)
	}
}

// edgeOf is the data centre in the tail of a Cf-Ray header (a56de7373f3d69dc-DFW
// gives DFW); "" when there is none.
func edgeOf(h http.Header) string {
	ray := h.Get("Cf-Ray")
	i := strings.LastIndexByte(ray, '-')
	if i < 0 || i == len(ray)-1 {
		return ""
	}
	return ray[i+1:]
}

// edgeText says whether a reply came through the same data centre as the last
// request of its key (a cache may live per region): "" when either is unknown.
func edgeText(prev, cur string) string {
	switch {
	case prev == "" || cur == "":
		return ""
	case prev == cur:
		return "edge same (" + cur + ")"
	}
	return "edge " + prev + " -> " + cur
}
