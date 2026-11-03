package angel

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/ktavfile"
)

func pendingPath(dir string) string {
	return filepath.Join(dir, crashDir, "pending-"+strconv.Itoa(os.Getpid())+".txt")
}

func TestBeginRunWritesTheRecordAndEndRemovesIt(t *testing.T) {
	dir := t.TempDir()
	lc, note := beginRun(dir, "1.2.3")
	t.Cleanup(lc.End)
	if note != "" {
		t.Errorf("a first run reports %q", note)
	}
	var r Run
	if err := ktavfile.Read(runPath(dir), &r, nil); err != nil {
		t.Fatal(err)
	}
	if r.PID != strconv.Itoa(os.Getpid()) || r.Version != "1.2.3" || r.Started == "" || r.Parent == "" || r.Job == "" {
		t.Errorf("record = %+v", r)
	}
	if _, err := os.Stat(pendingPath(dir)); err != nil {
		t.Errorf("no crash file: %v", err)
	}

	lc.End()
	for _, p := range []string{runPath(dir), pendingPath(dir)} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there after a clean exit: %v", filepath.Base(p), err)
		}
	}
	lc.End() // twice is fine
	var none *Lifecycle
	none.End() // and so is a server without the files
}

func TestNextServerSaysThatThePreviousOneVanished(t *testing.T) {
	dir := t.TempDir()
	old := Run{PID: "424242", Started: "2026-11-02 21:19:36", Version: "0.1.0", Parent: "7", Job: "its processes are killed when the job closes"}
	if err := ktavfile.Write(runPath(dir), old); err != nil {
		t.Fatal(err)
	}
	last := time.Date(2026, 11, 3, 8, 10, 45, 0, time.Local)
	if err := os.Chtimes(runPath(dir), last, last); err != nil {
		t.Fatal(err)
	}

	lc, note := beginRun(dir, "x")
	t.Cleanup(lc.End)
	for _, want := range []string{
		"previous server pid 424242", "started 2026-11-02 21:19:36", "last alive 2026-11-03 08:10:45",
		"no crash report", "it ran in a job: its processes are killed when the job closes",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q lacks %q", note, want)
		}
	}
	var r Run
	if err := ktavfile.Read(runPath(dir), &r, nil); err != nil || r.PID != strconv.Itoa(os.Getpid()) {
		t.Errorf("the new server did not take the record: %+v, %v", r, err)
	}
}

// A stop by ccgp says so in the log and leaves nothing for the next server to wonder about.
func TestAStopByCcgpIsNotAVanishing(t *testing.T) {
	dir := t.TempDir()
	if err := ktavfile.Write(runPath(dir), Run{PID: "424242", Started: "2026-11-02 21:19:36", Version: "v", Parent: "7", Job: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ccgp.log"), []byte("earlier line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noteStop(dir, 424242, "ccgp --stop")

	b, _ := os.ReadFile(filepath.Join(dir, "ccgp.log"))
	if got := string(b); !strings.HasPrefix(got, "earlier line\n[") || !strings.Contains(got, "] server pid 424242 stopped by ccgp --stop (pid ") {
		t.Errorf("log = %q", got)
	}
	lc, note := beginRun(dir, "x")
	t.Cleanup(lc.End)
	if note != "" {
		t.Errorf("a stopped server is reported: %q", note)
	}

	// Without a log (no server ever ran here) nothing is created.
	empty := t.TempDir()
	noteStop(empty, 1, "ccgp --stop")
	if _, err := os.Stat(filepath.Join(empty, "ccgp.log")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a log was created: %v", err)
	}
}

func TestCrashFilesBecomeReports(t *testing.T) {
	dir := t.TempDir()
	cd := filepath.Join(dir, crashDir)
	if err := os.MkdirAll(cd, 0o700); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 11, 3, 8, 10, 46, 0, time.Local)
	full, empty := filepath.Join(cd, "pending-111.txt"), filepath.Join(cd, "pending-222.txt")
	if err := os.WriteFile(full, []byte("panic: boom\n\ngoroutine 1 [running]:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got := reapCrashes(dir)
	if len(got) != 1 || got[0] != "crash-20261103-081046-pid111.txt" {
		t.Fatalf("reports = %v", got)
	}
	if b, err := os.ReadFile(filepath.Join(cd, got[0])); err != nil || !strings.Contains(string(b), "panic: boom") {
		t.Errorf("report: %q, %v", b, err)
	}
	for _, p := range []string{full, empty} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was not cleared: %v", filepath.Base(p), err)
		}
	}
}

func TestOnlyTheNewestCrashReportsAreKept(t *testing.T) {
	dir := t.TempDir()
	cd := filepath.Join(dir, crashDir)
	if err := os.MkdirAll(cd, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range keepCrashes + 3 {
		name := fmt.Sprintf("crash-202611%02d-000000-pid1.txt", i+1)
		if err := os.WriteFile(filepath.Join(cd, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reapCrashes(dir)
	entries, _ := os.ReadDir(cd)
	if len(entries) != keepCrashes {
		t.Fatalf("%d reports kept, want %d", len(entries), keepCrashes)
	}
	if _, err := os.Stat(filepath.Join(cd, "crash-20261101-000000-pid1.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the oldest report is still there: %v", err)
	}
}

func TestVanishedNote(t *testing.T) {
	last := time.Date(2026, 11, 3, 8, 10, 45, 0, time.Local)
	prev := Run{PID: "9", Started: "2026-11-02 21:19:36", Job: "none"}
	if got := vanishedNote(Run{}, time.Time{}, false, nil); got != "" {
		t.Errorf("nothing to report: %q", got)
	}
	if got := vanishedNote(prev, last, true, []string{"crash-1.txt"}); !strings.Contains(got, "crash report: crash-1.txt") ||
		strings.Contains(got, "no crash report") || strings.Contains(got, "job") {
		t.Errorf("with a report: %q", got)
	}
	if got := vanishedNote(Run{}, time.Time{}, false, []string{"crash-2.txt"}); !strings.HasPrefix(got, "an earlier server crashed") {
		t.Errorf("a report alone: %q", got)
	}
}

func TestHeartbeatTouchesTheRecord(t *testing.T) {
	old := heartbeat
	heartbeat = 10 * time.Millisecond
	t.Cleanup(func() { heartbeat = old })

	dir := t.TempDir()
	lc, _ := beginRun(dir, "x")
	t.Cleanup(lc.End)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(runPath(dir), past, past); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if st, err := os.Stat(runPath(dir)); err == nil && time.Since(st.ModTime()) < time.Minute {
			return
		}
	}
	t.Error("the record was not touched")
}

// A server that panics leaves its stacks in a file of its own, and its record
// stays: the next server reads both.
func TestCrashOutputIsKept(t *testing.T) {
	if dir := os.Getenv("ANGEL_CRASH_CHILD"); dir != "" {
		beginRun(dir, "child")
		panic("boom from the child")
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashOutputIsKept$")
	cmd.Env = append(os.Environ(), "ANGEL_CRASH_CHILD="+dir)
	if err := cmd.Run(); err == nil {
		t.Fatal("the child did not crash")
	}

	if _, _, ok := readRun(dir); !ok {
		t.Error("the crashed server's record is gone")
	}
	reports := reapCrashes(dir)
	if len(reports) != 1 {
		t.Fatalf("reports = %v", reports)
	}
	b, err := os.ReadFile(filepath.Join(dir, crashDir, reports[0]))
	if err != nil || !strings.Contains(string(b), "boom from the child") || !strings.Contains(string(b), "goroutine") {
		t.Errorf("crash report = %q, %v", b, err)
	}
}
