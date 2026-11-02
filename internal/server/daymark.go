package server

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// dayWriter writes to w and puts a "--- 2026-11-02 ---" line before the first
// line of every day (and of the process): log lines carry the time only, so the
// file does not say which day a line is from. A line is dated by when it is
// written; a request stamped just before midnight can follow the new day's mark.
type dayWriter struct {
	mu  sync.Mutex
	w   io.Writer
	day string
	now func() time.Time
}

func newDayWriter(w io.Writer) *dayWriter {
	d := &dayWriter{w: w, now: time.Now}
	d.mu.Lock()
	d.markLocked()
	d.mu.Unlock()
	return d
}

func (d *dayWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.markLocked()
	return d.w.Write(p)
}

func (d *dayWriter) markLocked() {
	if day := d.now().Format("2006-01-02"); day != d.day {
		d.day = day
		_, _ = fmt.Fprintf(d.w, "--- %s ---\n", day)
	}
}
