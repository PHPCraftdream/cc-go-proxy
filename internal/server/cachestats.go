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

// cacheStats counts how much of the big prompts the backend read from its cache
// since the last Take; it covers every request, not only the slow ones that
// get a line of their own.
type cacheStats struct {
	mu                sync.Mutex
	requests, partial int
	miss              int
	input, cached     int64
}

var promptStats = &cacheStats{}

// Add counts the usage of one finished reply; prompts under cacheMissMin are
// never cached anyway and not counted.
func (s *cacheStats) Add(us codex.Usage) {
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
}

// Take is the counts as a log line and starts them again; "" when nothing was counted.
func (s *cacheStats) Take() string {
	s.mu.Lock()
	r, p, m, in, c := s.requests, s.partial, s.miss, s.input, s.cached
	s.requests, s.partial, s.miss, s.input, s.cached = 0, 0, 0, 0, 0
	s.mu.Unlock()
	if r == 0 {
		return ""
	}
	return fmt.Sprintf("%d big prompts, %d read from the cache in part (under %d%%), %d not at all; %d%% of %s input tokens came from the cache",
		r, p, cachePartialPct, m, c*100/in, millions(in))
}

// millions writes n tokens as 23.4M, or 812k under a million.
func millions(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
	return fmt.Sprintf("%dk", n/1000)
}

// writeCacheStats prints the cache line of the Codex prompts since the last call.
func writeCacheStats(w io.Writer, now time.Time) {
	if s := promptStats.Take(); s != "" {
		_, _ = fmt.Fprintf(w, "[%s] upstream cache [codex]: %s\n", now.Format("15:04:05"), s)
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
