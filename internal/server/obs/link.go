package obs

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
	UploadMin     = 256 << 10
	uploadTimeMin = 100 * time.Millisecond
	// linkSamplesMax bounds the TLS times kept between two log lines.
	linkSamplesMax = 4096
)

// Causes of a broken upstream call, as the link line names them.
const (
	causeConnect = "connect timeout"
	CauseLost    = "connection lost"
	causePing    = "silence (health-ping close)"
	causeNoReply = "no reply in time"
	causeReset   = "reset"
	CauseWSEnded = "websocket ended by the backend"
	causeOther   = "other"
)

var linkCauses = []string{causeConnect, CauseLost, causePing, causeNoReply, causeReset, CauseWSEnded, causeOther}

// linkStats says how the link to the Codex backend behaved since the last Take:
// how long the TLS handshakes took, how fast big bodies went out, what broke
// the calls and whether ccgp sent them again or the client saw the failure.
type LinkStats struct {
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

var LinkHealth = &LinkStats{}

// Reply counts one finished upstream reply: the TLS time of a connection opened
// for it (0 = it was reused or warm) and how its body went out.
func (s *LinkStats) Reply(tls time.Duration, sent int, wrote time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies++
	if tls > 0 && len(s.tls) < linkSamplesMax {
		s.tls = append(s.tls, tls)
	}
	if sent >= UploadMin && wrote >= uploadTimeMin {
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
func (s *LinkStats) Break(cause string, retried bool) {
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
func (s *LinkStats) Take() string {
	s.mu.Lock()
	replies, tls, uploads, upBytes, upTime, slowest := s.replies, s.tls, s.uploads, s.upBytes, s.upTime, s.slowest
	retried, failed, kinds := s.retried, s.failed, s.breakKind
	s.replies, s.tls, s.uploads, s.upBytes, s.upTime, s.slowest = 0, nil, 0, 0, 0, 0
	s.retried, s.failed, s.breakKind = 0, 0, nil
	s.mu.Unlock()
	if replies == 0 && retried+failed == 0 {
		return ""
	}
	parts := []string{Count(replies, "reply", "replies")}
	if len(tls) > 0 {
		sort.Slice(tls, func(i, j int) bool { return tls[i] < tls[j] })
		parts = append(parts, fmt.Sprintf("%s, TLS median %s, max %s",
			Count(len(tls), "new connection", "new connections"), RoundDur(tls[len(tls)/2]), RoundDur(tls[len(tls)-1])))
	}
	if uploads > 0 {
		parts = append(parts, fmt.Sprintf("upload of %s %s/s overall, slowest %s/s",
			Count(uploads, "big body", "big bodies"), SizeString(int(float64(upBytes)/upTime.Seconds())), SizeString(int(slowest))))
	}
	if n := retried + failed; n > 0 {
		var by []string
		for _, c := range linkCauses {
			if k := kinds[c]; k > 0 {
				by = append(by, fmt.Sprintf("%d %s", k, c))
			}
		}
		parts = append(parts, fmt.Sprintf("%s (%d sent again by ccgp, %d reached the client): %s",
			Count(n, "broken call", "broken calls"), retried, failed, strings.Join(by, ", ")))
	} else {
		parts = append(parts, "no broken calls")
	}
	return strings.Join(parts, "; ")
}

// linkCause names why an upstream call broke from the error text; ping says the
// silence before it was the time of a health-ping close. ok is false for a
// failure that says nothing about the link (the client left, a rejected login).
func LinkCause(msg string, ping bool) (cause string, ok bool) {
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
		return CauseLost, true
	case strings.Contains(m, "codex websocket: connection failed"):
		return CauseWSEnded, true
	case strings.Contains(m, "forcibly closed"), strings.Contains(m, "connection reset"),
		strings.Contains(m, "broken pipe"), strings.Contains(m, "unexpected eof"), strings.HasSuffix(m, "eof"):
		return causeReset, true
	}
	return causeOther, true
}

// noteBreak counts a broken call in the link stats.
func NoteBreak(msg string, ping, retried bool) {
	if cause, ok := LinkCause(msg, ping); ok {
		LinkHealth.Break(cause, retried)
	}
}

// WriteLinkStats prints the link line of the Codex calls since the last call.
func WriteLinkStats(w io.Writer, now time.Time) {
	if text := LinkHealth.Take(); text != "" {
		_, _ = fmt.Fprintf(w, "[%s] upstream link [codex]: %s\n", now.Format("15:04:05"), text)
	}
}

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

var ReplySpeed = &speedStats{}

// speedKind names the transport of a reply from its request note.
func SpeedKind(transport string) string {
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
			RoundDur(median(first[kind])), RoundDur(median(total[kind])), Count(n, "reply", "replies")))
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

func WriteSpeedStats(w io.Writer, now time.Time) {
	if text := ReplySpeed.Take(); text != "" {
		_, _ = fmt.Fprintf(w, "[%s] upstream speed [codex]: %s\n", now.Format("15:04:05"), text)
	}
}

// dayWriter writes to w and puts a "--- 2026-11-02 ---" line before the first
// line of every day (and of the process): log lines carry the time only, so the
// file does not say which day a line is from. The stamp of a line is the start
// of its request, so a line stamped later than the clock when it is written
// began before midnight and stays above the new day's mark. A line written
// after the first of the new day that began before it still falls under the mark.
type dayWriter struct {
	mu  sync.Mutex
	w   io.Writer
	day string
	now func() time.Time
}

func NewDayWriter(w io.Writer) *dayWriter {
	d := &dayWriter{w: w, now: time.Now}
	d.mu.Lock()
	d.markLocked(d.now())
	d.mu.Unlock()
	return d
}

func (d *dayWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now := d.now(); !beganEarlier(p, now) {
		d.markLocked(now)
	}
	return d.w.Write(p)
}

func (d *dayWriter) markLocked(now time.Time) {
	if day := now.Format("2006-01-02"); day != d.day {
		d.day = day
		_, _ = fmt.Fprintf(d.w, "--- %s ---\n", day)
	}
}

// beganEarlier reports whether line starts with a "[HH:MM:SS]" stamp that is later
// in the day than now: a stamp is never in the future, so its request began on
// the day before.
func beganEarlier(line []byte, now time.Time) bool {
	if len(line) < 10 || line[0] != '[' || line[9] != ']' {
		return false
	}
	t, err := time.Parse("15:04:05", string(line[1:9]))
	if err != nil {
		return false
	}
	secs := func(h, m, s int) int { return h*3600 + m*60 + s }
	return secs(t.Hour(), t.Minute(), t.Second()) > secs(now.Hour(), now.Minute(), now.Second())
}
