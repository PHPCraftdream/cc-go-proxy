package server

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Transports compared by the speed line, in its order.
const (
	speedHTTP    = "http"
	speedWSFull  = "websocket full"
	speedWSDelta = "websocket delta"
)

var speedKinds = []string{speedHTTP, speedWSFull, speedWSDelta}

// speedSamplesMax bounds the samples of one transport between two lines.
const speedSamplesMax = 4096

// speedStats keeps, per transport, how long the completed Codex replies took to
// the first token and to their end, so HTTP and the websocket (full request or
// delta) can be compared on real traffic. Medians over different prompts: a
// trend to read over hours, not a benchmark.
type speedStats struct {
	mu    sync.Mutex
	first map[string][]time.Duration
	total map[string][]time.Duration
}

var replySpeed = &speedStats{}

// speedKind names the transport of a reply from its request note.
func speedKind(transport string) string {
	switch {
	case strings.HasPrefix(transport, "transport websocket (delta"):
		return speedWSDelta
	case strings.HasPrefix(transport, "transport websocket (full"):
		return speedWSFull
	}
	return speedHTTP
}

// Add counts one completed reply; first is zero when it had no content.
func (s *speedStats) Add(kind string, first, total time.Duration) {
	if first <= 0 || total <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first == nil {
		s.first = make(map[string][]time.Duration)
		s.total = make(map[string][]time.Duration)
	}
	if len(s.first[kind]) >= speedSamplesMax {
		return
	}
	s.first[kind] = append(s.first[kind], first)
	s.total[kind] = append(s.total[kind], total)
}

// Take returns the line's text since the last call ("" when nothing came) and
// starts again.
func (s *speedStats) Take() string {
	s.mu.Lock()
	first, total := s.first, s.total
	s.first, s.total = nil, nil
	s.mu.Unlock()
	var parts []string
	for _, kind := range speedKinds {
		n := len(first[kind])
		if n == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s / %s (%s)", kind,
			roundDur(median(first[kind])), roundDur(median(total[kind])), count(n, "reply", "replies")))
	}
	if len(parts) == 0 {
		return ""
	}
	return "first token / whole reply, median: " + strings.Join(parts, ", ")
}

func median(ds []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func writeSpeedStats(w io.Writer, now time.Time) {
	if text := replySpeed.Take(); text != "" {
		_, _ = fmt.Fprintf(w, "[%s] upstream speed [codex]: %s\n", now.Format("15:04:05"), text)
	}
}
