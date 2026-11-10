package server

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Upload speed is measured on bodies of at least uploadMin that took at least
// uploadTimeMin to write: smaller ones say nothing about the link.
const (
	uploadMin     = 256 << 10
	uploadTimeMin = 100 * time.Millisecond
	// linkSamplesMax bounds the TLS times kept between two log lines.
	linkSamplesMax = 4096
)

// Causes of a broken upstream call, as the link line names them.
const (
	causeConnect = "connect timeout"
	causeLost    = "connection lost"
	causePing    = "silence (health-ping close)"
	causeNoReply = "no reply in time"
	causeReset   = "reset"
	causeWSEnded = "websocket ended by the backend"
	causeOther   = "other"
)

var linkCauses = []string{causeConnect, causeLost, causePing, causeNoReply, causeReset, causeWSEnded, causeOther}

// linkStats says how the link to the Codex backend behaved since the last Take:
// how long the TLS handshakes took, how fast big bodies went out, what broke
// the calls and whether ccgp sent them again or the client saw the failure.
type linkStats struct {
	mu        sync.Mutex
	replies   int
	tls       []time.Duration
	uploads   int
	upBytes   int64
	upTime    time.Duration
	slowest   float64 // bytes per second of the slowest upload, 0 = none
	retried   int
	failed    int
	breakKind map[string]int
}

var linkHealth = &linkStats{}

// Reply counts one finished upstream reply: the TLS time of a connection opened
// for it (0 = it was reused or warm) and how its body went out.
func (s *linkStats) Reply(tls time.Duration, sent int, wrote time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies++
	if tls > 0 && len(s.tls) < linkSamplesMax {
		s.tls = append(s.tls, tls)
	}
	if sent >= uploadMin && wrote >= uploadTimeMin {
		s.uploads++
		s.upBytes += int64(sent)
		s.upTime += wrote
		if bps := float64(sent) / wrote.Seconds(); s.slowest == 0 || bps < s.slowest {
			s.slowest = bps
		}
	}
}

// Break counts a broken call: retried says ccgp sent it again, else the client
// saw the failure.
func (s *linkStats) Break(cause string, retried bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.breakKind == nil {
		s.breakKind = map[string]int{}
	}
	s.breakKind[cause]++
	if retried {
		s.retried++
	} else {
		s.failed++
	}
}

// Take is the counts as a log text, and starts them again; "" when nothing was
// counted.
func (s *linkStats) Take() string {
	s.mu.Lock()
	replies, tls, uploads, upBytes, upTime, slowest := s.replies, s.tls, s.uploads, s.upBytes, s.upTime, s.slowest
	retried, failed, kinds := s.retried, s.failed, s.breakKind
	s.replies, s.tls, s.uploads, s.upBytes, s.upTime, s.slowest = 0, nil, 0, 0, 0, 0
	s.retried, s.failed, s.breakKind = 0, 0, nil
	s.mu.Unlock()
	if replies == 0 && retried+failed == 0 {
		return ""
	}
	parts := []string{count(replies, "reply", "replies")}
	if len(tls) > 0 {
		sort.Slice(tls, func(i, j int) bool { return tls[i] < tls[j] })
		parts = append(parts, fmt.Sprintf("%s, TLS median %s, max %s",
			count(len(tls), "new connection", "new connections"), roundDur(tls[len(tls)/2]), roundDur(tls[len(tls)-1])))
	}
	if uploads > 0 {
		parts = append(parts, fmt.Sprintf("upload of %s %s/s overall, slowest %s/s",
			count(uploads, "big body", "big bodies"), sizeString(int(float64(upBytes)/upTime.Seconds())), sizeString(int(slowest))))
	}
	if n := retried + failed; n > 0 {
		var by []string
		for _, c := range linkCauses {
			if k := kinds[c]; k > 0 {
				by = append(by, fmt.Sprintf("%d %s", k, c))
			}
		}
		parts = append(parts, fmt.Sprintf("%s (%d sent again by ccgp, %d reached the client): %s",
			count(n, "broken call", "broken calls"), retried, failed, strings.Join(by, ", ")))
	} else {
		parts = append(parts, "no broken calls")
	}
	return strings.Join(parts, "; ")
}

// linkCause names why an upstream call broke from the error text; ping says the
// silence before it was the time of a health-ping close. ok is false for a
// failure that says nothing about the link (the client left, a rejected login).
func linkCause(msg string, ping bool) (cause string, ok bool) {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "context canceled"), strings.Contains(m, "not logged in"), strings.Contains(m, "log in again"):
		return "", false
	case ping:
		return causePing, true
	case strings.Contains(m, "tls handshake timeout"), strings.Contains(m, "no connection after"),
		strings.Contains(m, "i/o timeout"), strings.Contains(m, "connect:"), strings.Contains(m, "dial"):
		return causeConnect, true
	case strings.Contains(m, "timeout awaiting response headers"):
		return causeNoReply, true
	case strings.Contains(m, "client connection lost"):
		return causeLost, true
	case strings.Contains(m, "codex websocket: connection failed"):
		return causeWSEnded, true
	case strings.Contains(m, "forcibly closed"), strings.Contains(m, "connection reset"),
		strings.Contains(m, "broken pipe"), strings.Contains(m, "unexpected eof"), strings.HasSuffix(m, "eof"):
		return causeReset, true
	}
	return causeOther, true
}

// noteBreak counts a broken call in the link stats.
func noteBreak(msg string, ping, retried bool) {
	if cause, ok := linkCause(msg, ping); ok {
		linkHealth.Break(cause, retried)
	}
}

// writeLinkStats prints the link line of the Codex calls since the last call.
func writeLinkStats(w io.Writer, now time.Time) {
	if text := linkHealth.Take(); text != "" {
		_, _ = fmt.Fprintf(w, "[%s] upstream link [codex]: %s\n", now.Format("15:04:05"), text)
	}
}

// count is "1 reply" or "3 replies".
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
