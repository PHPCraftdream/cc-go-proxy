package server

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDayWriterMarksEachDay(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 1, 23, 59, 0, 0, time.UTC)
	d := &dayWriter{w: &buf, now: func() time.Time { return now }}

	write := func(s string) {
		t.Helper()
		if n, err := d.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	write("[23:59:00] a\n")
	write("[23:59:30] b\n")
	now = now.Add(2 * time.Minute) // 00:01 on the 2nd
	write("[00:01:00] c\n")
	write("[00:02:00] d\n")

	want := "--- 2026-11-01 ---\n[23:59:00] a\n[23:59:30] b\n--- 2026-11-02 ---\n[00:01:00] c\n[00:02:00] d\n"
	if got := buf.String(); got != want {
		t.Errorf("log:\n%s\nwant:\n%s", got, want)
	}
}

// A line stamped later than the clock is of a request that began before midnight:
// it stays above the new day's mark, which waits for a line of the new day (or one
// without a stamp).
func TestDayWriterKeepsALateStampedLineUnderTheOldDay(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 11, 1, 23, 59, 0, 0, time.UTC)
	d := &dayWriter{w: &buf, now: func() time.Time { return now }}
	write := func(s string) {
		t.Helper()
		if _, err := d.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	write("[23:59:00] a\n")
	now = time.Date(2026, 11, 2, 0, 0, 5, 0, time.UTC)
	write("[23:59:57] b\n") // began yesterday, written after midnight
	write("[00:00:01] c\n") // began today
	now = time.Date(2026, 11, 3, 0, 0, 2, 0, time.UTC)
	write("✅ banner\n") // no stamp: written now

	want := "--- 2026-11-01 ---\n[23:59:00] a\n[23:59:57] b\n--- 2026-11-02 ---\n[00:00:01] c\n--- 2026-11-03 ---\n✅ banner\n"
	if got := buf.String(); got != want {
		t.Errorf("log:\n%s\nwant:\n%s", got, want)
	}
}

func TestBeganEarlier(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 5, 0, 0, time.UTC)
	for line, want := range map[string]bool{
		"[23:59:57] 200 - x\n": true,
		"[00:05:01] x\n":       true,
		"[00:05:00] x\n":       false,
		"[00:00:01] x\n":       false,
		"[25:00:00] x\n":       false,
		"[ab:cd:ef] x\n":       false,
		"no stamp\n":           false,
		"[1:2:3]\n":            false,
		"":                     false,
	} {
		if got := beganEarlier([]byte(line), now); got != want {
			t.Errorf("beganEarlier(%q) = %v, want %v", line, got, want)
		}
	}
}

// The mark opens a process's log even before its first line.
func TestNewDayWriterMarksAtOnce(t *testing.T) {
	var buf bytes.Buffer
	d := newDayWriter(&buf)
	if got := buf.String(); !strings.HasPrefix(got, "--- ") || !strings.HasSuffix(got, " ---\n") || strings.Count(got, "\n") != 1 {
		t.Fatalf("log = %q, want one date line", got)
	}
	_, _ = d.Write([]byte("x\n"))
	if strings.Count(buf.String(), "---") != 2 {
		t.Errorf("the same day was marked twice: %q", buf.String())
	}
}
