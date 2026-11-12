package sessions

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const sid = "11111111-2222-4333-8444-555555555555"

// entry is a transcript line of session id.
func entry(id, text string) string {
	return `{"type":"user","sessionId":"` + id + `","message":{"content":"` + text + `"}}` + "\n"
}

type pair struct {
	src, dst string // config dirs
	s, d     string // the session file on each side
}

// both has the session on both sides, gone on differently after the first line.
func both(t *testing.T) pair {
	t.Helper()
	p := pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "a", sid+".jsonl")
	p.d = filepath.Join(p.dst, "projects", "a", sid+".jsonl")
	put(t, p.s, entry(sid, "one")+entry(sid, "from-source"), t0)
	put(t, p.d, entry(sid, "one")+entry(sid, "from-destination"), t0.Add(time.Hour))
	return p
}

func copyOne(t *testing.T, p pair, opt Options) Result {
	t.Helper()
	res, err := Copy(p.src, p.dst, 5, opt)
	if err != nil || len(res) != 1 {
		t.Fatalf("Copy = %+v, %v", res, err)
	}
	if res[0].Err != nil {
		t.Fatalf("Copy: %v", res[0].Err)
	}
	return res[0]
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "*.bak-*"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// ---- fork ----

func TestForkKeepsBothVersions(t *testing.T) {
	p := both(t)
	side := filepath.Join(p.src, "projects", "a", sid)
	put(t, filepath.Join(side, "subagents", "agent-1.jsonl"), entry(sid, "sub"), t0)
	put(t, filepath.Join(side, "tool-results", "r.txt"), "see "+sid, t0)
	old := read(t, p.d)

	r := copyOne(t, p, Options{Fork: true})
	fid := forkID(sid)
	if r.Status != Forked || r.Fork != fid || r.Backup != "" || r.Files != 2 {
		t.Fatalf("result = %+v, want forked as %s with 2 sidecar files", r, fid)
	}
	if got := read(t, p.d); got != old {
		t.Errorf("the destination was touched: %q", got)
	}
	fork := filepath.Join(p.dst, "projects", "a", fid+".jsonl")
	if got, want := read(t, fork), entry(fid, "one")+entry(fid, "from-source"); got != want {
		t.Errorf("fork = %q, want %q", got, want)
	}
	// The files of the folder follow the new id, text files too.
	if got := read(t, filepath.Join(p.dst, "projects", "a", fid, "subagents", "agent-1.jsonl")); got != entry(fid, "sub") {
		t.Errorf("subagent transcript = %q", got)
	}
	if got := read(t, filepath.Join(p.dst, "projects", "a", fid, "tool-results", "r.txt")); got != "see "+fid {
		t.Errorf("tool result = %q", got)
	}
	if exists(filepath.Join(p.dst, "projects", "a", sid)) {
		t.Error("the sidecar went to the diverged session's directory")
	}
	list, err := List(p.dst)
	if err != nil || len(list) != 2 {
		t.Errorf("sessions = %v, %v; want the old one and the fork", ids(list), err)
	}
}

func TestForkIsRepeatableAndExtendsItself(t *testing.T) {
	p := both(t)
	fid := forkID(sid)
	fork := filepath.Join(p.dst, "projects", "a", fid+".jsonl")

	if r := copyOne(t, p, Options{Fork: true}); r.Status != Forked {
		t.Fatalf("first = %+v", r)
	}
	// The next run lands on the same file and finds nothing to do.
	r := copyOne(t, p, Options{Fork: true})
	if r.Status != Unchanged || r.Fork != fid {
		t.Errorf("second = %+v, want unchanged as %s", r, fid)
	}
	if list, _ := List(p.dst); len(list) != 2 {
		t.Errorf("a rerun made another session: %v", ids(list))
	}

	// The source goes on: the fork follows it.
	put(t, p.s, entry(sid, "one")+entry(sid, "from-source")+entry(sid, "more"), t0.Add(2*time.Hour))
	r = copyOne(t, p, Options{Fork: true})
	if r.Status != Forked || r.Fork != fid {
		t.Errorf("after growth = %+v", r)
	}
	if got, want := read(t, fork), entry(fid, "one")+entry(fid, "from-source")+entry(fid, "more"); got != want {
		t.Errorf("fork = %q, want %q", got, want)
	}
}

func TestForkThatWentItsOwnWayIsLeftAlone(t *testing.T) {
	p := both(t)
	fid := forkID(sid)
	fork := filepath.Join(p.dst, "projects", "a", fid+".jsonl")
	put(t, fork, entry(fid, "one")+entry(fid, "continued elsewhere"), t0.Add(time.Hour))

	r := copyOne(t, p, Options{Fork: true})
	if r.Status != Diverged || r.Fork != fid {
		t.Fatalf("result = %+v, want diverged with the fork named", r)
	}
	if got := read(t, fork); got != entry(fid, "one")+entry(fid, "continued elsewhere") {
		t.Errorf("the fork was changed: %q", got)
	}
	if list, _ := List(p.dst); len(list) != 2 {
		t.Errorf("sessions = %v; no third session may appear", ids(list))
	}
}

func TestForkOnlyWhenDiverged(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	s := filepath.Join(src, "projects", "a", sid+".jsonl")
	d := filepath.Join(dst, "projects", "a", sid+".jsonl")
	p := pair{src, dst, s, d}
	put(t, s, entry(sid, "one"), t0)

	want := []Status{Copied, Unchanged}
	for i, w := range want {
		if r := copyOne(t, p, Options{Fork: true}); r.Status != w || r.Fork != "" {
			t.Errorf("run %d = %+v, want %v without a fork", i+1, r, w)
		}
	}
	put(t, s, entry(sid, "one")+entry(sid, "two"), t0.Add(time.Hour))
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Updated || r.Fork != "" {
		t.Errorf("growth = %+v, want updated without a fork", r)
	}
	put(t, d, entry(sid, "one")+entry(sid, "two")+entry(sid, "three"), t0.Add(2*time.Hour))
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Ahead || r.Fork != "" {
		t.Errorf("destination ahead = %+v, want ahead without a fork", r)
	}
	if list, _ := List(dst); len(list) != 1 {
		t.Errorf("sessions = %v, want just one", ids(list))
	}
}

func TestForkStripsThinkingToo(t *testing.T) {
	p := both(t)
	tr := strings.ReplaceAll(transcript, `{"type":"user","uuid":"u1"`, `{"type":"user","sessionId":"`+sid+`","uuid":"u1"`)
	put(t, p.s, tr, t0.Add(2*time.Hour))
	// The destination holds the first line only differently, so the two diverge at once.
	put(t, p.d, "x\n", t0)

	r := copyOne(t, p, Options{Fork: true, StripThinking: true})
	if r.Status != Forked || r.Removed != 2 {
		t.Fatalf("result = %+v, want forked with 2 blocks removed", r)
	}
	got := read(t, filepath.Join(p.dst, "projects", "a", forkID(sid)+".jsonl"))
	if strings.Contains(got, "thinking") || strings.Contains(got, sid) || !strings.Contains(got, forkID(sid)) {
		t.Errorf("fork = %q", got)
	}
}

func TestForkDoesNotCrossTheProjectOfAnotherSession(t *testing.T) {
	p := both(t)
	put(t, filepath.Join(p.src, "projects", "b", "other.jsonl"), "x\n", t0.Add(-time.Hour))
	res, err := Copy(p.src, p.dst, 5, Options{Fork: true, Project: "a"})
	if err != nil || len(res) != 1 || res[0].Status != Forked {
		t.Fatalf("Copy = %+v, %v", res, err)
	}
	if exists(filepath.Join(p.dst, "projects", "b")) {
		t.Error("another project was copied")
	}
}

// Fork with Force: the original is left alone, a fork that went its own way
// (an earlier ccgp wrote it otherwise, or it was continued) is replaced and
// kept as a backup.
func TestForkWithForceReplacesTheFork(t *testing.T) {
	p := both(t)
	orig := read(t, p.d)
	fid := forkID(sid)
	fpath := filepath.Join(p.dst, "projects", "a", fid+".jsonl")
	put(t, fpath, entry(fid, "an older fork"), t0)

	r := copyOne(t, p, Options{Fork: true})
	if r.Status != Diverged || r.Fork != fid {
		t.Fatalf("fork alone = %+v, want the fork left as it is", r)
	}
	r = copyOne(t, p, Options{Fork: true, Force: true})
	if r.Status != Forked || r.Fork != fid || r.Backup == "" {
		t.Fatalf("fork with force = %+v, want the fork replaced", r)
	}
	if got := read(t, p.d); got != orig {
		t.Errorf("the original was touched: %q", got)
	}
	if got := read(t, fpath); got != entry(fid, "one")+entry(fid, "from-source") {
		t.Errorf("fork = %q", got)
	}
	if got := read(t, r.Backup); got != entry(fid, "an older fork") {
		t.Errorf("backup = %q", got)
	}
	if b := backups(t, filepath.Join(p.dst, "projects", "a")); len(b) != 1 {
		t.Errorf("backups = %v, want only the fork's", b)
	}
	if r = copyOne(t, p, Options{Fork: true, Force: true}); r.Status != Unchanged {
		t.Errorf("rerun = %+v", r)
	}
}

// Fork with Force on a session that did not diverge: an ordinary copy, the
// destination's other files are not replaced.
func TestForkWithForceOnAnUpdateReplacesNothing(t *testing.T) {
	p := pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "a", sid+".jsonl")
	p.d = filepath.Join(p.dst, "projects", "a", sid+".jsonl")
	put(t, p.s, entry(sid, "one")+entry(sid, "two"), t0)
	put(t, p.d, entry(sid, "one"), t0)
	put(t, filepath.Join(p.src, "projects", "a", sid, "tool-results", "r.txt"), "source", t0)
	side := filepath.Join(p.dst, "projects", "a", sid, "tool-results", "r.txt")
	put(t, side, "destination", t0)

	r := copyOne(t, p, Options{Fork: true, Force: true})
	if r.Status != Updated || r.Fork != "" || r.Backup != "" {
		t.Fatalf("result = %+v, want updated", r)
	}
	if got := read(t, side); got != "destination" {
		t.Errorf("sidecar file replaced: %q", got)
	}
}

// ---- force ----

func TestForceReplacesAndKeepsTheOldFile(t *testing.T) {
	p := both(t)
	old := read(t, p.d)

	r := copyOne(t, p, Options{Force: true})
	if r.Status != Replaced || r.Fork != "" || r.Backup == "" {
		t.Fatalf("result = %+v, want replaced with a backup", r)
	}
	if got, want := read(t, p.d), entry(sid, "one")+entry(sid, "from-source"); got != want {
		t.Errorf("destination = %q, want %q", got, want)
	}
	if got := read(t, r.Backup); got != old {
		t.Errorf("backup = %q, want the old destination %q", got, old)
	}
	if base := filepath.Base(r.Backup); !strings.HasPrefix(base, sid+".jsonl.bak-") {
		t.Errorf("backup name = %q", base)
	}
	info, err := os.Stat(p.d)
	if err != nil || !info.ModTime().Equal(t0) {
		t.Errorf("mtime = %v, %v; want the source's", info, err)
	}
	if list, _ := List(p.dst); len(list) != 1 {
		t.Errorf("sessions = %v; a backup is not a session", ids(list))
	}

	// Now both are the same: nothing more to do.
	if r := copyOne(t, p, Options{Force: true}); r.Status != Unchanged || r.Backup != "" {
		t.Errorf("rerun = %+v, want unchanged", r)
	}
	if n := len(backups(t, filepath.Dir(p.d))); n != 1 {
		t.Errorf("%d backups, want 1", n)
	}
}

func TestForceReplacesADestinationThatIsAhead(t *testing.T) {
	p := both(t)
	put(t, p.d, entry(sid, "one")+entry(sid, "from-source")+entry(sid, "extra"), t0.Add(time.Hour))
	old := read(t, p.d)

	if r := copyOne(t, p, Options{}); r.Status != Ahead {
		t.Fatalf("without force = %+v, want ahead", r)
	}
	r := copyOne(t, p, Options{Force: true})
	if r.Status != Replaced || read(t, r.Backup) != old {
		t.Fatalf("result = %+v", r)
	}
	if got := read(t, p.d); got != entry(sid, "one")+entry(sid, "from-source") {
		t.Errorf("destination = %q", got)
	}
}

func TestForceChangesNothingThatDoesNotDiverge(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	s := filepath.Join(src, "projects", "a", sid+".jsonl")
	d := filepath.Join(dst, "projects", "a", sid+".jsonl")
	p := pair{src, dst, s, d}
	put(t, s, entry(sid, "one"), t0)

	for i, w := range []Status{Copied, Unchanged} {
		if r := copyOne(t, p, Options{Force: true}); r.Status != w || r.Backup != "" {
			t.Errorf("run %d = %+v, want %v", i+1, r, w)
		}
	}
	put(t, s, entry(sid, "one")+entry(sid, "two"), t0.Add(time.Hour))
	if r := copyOne(t, p, Options{Force: true}); r.Status != Updated || r.Backup != "" {
		t.Errorf("growth = %+v, want updated", r)
	}
	if n := len(backups(t, filepath.Dir(d))); n != 0 {
		t.Errorf("%d backups of files that were fine", n)
	}
}

func TestForceBackupsDoNotCollide(t *testing.T) {
	p := both(t)
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		put(t, p.d, entry(sid, "one")+entry(sid, "again")+entry(sid, strings.Repeat("x", i)), t0.Add(time.Hour))
		r := copyOne(t, p, Options{Force: true})
		if r.Status != Replaced || seen[r.Backup] || !exists(r.Backup) {
			t.Fatalf("run %d = %+v (backups so far %v)", i, r, seen)
		}
		seen[r.Backup] = true
	}
	if n := len(backups(t, filepath.Dir(p.d))); n != 3 {
		t.Errorf("%d backups, want 3", n)
	}
}

func TestForceReplacesSidecarFilesToo(t *testing.T) {
	p := both(t)
	put(t, filepath.Join(p.src, "projects", "a", sid, "tool-results", "r.txt"), "new", t0)
	put(t, filepath.Join(p.dst, "projects", "a", sid, "tool-results", "r.txt"), "old", t0)

	r := copyOne(t, p, Options{Force: true})
	if r.Status != Replaced || r.Files != 1 {
		t.Fatalf("result = %+v", r)
	}
	f := filepath.Join(p.dst, "projects", "a", sid, "tool-results", "r.txt")
	if got := read(t, f); got != "new" {
		t.Errorf("sidecar = %q", got)
	}
	b := backups(t, filepath.Dir(f))
	if len(b) != 1 || read(t, b[0]) != "old" {
		t.Errorf("sidecar backups = %v", b)
	}
}

func TestWithoutFlagsASidecarThatDivergedIsLeftAlone(t *testing.T) {
	p := both(t)
	put(t, p.d, entry(sid, "one"), t0) // behind: the transcript is extended, the sidecar is looked at
	put(t, filepath.Join(p.src, "projects", "a", sid, "r.txt"), "new", t0)
	f := filepath.Join(p.dst, "projects", "a", sid, "r.txt")
	put(t, f, "old", t0)
	copyOne(t, p, Options{})
	if got := read(t, f); got != "old" {
		t.Errorf("sidecar = %q, want it left alone", got)
	}
}

// ---- id and reader ----

func TestForkIDIsAStableUUID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	a, b := forkID(sid), forkID("another")
	if !re.MatchString(a) || !re.MatchString(b) {
		t.Errorf("not a version 4 uuid: %s %s", a, b)
	}
	if a != forkID(sid) {
		t.Error("the id changes between calls")
	}
	if a == b || a == sid {
		t.Errorf("ids collide: %s %s %s", a, b, sid)
	}
}

func swapAll(t *testing.T, in string, sw *forkIDs, bufSize int) string {
	t.Helper()
	var out strings.Builder
	r := testSwapReader(strings.NewReader(in), noErr(sw.line))
	if _, err := io.CopyBuffer(struct{ io.Writer }{&out}, struct{ io.Reader }{r}, make([]byte, bufSize)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestSwapReader(t *testing.T) {
	sw := &forkIDs{old: "OLD-ID", fid: "NEW-ID"}
	for _, in := range []string{
		"",
		"no id here\n",
		"OLD-ID\n",
		"a OLD-ID b OLD-ID c\nsecond OLD-ID\n",
		"no newline at the end OLD-ID",
		"\n\n\nOLD-ID\n\n",
	} {
		want := strings.ReplaceAll(in, "OLD-ID", "NEW-ID")
		for _, size := range []int{1, 3, 7, 4096} {
			if got := swapAll(t, in, sw, size); got != want {
				t.Errorf("buffer %d: %q -> %q, want %q", size, in, got, want)
			}
		}
	}
}

func TestSwapReaderLongLinesAndLongerIDs(t *testing.T) {
	sw := &forkIDs{old: "OLD", fid: "A-MUCH-LONGER-ID"}
	long := "OLD" + strings.Repeat("x", 3<<20) + "OLD\n" + strings.Repeat("y", 2<<20) + "OLD"
	want := strings.ReplaceAll(long, "OLD", "A-MUCH-LONGER-ID")
	if got := swapAll(t, long, sw, 1000); got != want {
		t.Errorf("long lines: got %d bytes, want %d", len(got), len(want))
	}
}

func TestSwapReaderWithoutAnIDPassesThrough(t *testing.T) {
	in := "OLD\n"
	if got := swapAll(t, in, &forkIDs{fid: "x"}, 16); got != in {
		t.Errorf("got %q", got)
	}
}

func TestSwapReaderReportsTheSourceError(t *testing.T) {
	boom := errors.New("boom")
	r := testSwapReader(io.MultiReader(strings.NewReader("OLD\nOL"), iotest.ErrReader(boom)), noErr((&forkIDs{old: "OLD", fid: "NEW"}).line))
	got, err := io.ReadAll(r)
	if !errors.Is(err, boom) || string(got) != "NEW\nOL" {
		t.Errorf("got %q, %v; want the data read before the error, then the error", got, err)
	}
}

// ---- the id in another project folder ----

// elsewhere has the session in folder b of the source, and another session of
// the same id in folder a of the destination (Claude Code moved it to a
// worktree's folder on one side only).
func elsewhere(t *testing.T) (p pair, other string) {
	t.Helper()
	p = pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "b", sid+".jsonl")
	p.d = filepath.Join(p.dst, "projects", "b", sid+".jsonl")
	other = filepath.Join(p.dst, "projects", "a", sid+".jsonl")
	put(t, p.s, entry(sid, "one")+entry(sid, "in-the-worktree"), t0)
	put(t, other, entry(sid, "one")+entry(sid, "at-home"), t0.Add(time.Hour))
	return p, other
}

func TestAnIdInAnotherFolderIsNotWrittenNextToIt(t *testing.T) {
	for _, opt := range []Options{{}, {Force: true}} {
		p, other := elsewhere(t)
		r := copyOne(t, p, opt)
		if r.Status != Elsewhere || r.Other != "a" || r.Fork != "" || !r.Status.Skipped() {
			t.Errorf("force=%v: result = %+v, want elsewhere in a", opt.Force, r)
		}
		if exists(filepath.Dir(p.d)) {
			t.Errorf("force=%v: folder b was made at the destination", opt.Force)
		}
		if got := read(t, other); got != entry(sid, "one")+entry(sid, "at-home") {
			t.Errorf("force=%v: the other session was touched: %q", opt.Force, got)
		}
		if len(backups(t, filepath.Dir(other))) != 0 {
			t.Errorf("force=%v: a backup was made", opt.Force)
		}
	}
}

func TestForkWritesAnIdOfAnotherFolderUnderANewOne(t *testing.T) {
	p, other := elsewhere(t)
	put(t, filepath.Join(p.src, "projects", "b", sid, "subagents", "agent-1.jsonl"), entry(sid, "sub"), t0)
	fid := forkID(sid)
	r := copyOne(t, p, Options{Fork: true})
	if r.Status != Forked || r.Fork != fid || r.Other != "a" || r.Files != 1 {
		t.Fatalf("result = %+v, want forked as %s", r, fid)
	}
	fork := filepath.Join(p.dst, "projects", "b", fid+".jsonl")
	if got, want := read(t, fork), entry(fid, "one")+entry(fid, "in-the-worktree"); got != want {
		t.Errorf("fork = %q, want %q", got, want)
	}
	if got := read(t, filepath.Join(p.dst, "projects", "b", fid, "subagents", "agent-1.jsonl")); got != entry(fid, "sub") {
		t.Errorf("subagent transcript = %q", got)
	}
	if exists(p.d) || read(t, other) != entry(sid, "one")+entry(sid, "at-home") {
		t.Error("the old id was written, or the other session touched")
	}
	// Again: the fork is there, and grows with the source.
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Unchanged || r.Fork != fid {
		t.Errorf("rerun = %+v", r)
	}
	put(t, p.s, entry(sid, "one")+entry(sid, "in-the-worktree")+entry(sid, "more"), t0.Add(2*time.Hour))
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Forked || !strings.HasSuffix(read(t, fork), entry(fid, "more")) {
		t.Errorf("after growth = %+v", r)
	}
}

// The id in its own folder too: that file is the destination, as always.
func TestAnIdInItsOwnFolderIsSyncedThere(t *testing.T) {
	p, _ := elsewhere(t)
	put(t, p.d, entry(sid, "one"), t0)
	if r := copyOne(t, p, Options{}); r.Status != Updated || r.Other != "" {
		t.Errorf("result = %+v, want updated in its own folder", r)
	}
}

// noErr is a line function that cannot fail, as swapReader takes it.
func noErr(fn func([]byte) []byte) func([]byte) ([]byte, error) {
	return func(b []byte) ([]byte, error) { return fn(b), nil }
}

// testSwapReader passes r through fn line after line.
func testSwapReader(r io.Reader, fn func([]byte) ([]byte, error)) *swapReader {
	return &swapReader{src: newSeqLines(r, nil, 0), fn: func(b []byte, _ any) ([]byte, error) { return fn(b) }}
}
