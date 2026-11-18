package sessions

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions/forkids"
	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions/jsonl"
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
	fid := forkids.ForkID(sid)
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
	fid := forkids.ForkID(sid)
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
	fid := forkids.ForkID(sid)
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
	got := read(t, filepath.Join(p.dst, "projects", "a", forkids.ForkID(sid)+".jsonl"))
	if strings.Contains(got, "thinking") || strings.Contains(got, sid) || !strings.Contains(got, forkids.ForkID(sid)) {
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
	fid := forkids.ForkID(sid)
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
	// Changed at the destination after the copy: another time than the source's.
	put(t, filepath.Join(p.dst, "projects", "a", sid, "tool-results", "r.txt"), "old", t0.Add(time.Hour))

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
	put(t, f, "old", t0.Add(time.Hour)) // changed after the copy: its time differs from the source's
	copyOne(t, p, Options{})
	if got := read(t, f); got != "old" {
		t.Errorf("sidecar = %q, want it left alone", got)
	}
}

// ---- id and reader ----

func TestForkIDIsAStableUUID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	a, b := forkids.ForkID(sid), forkids.ForkID("another")
	if !re.MatchString(a) || !re.MatchString(b) {
		t.Errorf("not a version 4 uuid: %s %s", a, b)
	}
	if a != forkids.ForkID(sid) {
		t.Error("the id changes between calls")
	}
	if a == b || a == sid {
		t.Errorf("ids collide: %s %s %s", a, b, sid)
	}
}

func swapAll(t *testing.T, in string, sw *forkids.IDs, bufSize int) string {
	t.Helper()
	var out strings.Builder
	r := testSwapReader(strings.NewReader(in), noErr(sw.Line))
	if _, err := io.CopyBuffer(struct{ io.Writer }{&out}, struct{ io.Reader }{r}, make([]byte, bufSize)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestSwapReader(t *testing.T) {
	sw := &forkids.IDs{Old: "OLD-ID", FID: "NEW-ID"}
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
	sw := &forkids.IDs{Old: "OLD", FID: "A-MUCH-LONGER-ID"}
	long := "OLD" + strings.Repeat("x", 3<<20) + "OLD\n" + strings.Repeat("y", 2<<20) + "OLD"
	want := strings.ReplaceAll(long, "OLD", "A-MUCH-LONGER-ID")
	if got := swapAll(t, long, sw, 1000); got != want {
		t.Errorf("long lines: got %d bytes, want %d", len(got), len(want))
	}
}

func TestSwapReaderWithoutAnIDPassesThrough(t *testing.T) {
	in := "OLD\n"
	if got := swapAll(t, in, &forkids.IDs{FID: "x"}, 16); got != in {
		t.Errorf("got %q", got)
	}
}

func TestSwapReaderReportsTheSourceError(t *testing.T) {
	boom := errors.New("boom")
	r := testSwapReader(io.MultiReader(strings.NewReader("OLD\nOL"), iotest.ErrReader(boom)), noErr((&forkids.IDs{Old: "OLD", FID: "NEW"}).Line))
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
	fid := forkids.ForkID(sid)
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
	return &swapReader{src: jsonl.NewSeqLines(r, nil, 0), fn: func(b []byte, _ any) ([]byte, error) { return fn(b) }}
}

const (
	home     = "D--proj"
	worktree = "D--proj-worktrees-x"
	sid2     = "22222222-3333-4444-8555-666666666666"
)

// cwdEntry is a transcript line of session id that records its working directory.
func cwdEntry(id, cwd, text string) string {
	c, _ := json.Marshal(cwd)
	return `{"type":"user","sessionId":"` + id + `","cwd":` + string(c) + `,"message":{"content":"` + text + `"}}` + "\n"
}

// moved has, at the source, an older session in the folder of dir and a newer
// one begun in dir that Claude Code moved into the folder of dir's worktree.
func moved(t *testing.T) (src, dst, dir string) {
	t.Helper()
	src, dst, dir = t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
	put(t, filepath.Join(src, "projects", home, sid+".jsonl"), cwdEntry(sid, dir, "old"), t0)
	put(t, filepath.Join(src, "projects", worktree, sid2+".jsonl"),
		cwdEntry(sid2, dir, "new")+cwdEntry(sid2, filepath.Join(dir, "worktrees", "x"), "in the worktree"), t0.Add(time.Hour))
	return src, dst, dir
}

func TestASessionMovedIntoAWorktreeIsTheDirectorysOwn(t *testing.T) {
	src, dst, dir := moved(t)
	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir})
	if err != nil || len(res) != 1 {
		t.Fatalf("Copy = %+v, %v", res, err)
	}
	r := res[0]
	if r.Session.ID != sid2 || r.Session.Project != worktree || r.Session.Into != home || r.Status != Copied {
		t.Fatalf("result = %+v, want the worktree's session written into %s", r, home)
	}
	if got := read(t, filepath.Join(dst, "projects", home, sid2+".jsonl")); !strings.Contains(got, `"new"`) {
		t.Errorf("written = %q", got)
	}
	if exists(filepath.Join(dst, "projects", worktree)) {
		t.Error("the worktree's folder was made at the destination")
	}
	// Both, newest first, when the count allows.
	res, _ = Copy(src, dst, 5, Options{Project: home, Within: dir})
	if len(res) != 2 || res[0].Session.ID != sid2 || res[1].Session.ID != sid || res[1].Session.Into != "" {
		t.Errorf("results = %+v", res)
	}
}

func TestOnlyTheDirectorysSubdirectoriesCount(t *testing.T) {
	src, dst, dir := moved(t)
	// A sibling directory whose folder name starts the same, a folder whose
	// transcript names no directory, and sessions begun in subdirectories
	// (theirs: Claude Code lists them there, so a copy in dir's folder would be
	// seen nowhere).
	put(t, filepath.Join(src, "projects", home+"--rush-ws", "c.jsonl"), cwdEntry("c", filepath.Join(dir, ".rush", "ws"), "a subdirectory"), t0.Add(4*time.Hour))
	put(t, filepath.Join(src, "projects", home+"-worktrees-w1", "d.jsonl"), cwdEntry("d", filepath.Join(dir, "worktrees", "w1"), "begun in a worktree"), t0.Add(5*time.Hour))
	put(t, filepath.Join(src, "projects", home+"-other", "a.jsonl"), cwdEntry("a", dir+"-other", "sibling"), t0.Add(2*time.Hour))
	put(t, filepath.Join(src, "projects", home+"-bare", "b.jsonl"), entry("b", "no cwd"), t0.Add(3*time.Hour))
	res, err := Copy(src, dst, 10, Options{Project: home, Within: dir})
	if err != nil || len(res) != 2 || res[0].Session.ID != sid2 || res[1].Session.ID != sid {
		t.Errorf("results = %+v, %v; want the worktree's and the directory's own", res, err)
	}
	// Without Within only the folder itself.
	res, _ = Copy(src, t.TempDir(), 10, Options{Project: home})
	if len(res) != 1 || res[0].Session.ID != sid {
		t.Errorf("without Within = %+v", res)
	}
}

func TestAnIdInBothFoldersIsTakenOnceTheNewest(t *testing.T) {
	src, dst, dir := moved(t)
	put(t, filepath.Join(src, "projects", worktree, sid+".jsonl"), cwdEntry(sid, dir, "old")+entry(sid, "more"), t0.Add(2*time.Hour))
	res, err := Copy(src, dst, 10, Options{Project: home, Within: dir})
	if err != nil || len(res) != 2 {
		t.Fatalf("results = %+v, %v", res, err)
	}
	if res[0].Session.ID != sid || res[0].Session.Project != worktree {
		t.Errorf("first = %+v, want the newer copy of %s from the worktree", res[0].Session, sid)
	}
}

// The live case: the destination holds the session's original in the folder
// of the directory; the worktree's continuation is forked next to it.
func TestForkOfAMovedSessionLandsInTheDirectorysFolder(t *testing.T) {
	src, dst, dir := moved(t)
	put(t, filepath.Join(dst, "projects", home, sid2+".jsonl"), cwdEntry(sid2, dir, "original"), t0)
	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir, Fork: true})
	if err != nil || len(res) != 1 || res[0].Status != Forked {
		t.Fatalf("results = %+v, %v", res, err)
	}
	if !exists(filepath.Join(dst, "projects", home, forkids.ForkID(sid2)+".jsonl")) {
		t.Error("the fork is not in the directory's folder")
	}
}

func TestTranscriptCwd(t *testing.T) {
	dir := t.TempDir()
	long := filepath.Join(dir, "long.jsonl")
	put(t, long, `{"cwd":"C:\\x","pad":"`+strings.Repeat("a", 200<<10)+`"}`+"\n", t0)
	if got := transcriptCwd(long); got != `C:\x` {
		t.Errorf("long first line: %q", got)
	}
	late := filepath.Join(dir, "late.jsonl")
	put(t, late, strings.Repeat(`{"type":"mode"}`+"\n", 3)+`{"type":"user","cwd":"/a/b"}`+"\n", t0)
	if got := transcriptCwd(late); got != "/a/b" {
		t.Errorf("cwd on the fourth line: %q", got)
	}
	none := filepath.Join(dir, "none.jsonl")
	put(t, none, entry(sid, "x"), t0)
	if got := transcriptCwd(none); got != "" {
		t.Errorf("no cwd: %q", got)
	}
	if got := transcriptCwd(filepath.Join(dir, "missing.jsonl")); got != "" {
		t.Errorf("missing file: %q", got)
	}
}

// The live case: a session begun in the directory and continued in worktrees
// records the directory first, the worktrees later; Claude Code moved the file
// into the last worktree's folder.
func TestASessionBegunInTheDirectoryAndMovedIntoAWorktreeIsItsOwn(t *testing.T) {
	src, dst, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
	put(t, filepath.Join(src, "projects", home, sid+".jsonl"), cwdEntry(sid, dir, "old"), t0)
	put(t, filepath.Join(src, "projects", worktree, sid2+".jsonl"),
		cwdEntry(sid2, dir, "begun here")+cwdEntry(sid2, filepath.Join(dir, "web"), "a subdirectory")+
			cwdEntry(sid2, filepath.Join(dir, "worktrees", "x"), "then in the worktree"), t0.Add(time.Hour))
	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir})
	if err != nil || len(res) != 1 || res[0].Session.ID != sid2 || res[0].Session.Into != home || res[0].Status != Copied {
		t.Fatalf("results = %+v, %v; want the moved session written into %s", res, err, home)
	}
	if !exists(filepath.Join(dst, "projects", home, sid2+".jsonl")) {
		t.Error("not written into the directory's folder")
	}
}

func relocatedEntry(id, cwd string) string {
	c, _ := json.Marshal(cwd)
	return `{"type":"relocated","relocatedCwd":` + string(c) + `,"sessionId":"` + id + `"}` + "\n"
}

func ownerEntry(id string) string {
	return `{"type":"session-owner","accountUuid":"cccccccc-0000-4000-8000-000000000001","organizationUuid":"cccccccc-0000-4000-8000-000000000002","sessionId":"` + id + `"}` + "\n"
}

// Claude Code lists a session under the last relocatedCwd of its transcript:
// one written into the directory's folder must say the directory, or the
// directory's /resume hides it as a worktree's (seen live).
func TestASessionWrittenIntoTheDirectorySaysItLivesThere(t *testing.T) {
	src, dst, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
	wt := filepath.Join(dir, "worktrees", "x")
	body := cwdEntry(sid2, dir, "begun here") + relocatedEntry(sid2, wt) + ownerEntry(sid2) +
		cwdEntry(sid2, wt, `text that says {"type":"relocated","relocatedCwd":"elsewhere"}`) + relocatedEntry(sid2, wt)
	put(t, filepath.Join(src, "projects", worktree, sid2+".jsonl"), body, t0)

	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir})
	if err != nil || len(res) != 1 || res[0].Status != Copied {
		t.Fatalf("results = %+v, %v", res, err)
	}
	want := strings.ReplaceAll(body, relocatedEntry(sid2, wt), relocatedEntry(sid2, dir))
	if got := read(t, filepath.Join(dst, "projects", home, sid2+".jsonl")); got != want {
		t.Errorf("copy =\n%s\nwant\n%s", got, want)
	}
	if res, _ = Copy(src, dst, 1, Options{Project: home, Within: dir}); res[0].Status != Unchanged {
		t.Errorf("rerun = %+v", res[0])
	}
}

func lastPromptEntry(id, prompt string) string {
	return `{"type":"last-prompt","lastPrompt":"` + prompt + `","leafUuid":"aaaaaaaa-0000-4000-8000-000000000001","sessionId":"` + id + `"}` + "\n"
}

// Claude Code lists a transcript with no relocation in its tail under the cwd
// of its first lines: a session begun in another directory and kept in this
// one's folder would be hidden from its /resume (seen live, Windows with
// worktrees). The copy says it lives here after each last-prompt line, and
// stays append-only as the source grows.
func TestASessionBegunElsewhereSaysItLivesHere(t *testing.T) {
	src, dst, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
	other := filepath.Join(t.TempDir(), "pc")
	path := filepath.Join(src, "projects", home, sid+".jsonl")
	first := cwdEntry(sid, other, "begun there") + lastPromptEntry(sid, "one") + cwdEntry(sid, dir, "then here")
	put(t, path, first, t0)

	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir, StripThinking: true})
	if err != nil || len(res) != 1 || res[0].Status != Copied {
		t.Fatalf("results = %+v, %v", res, err)
	}
	stamp := relocatedEntry(sid, dir)
	want := cwdEntry(sid, other, "begun there") + lastPromptEntry(sid, "one") + stamp + cwdEntry(sid, dir, "then here")
	out := filepath.Join(dst, "projects", home, sid+".jsonl")
	if got := read(t, out); got != want {
		t.Errorf("copy =\n%s\nwant\n%s", got, want)
	}

	more := `{"type":"assistant","sessionId":"` + sid + `","message":{"content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"y"}]}}` + "\n" +
		lastPromptEntry(sid, "thinking\\\" two")
	put(t, path, first+more, t0.Add(time.Hour))
	res, err = Copy(src, dst, 1, Options{Project: home, Within: dir, StripThinking: true})
	if err != nil || res[0].Status != Updated {
		t.Fatalf("grown source = %+v, %v", res, err)
	}
	got := read(t, out)
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, lastPromptEntry(sid, "thinking\\\" two")+stamp) || strings.Contains(got, `"thinking":"x"`) {
		t.Errorf("extended copy =\n%s", got)
	}
	if res, _ = Copy(src, dst, 1, Options{Project: home, Within: dir, StripThinking: true}); res[0].Status != Unchanged {
		t.Errorf("rerun = %+v", res[0])
	}

	// A fork names its own id.
	fdst := t.TempDir()
	put(t, filepath.Join(fdst, "projects", home, sid+".jsonl"), cwdEntry(sid, other, "the original"), t0)
	res, err = Copy(src, fdst, 1, Options{Project: home, Within: dir, Fork: true})
	if err != nil || res[0].Status != Forked {
		t.Fatalf("fork = %+v, %v", res, err)
	}
	fid := forkids.ForkID(sid)
	if got := read(t, filepath.Join(fdst, "projects", home, fid+".jsonl")); !strings.Contains(got, `"type":"last-prompt","lastPrompt":"one"`) ||
		!strings.Contains(got, relocatedEntry(fid, dir)) || strings.Contains(got, sid) {
		t.Errorf("fork =\n%s", got)
	}
}

// A session begun in the directory gets no relocation.
func TestASessionBegunHereGetsNoRelocation(t *testing.T) {
	src, dst, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
	body := cwdEntry(sid, dir, "x") + lastPromptEntry(sid, "one")
	put(t, filepath.Join(src, "projects", home, sid+".jsonl"), body, t0)
	if _, err := Copy(src, dst, 1, Options{Project: home, Within: dir}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "projects", home, sid+".jsonl")); got != body {
		t.Errorf("copy = %q", got)
	}
}

func TestRelocatedLine(t *testing.T) {
	h, _ := json.Marshal(`D:\p`)
	if got := string(relocatedLine([]byte(lastPromptEntry(sid, "x")), h)); got != relocatedEntry(sid, `D:\p`) {
		t.Errorf("got %q", got)
	}
	crlf := strings.TrimSuffix(lastPromptEntry(sid, "x"), "\n") + "\r\n"
	if got := string(relocatedLine([]byte(crlf), h)); got != strings.TrimSuffix(relocatedEntry(sid, `D:\p`), "\n")+"\r\n" {
		t.Errorf("crlf: %q", got)
	}
	if got := relocatedLine([]byte(`{"type":"last-prompt"}`+"\n"), h); got != nil {
		t.Errorf("no session id: %q", got)
	}
}

// A session of the directory's own folder keeps what it says.
func TestASessionOfItsOwnFolderKeepsItsRelocation(t *testing.T) {
	src, dst, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
	body := cwdEntry(sid, dir, "x") + relocatedEntry(sid, filepath.Join(dir, "worktrees", "x")) + ownerEntry(sid)
	put(t, filepath.Join(src, "projects", home, sid+".jsonl"), body, t0)
	if _, err := Copy(src, dst, 1, Options{Project: home, Within: dir}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "projects", home, sid+".jsonl")); got != body {
		t.Errorf("copy = %q", got)
	}
}

// A fork has no owner: Claude Code stamps it with the account that opens it.
// (Its uuids would be mapped like every id, naming no account at all.)
func TestAForkHasNoOwner(t *testing.T) {
	src, dst, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
	wt := filepath.Join(dir, "worktrees", "x")
	put(t, filepath.Join(src, "projects", worktree, sid2+".jsonl"),
		cwdEntry(sid2, dir, "begun here")+ownerEntry(sid2)+relocatedEntry(sid2, wt)+ownerEntry(sid2), t0.Add(time.Hour))
	put(t, filepath.Join(src, "projects", worktree, sid2, "subagents", "agent-a1.jsonl"), ownerEntry(sid2)+entry(sid2, "sub"), t0)
	put(t, filepath.Join(dst, "projects", home, sid2+".jsonl"), cwdEntry(sid2, dir, "the original"), t0)

	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir, Fork: true})
	if err != nil || len(res) != 1 || res[0].Status != Forked {
		t.Fatalf("results = %+v, %v", res, err)
	}
	fid := forkids.ForkID(sid2)
	got := read(t, filepath.Join(dst, "projects", home, fid+".jsonl"))
	if want := cwdEntry(fid, dir, "begun here") + relocatedEntry(fid, dir); got != want {
		t.Errorf("fork =\n%s\nwant\n%s", got, want)
	}
	if got := read(t, filepath.Join(dst, "projects", home, fid, "subagents", "agent-a1.jsonl")); got != entry(fid, "sub") {
		t.Errorf("subagent transcript = %q", got)
	}
}

// A worktree's folder holds sessions moved there and sessions begun there:
// each is judged by its own transcript, whatever order the folder is read in.
func TestAWorktreesFolderIsJudgedSessionBySession(t *testing.T) {
	for _, movedFirst := range []bool{true, false} {
		src, dst, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
		movedAt, begunAt := t0.Add(2*time.Hour), t0.Add(time.Hour)
		if !movedFirst {
			movedAt, begunAt = begunAt, movedAt
		}
		put(t, filepath.Join(src, "projects", worktree, sid2+".jsonl"), cwdEntry(sid2, dir, "moved here"), movedAt)
		put(t, filepath.Join(src, "projects", worktree, sid+".jsonl"), cwdEntry(sid, filepath.Join(dir, "worktrees", "x"), "begun here"), begunAt)
		res, err := Copy(src, dst, 10, Options{Project: home, Within: dir})
		if err != nil || len(res) != 1 || res[0].Session.ID != sid2 {
			t.Errorf("moved first %v: results = %+v, %v; want only the moved session", movedFirst, res, err)
		}
	}
}

// A fork's id taken in another folder of the destination (a fork written
// there before) is not written a second time.
func TestAForkIDHeldInAnotherFolderIsNotWrittenAgain(t *testing.T) {
	src, dst, dir := moved(t)
	put(t, filepath.Join(dst, "projects", home, sid2+".jsonl"), cwdEntry(sid2, dir, "the original"), t0)
	put(t, filepath.Join(dst, "projects", worktree, forkids.ForkID(sid2)+".jsonl"), "an earlier fork\n", t0)
	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir, Fork: true})
	if err != nil || len(res) != 1 {
		t.Fatalf("results = %+v, %v", res, err)
	}
	r := res[0]
	if r.Status != Elsewhere || r.Fork != forkids.ForkID(sid2) || r.Other != worktree || !r.Status.Skipped() {
		t.Errorf("result = %+v, want the fork elsewhere in %s", r, worktree)
	}
	if exists(filepath.Join(dst, "projects", home, forkids.ForkID(sid2)+".jsonl")) {
		t.Error("a second session of the fork's id was written")
	}
}

// --force does not write next to, or over, an id another folder holds.
func TestForceLeavesAnIdHeldElsewhere(t *testing.T) {
	src, dst, dir := moved(t)
	put(t, filepath.Join(dst, "projects", worktree, sid2+".jsonl"), "another session\n", t0)
	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir, Force: true})
	if err != nil || len(res) != 1 || res[0].Status != Elsewhere || res[0].Other != worktree {
		t.Fatalf("results = %+v, %v", res, err)
	}
	if exists(filepath.Join(dst, "projects", home, sid2+".jsonl")) || read(t, filepath.Join(dst, "projects", worktree, sid2+".jsonl")) != "another session\n" {
		t.Error("--force wrote next to, or over, the other folder's session")
	}
	if len(backups(t, filepath.Join(dst, "projects", worktree))) != 0 {
		t.Error("--force made a backup of the other folder's session")
	}
}

// Claude Code may write a marker line's fields in another order: it is still
// the marker; a message quoting the types is not.
func TestMarkerLinesAreKnownByTheirType(t *testing.T) {
	src, dst, dir := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
	wt, _ := json.Marshal(filepath.Join(dir, "worktrees", "x"))
	home2, _ := json.Marshal(dir)
	quote := `{"type":"user","sessionId":"%s","message":{"content":"quoting \"type\":\"relocated\" and \"session-owner\""}}` + "\n"
	body := cwdEntry(sid2, dir, "begun here") +
		`{"sessionId":"` + sid2 + `","relocatedCwd":` + string(wt) + `,"type":"relocated"}` + "\n" +
		`{"sessionId":"` + sid2 + `","type":"session-owner","accountUuid":"cccccccc-0000-4000-8000-000000000001"}` + "\n" +
		strings.Replace(quote, "%s", sid2, 1)
	put(t, filepath.Join(src, "projects", worktree, sid2+".jsonl"), body, t0.Add(time.Hour))
	put(t, filepath.Join(dst, "projects", home, sid2+".jsonl"), cwdEntry(sid2, dir, "the original"), t0)

	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir, Fork: true})
	if err != nil || len(res) != 1 || res[0].Status != Forked {
		t.Fatalf("results = %+v, %v", res, err)
	}
	fid := forkids.ForkID(sid2)
	want := cwdEntry(fid, dir, "begun here") +
		`{"sessionId":"` + fid + `","relocatedCwd":` + string(home2) + `,"type":"relocated"}` + "\n" +
		strings.Replace(quote, "%s", fid, 1)
	if got := read(t, filepath.Join(dst, "projects", home, fid+".jsonl")); got != want {
		t.Errorf("fork =\n%s\nwant\n%s", got, want)
	}
}
