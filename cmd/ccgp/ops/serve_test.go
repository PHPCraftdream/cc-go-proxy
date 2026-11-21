package ops

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

	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server"
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
	if err := LogsCmd([]string{"-n", "-1"}); err == nil || err.Error() != "usage: ccgp --logs [-f] [-n N]" {
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

func TestServerStale(t *testing.T) {
	v := server.ProxyVersion
	tests := []struct {
		name      string
		h         *angel.Health
		ownExe    string
		ownExeMod int64
		wantStale bool
		wantWhy   string
	}{
		{name: "same version and exe", h: &angel.Health{Version: v, Exe: "/ccgp", ExeMod: 10}, ownExe: "/ccgp", ownExeMod: 10},
		{name: "version differs", h: &angel.Health{Version: "0.0.9"}, wantStale: true, wantWhy: "server version 0.0.9, this binary is " + v},
		{name: "exe file replaced", h: &angel.Health{Version: v, Exe: "/ccgp", ExeMod: 10}, ownExe: "/ccgp", ownExeMod: 20, wantStale: true, wantWhy: "the executable file changed since the server started"},
		{name: "different exe path", h: &angel.Health{Version: v, Exe: "/old/ccgp", ExeMod: 10}, ownExe: "/new/ccgp", ownExeMod: 20},
		{name: "server exe unknown", h: &angel.Health{Version: v}, ownExe: "/ccgp", ownExeMod: 20},
		{name: "server mod time unknown", h: &angel.Health{Version: v, Exe: "/ccgp"}, ownExe: "/ccgp", ownExeMod: 20},
		{name: "own mod time unknown", h: &angel.Health{Version: v, Exe: "/ccgp", ExeMod: 10}, ownExe: "/ccgp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stale, why := serverStale(tt.h, v, tt.ownExe, tt.ownExeMod)
			if stale != tt.wantStale || why != tt.wantWhy {
				t.Errorf("serverStale = (%v, %q), want (%v, %q)", stale, why, tt.wantStale, tt.wantWhy)
			}
		})
	}
}

type fakeChecks struct {
	serverChecks
	checkHealth *angel.Health
	checkErr    error
	stopped     int
	started     int
	startHealth *angel.Health
}

func newFakeChecks(h *angel.Health) *fakeChecks {
	fc := &fakeChecks{checkHealth: h}
	fc.check = func(string) (*angel.Health, error) {
		if fc.checkErr != nil {
			return nil, fc.checkErr
		}
		return fc.checkHealth, nil
	}
	fc.stop = func(string, time.Duration) error { fc.stopped++; return nil }
	fc.start = func(string, []string, string, string, time.Duration) (*angel.Health, error) {
		fc.started++
		return fc.startHealth, nil
	}
	fc.stderr = &bytes.Buffer{}
	fc.ownVersion = server.ProxyVersion
	return fc
}

func TestEnsureServerStaleIdleRestarts(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: "old", PID: 1, Active: 0})
	fc.startHealth = &angel.Health{Version: server.ProxyVersion, PID: 2}
	h, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks)
	if err != nil {
		t.Fatal(err)
	}
	if h.PID != 2 {
		t.Errorf("pid = %d, want 2 (restarted)", h.PID)
	}
	if fc.stopped != 1 || fc.started != 1 {
		t.Errorf("stopped=%d started=%d, want 1/1", fc.stopped, fc.started)
	}
	if out := fc.stderr.(*bytes.Buffer).String(); !strings.Contains(out, "ccgp: restarted the server (") {
		t.Errorf("stderr = %q, want success line after restart", out)
	}
}

func TestEnsureServerStaleStartFailsNoSuccessLine(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: "old"})
	fc.start = func(string, []string, string, string, time.Duration) (*angel.Health, error) {
		return nil, errors.New("start boom")
	}
	if _, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks); err == nil {
		t.Fatal("want start error")
	}
	if out := fc.stderr.(*bytes.Buffer).String(); strings.Contains(out, "restarted the server") {
		t.Errorf("stderr = %q, want no success line on start failure", out)
	}
}

func TestEnsureServerStaleBusyWarnsAndKeeps(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: "old", PID: 1, Active: 3})
	h, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks)
	if err != nil {
		t.Fatal(err)
	}
	if h.PID != 1 {
		t.Errorf("pid = %d, want 1 (kept)", h.PID)
	}
	if fc.stopped != 0 || fc.started != 0 {
		t.Errorf("stopped=%d started=%d, want 0/0", fc.stopped, fc.started)
	}
	out := fc.stderr.(*bytes.Buffer).String()
	if !strings.Contains(out, "older than this binary") || !strings.Contains(out, "3 request(s) in flight") {
		t.Errorf("stderr = %q", out)
	}
}

func TestEnsureServerFreshUntouched(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: server.ProxyVersion, PID: 7})
	h, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks)
	if err != nil {
		t.Fatal(err)
	}
	if h.PID != 7 {
		t.Errorf("pid = %d, want 7", h.PID)
	}
	if fc.stopped != 0 || fc.started != 0 {
		t.Errorf("stopped=%d started=%d, want 0/0", fc.stopped, fc.started)
	}
	if out := fc.stderr.(*bytes.Buffer).String(); out != "" {
		t.Errorf("stderr = %q, want empty", out)
	}
}

func TestEnsureServerStaleStopFails(t *testing.T) {
	fc := newFakeChecks(&angel.Health{Version: "old"})
	fc.stop = func(string, time.Duration) error { return errors.New("boom") }
	if _, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks); err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want boom", err)
	}
	if out := fc.stderr.(*bytes.Buffer).String(); strings.Contains(out, "restarted the server") {
		t.Errorf("stderr = %q, want no success line on stop failure", out)
	}
}

type fakeOps struct {
	restartOps
	running *angel.Health
	stopped int
	started int
}

func newFakeOps(running *angel.Health) *fakeOps {
	fo := &fakeOps{running: running}
	fo.stop = func(string, time.Duration) error {
		if fo.running == nil {
			return angel.ErrNotRunning
		}
		fo.stopped++
		return nil
	}
	fo.start = func(string, []string, string, string, time.Duration) (*angel.Health, error) {
		fo.started++
		return &angel.Health{Version: server.ProxyVersion, PID: 100 + fo.started}, nil
	}
	fo.stdout = &bytes.Buffer{}
	fo.exe = func() (string, error) { return "/ccgp", nil }
	fo.logPath = func() (string, error) { return "/log", nil }
	return fo
}

var restartCfg = &config.Config{Host: "127.0.0.1", Port: "1"}

func TestRestartServerNotRunningJustStarts(t *testing.T) {
	fo := newFakeOps(nil)
	h, err := restartServer(restartCfg, &fo.restartOps)
	if err != nil {
		t.Fatal(err)
	}
	if fo.stopped != 0 || fo.started != 1 {
		t.Errorf("stopped=%d started=%d, want 0/1", fo.stopped, fo.started)
	}
	if h.PID != 101 {
		t.Errorf("pid = %d, want 101", h.PID)
	}
	if out := fo.stdout.(*bytes.Buffer).String(); out != "ccgp restarted (pid 101) at http://127.0.0.1:1\n" {
		t.Errorf("stdout = %q", out)
	}
}

func TestRestartServerRunningStopsThenStarts(t *testing.T) {
	fo := newFakeOps(&angel.Health{PID: 5, Active: 4})
	h, err := restartServer(restartCfg, &fo.restartOps)
	if err != nil {
		t.Fatal(err)
	}
	if fo.stopped != 1 || fo.started != 1 {
		t.Errorf("stopped=%d started=%d, want 1/1", fo.stopped, fo.started)
	}
	if h.PID != 101 {
		t.Errorf("pid = %d, want 101 (new server despite 4 in flight)", h.PID)
	}
}

func TestRestartServerStopErrorPropagates(t *testing.T) {
	fo := newFakeOps(&angel.Health{PID: 5})
	fo.stop = func(string, time.Duration) error { return errors.New("stop boom") }
	if _, err := restartServer(restartCfg, &fo.restartOps); err == nil || err.Error() != "stop boom" {
		t.Errorf("err = %v, want stop boom", err)
	}
	if fo.started != 0 {
		t.Errorf("started = %d, want 0", fo.started)
	}
	if out := fo.stdout.(*bytes.Buffer).String(); out != "" {
		t.Errorf("stdout = %q, want empty on failure", out)
	}
}

func TestRestartServerStartErrorPropagates(t *testing.T) {
	fo := newFakeOps(nil)
	fo.start = func(string, []string, string, string, time.Duration) (*angel.Health, error) {
		return nil, errors.New("start boom")
	}
	if _, err := restartServer(restartCfg, &fo.restartOps); err == nil || err.Error() != "start boom" {
		t.Errorf("err = %v, want start boom", err)
	}
	if out := fo.stdout.(*bytes.Buffer).String(); out != "" {
		t.Errorf("stdout = %q, want empty on failure", out)
	}
}

func TestOwnExeInfo(t *testing.T) {
	exe, mod := ownExeInfo()
	if exe == "" {
		t.Fatal("exe path is empty")
	}
	if mod == 0 {
		t.Error("mod time of the test binary should be known")
	}
}

// A plain first start (nothing running) is not a restart: no message.
func TestEnsureServerNotRunningStartsSilently(t *testing.T) {
	fc := newFakeChecks(nil)
	fc.checkErr = errors.New("connection refused")
	fc.startHealth = &angel.Health{Version: server.ProxyVersion, PID: 5}
	h, err := ensureServerWith(&config.Config{Host: "127.0.0.1", Port: "1"}, nil, &fc.serverChecks)
	if err != nil || h.PID != 5 {
		t.Fatalf("h=%v err=%v", h, err)
	}
	if fc.stopped != 0 || fc.started != 1 {
		t.Errorf("stopped=%d started=%d, want 0/1", fc.stopped, fc.started)
	}
	if out := fc.stderr.(*bytes.Buffer).String(); out != "" {
		t.Errorf("stderr = %q, want empty", out)
	}
}
