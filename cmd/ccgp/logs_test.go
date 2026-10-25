package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")

	write(t, path, "l1\nl2\nl3\n")
	got, err := tailLines(path, 2)
	if err != nil || len(got) != 2 || got[0] != "l2" || got[1] != "l3" {
		t.Fatalf("more than n: got %q, %v", got, err)
	}

	got, err = tailLines(path, 5)
	if err != nil || len(got) != 3 || got[0] != "l1" {
		t.Fatalf("fewer than n: got %q, %v", got, err)
	}

	got, err = tailLines(path, 3)
	if err != nil || len(got) != 3 || got[0] != "l1" || got[2] != "l3" {
		t.Fatalf("exactly n: got %q, %v", got, err)
	}

	write(t, path, "a\nb\nc")
	got, err = tailLines(path, 2)
	if err != nil || len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("no trailing newline: got %q, %v", got, err)
	}

	long := strings.Repeat("x", 100)
	write(t, path, "first\n"+long)
	got, err = tailLines(path, 1)
	if err != nil || len(got) != 1 || got[0] != long {
		t.Fatalf("long line: got len %d, %v", len(got), err)
	}

	write(t, path, "")
	got, err = tailLines(path, 3)
	if err != nil || got != nil {
		t.Fatalf("empty file: got %q, %v", got, err)
	}

	write(t, path, "a\nb\n")
	got, err = tailLines(path, 0)
	if err != nil || got != nil {
		t.Fatalf("n=0: got %q, %v", got, err)
	}

	crlf := "l1\r\nl2\r\n"
	write(t, path, crlf)
	got, err = tailLines(path, 2)
	if err != nil || len(got) != 2 || got[0] != "l1\r" || got[1] != "l2\r" {
		t.Fatalf("crlf: got %q, %v", got, err)
	}

	// Tail spanning multiple chunks.
	old := logChunkSize
	logChunkSize = 8
	t.Cleanup(func() { logChunkSize = old })
	write(t, path, "one\ntwo\nthree\nfour\nfive\n")
	got, err = tailLines(path, 3)
	if err != nil || len(got) != 3 || got[0] != "three" || got[2] != "five" {
		t.Fatalf("multi-chunk: got %q, %v", got, err)
	}

	// A last line longer than a chunk.
	write(t, path, "first\n"+long+"\n")
	got, err = tailLines(path, 1)
	if err != nil || len(got) != 1 || got[0] != long {
		t.Fatalf("line longer than a chunk: got %q, %v", got, err)
	}

	if _, err := tailLines(filepath.Join(dir, "missing"), 1); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: got %v", err)
	}
}

func TestParseLogsArgs(t *testing.T) {
	usage := "usage: ccgp --logs [-f] [-n N]"
	tests := []struct {
		args   []string
		n      int
		follow bool
		err    string
	}{
		{nil, 50, false, ""},
		{[]string{"-n", "10"}, 10, false, ""},
		{[]string{"-f"}, 50, true, ""},
		{[]string{"-f", "-n", "3"}, 3, true, ""},
		{[]string{"-n", "0"}, 0, false, ""},
		{[]string{"-n", "-1"}, 0, false, usage},
		{[]string{"-n", "abc"}, 0, false, usage},
		{[]string{"-n"}, 0, false, usage},
		{[]string{"-n20"}, 0, false, usage},
		{[]string{"foo"}, 0, false, usage},
		{[]string{"-x"}, 0, false, usage},
	}
	for _, tt := range tests {
		n, follow, err := parseLogsArgs(tt.args)
		if tt.err == "" {
			if err != nil || n != tt.n || follow != tt.follow {
				t.Errorf("%v: got n=%d follow=%v err=%v", tt.args, n, follow, err)
			}
			continue
		}
		if err == nil || err.Error() != tt.err {
			t.Errorf("%v: got err %v, want %q", tt.args, err, tt.err)
		}
	}
}

func TestLogsCmdUsageError(t *testing.T) {
	if err := logsCmd([]string{"-n", "-1"}); err == nil || err.Error() != "usage: ccgp --logs [-f] [-n N]" {
		t.Fatalf("got %v", err)
	}
}

func appendTo(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// syncBuffer is a bytes.Buffer safe to read while followLog writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startFollow runs followLog and returns the output and a stop function that
// checks the loop ends with context.Canceled.
func startFollow(t *testing.T, path string, offset int64) (*syncBuffer, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	buf := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- followLog(ctx, buf, path, 10*time.Millisecond, offset) }()
	return buf, func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("followLog returned %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("followLog did not return after cancel")
		}
	}
}

func TestFollowLogAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.txt")
	write(t, path, "seed line\n")

	buf, stop := startFollow(t, path, fileSize(path))
	defer stop()
	appendTo(t, path, "new line\n")
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(buf.String(), "new line") })
	if strings.Contains(buf.String(), "seed") {
		t.Fatalf("output before the offset was repeated: %q", buf.String())
	}
	appendTo(t, path, "second\n")
	waitFor(t, 2*time.Second, func() bool { return buf.String() == "new line\nsecond\n" })
}

func TestFollowLogRestartsAfterReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.txt")
	write(t, path, "long original content that will go away\n")

	buf, stop := startFollow(t, path, fileSize(path))
	defer stop()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write(t, path, "short\n")
	waitFor(t, 2*time.Second, func() bool { return buf.String() == "short\n" })
}

func TestFollowLogMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.txt")

	buf, stop := startFollow(t, path, 0)
	defer stop()
	time.Sleep(50 * time.Millisecond)
	write(t, path, "created later\n")
	waitFor(t, 2*time.Second, func() bool { return buf.String() == "created later\n" })
}
