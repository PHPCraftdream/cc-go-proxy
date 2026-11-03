package server

import (
	"fmt"
	"io"
	"sync"
	"time"
)

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

func newDayWriter(w io.Writer) *dayWriter {
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
