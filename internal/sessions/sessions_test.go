package sessions

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions/forkids"
	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions/jsonl"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func put(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ids(list []Session) string {
	var out []string
	for _, s := range list {
		out = append(out, s.ID)
	}
	return strings.Join(out, ",")
}

func TestListNewestFirstIgnoresSidecars(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "projects")
	put(t, filepath.Join(p, "a", "1.jsonl"), "x\n", t0)
	put(t, filepath.Join(p, "a", "2.jsonl"), "x\n", t0.Add(2*time.Hour))
	put(t, filepath.Join(p, "b", "3.jsonl"), "x\n", t0.Add(time.Hour))
	put(t, filepath.Join(p, "a", "2", "subagents", "agent-x.jsonl"), "x\n", t0.Add(5*time.Hour))
	put(t, filepath.Join(p, "a", "notes.txt"), "x\n", t0.Add(6*time.Hour))
	put(t, filepath.Join(p, "stray.jsonl"), "x\n", t0.Add(7*time.Hour))

	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(list); got != "2,3,1" {
		t.Errorf("order = %s, want 2,3,1", got)
	}
	if list[0].Project != "a" || list[1].Project != "b" {
		t.Errorf("projects = %q %q", list[0].Project, list[1].Project)
	}
}

func TestListMissingDir(t *testing.T) {
	list, err := List(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(list) != 0 {
		t.Errorf("List = %v, %v", list, err)
	}
}

func TestCopyNewestWithSidecarsAndNothingElse(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	p := filepath.Join(src, "projects")
	put(t, filepath.Join(p, "a", "old.jsonl"), "old\n", t0)
	put(t, filepath.Join(p, "a", "mid.jsonl"), "mid\n", t0.Add(time.Hour))
	put(t, filepath.Join(p, "a", "new.jsonl"), "new\n", t0.Add(2*time.Hour))
	put(t, filepath.Join(p, "a", "new", "tool-results", "r.txt"), "result", t0.Add(2*time.Hour))
	put(t, filepath.Join(p, "a", "new", "subagents", "agent-1.jsonl"), "{}\n", t0.Add(2*time.Hour))
	// Neighbours of projects/ must never travel.
	put(t, filepath.Join(src, ".credentials.json"), "SECRET", t0)
	put(t, filepath.Join(src, "history.jsonl"), "h\n", t0)
	put(t, filepath.Join(src, "sessions", "1.json"), "{\"peerToken\":\"x\"}", t0)

	res, err := Copy(src, dst, 2, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Session.ID != "new" || res[1].Session.ID != "mid" {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Status != Copied || res[0].Files != 2 {
		t.Errorf("new = %+v", res[0])
	}

	got := map[string]bool{}
	_ = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dst, path)
			got[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	want := []string{
		"projects/a/new.jsonl", "projects/a/mid.jsonl",
		"projects/a/new/tool-results/r.txt", "projects/a/new/subagents/agent-1.jsonl",
	}
	if len(got) != len(want) {
		t.Errorf("files = %v, want exactly %v", got, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %s (have %v)", w, got)
		}
	}

	info, err := os.Stat(filepath.Join(dst, "projects", "a", "new.jsonl"))
	if err != nil || !info.ModTime().Equal(t0.Add(2*time.Hour)) {
		t.Errorf("mtime = %v, %v; want the source's", info, err)
	}
}

func TestCopyAppendOnlyRules(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	s := filepath.Join(src, "projects", "a", "s.jsonl")
	d := filepath.Join(dst, "projects", "a", "s.jsonl")
	put(t, s, "1\n", t0)

	status := func() Status {
		t.Helper()
		res, err := Copy(src, dst, 5, Options{})
		if err != nil || len(res) != 1 || res[0].Err != nil {
			t.Fatalf("Copy = %+v, %v", res, err)
		}
		return res[0].Status
	}

	if st := status(); st != Copied {
		t.Fatalf("first = %v", st)
	}
	if st := status(); st != Unchanged {
		t.Errorf("second = %v, want unchanged", st)
	}

	put(t, s, "1\n2\n", t0.Add(time.Hour))
	if st := status(); st != Updated || read(t, d) != "1\n2\n" {
		t.Errorf("source grew: %v %q", st, read(t, d))
	}

	// The destination got ahead on its own: keep it.
	put(t, d, "1\n2\n3\n", t0.Add(2*time.Hour))
	if st := status(); st != Ahead || read(t, d) != "1\n2\n3\n" {
		t.Errorf("destination ahead: %v %q", st, read(t, d))
	}

	// Both sides moved on differently: keep the destination, report it.
	put(t, s, "1\n2\nX\n", t0.Add(3*time.Hour))
	if st := status(); st != Diverged || read(t, d) != "1\n2\n3\n" {
		t.Errorf("diverged: %v %q", st, read(t, d))
	}
}

func TestCopyCutsPartialLastLine(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	put(t, filepath.Join(src, "projects", "a", "live.jsonl"), "{\"a\":1}\n{\"b\":", t0)
	if _, err := Copy(src, dst, 1, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "projects", "a", "live.jsonl")); got != "{\"a\":1}\n" {
		t.Errorf("copied %q", got)
	}
}

func TestCopyGuards(t *testing.T) {
	dir := t.TempDir()
	if _, err := Copy(dir, dir, 1, Options{}); err == nil {
		t.Error("same directory accepted")
	}
	if _, err := Copy(dir, t.TempDir(), 0, Options{}); err == nil {
		t.Error("count 0 accepted")
	}
	res, err := Copy(t.TempDir(), t.TempDir(), 3, Options{})
	if err != nil || len(res) != 0 {
		t.Errorf("empty source = %v, %v", res, err)
	}
}

func TestCopyStripThinkingIsRepeatable(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	s := filepath.Join(src, "projects", "a", "s.jsonl")
	d := filepath.Join(dst, "projects", "a", "s.jsonl")
	put(t, s, transcript, t0)

	opt := Options{StripThinking: true}
	res, err := Copy(src, dst, 1, opt)
	if err != nil || res[0].Removed != 2 {
		t.Fatalf("Copy = %+v, %v", res, err)
	}
	if strings.Contains(read(t, d), "thinking") {
		t.Errorf("thinking left in %q", read(t, d))
	}

	if res, _ = Copy(src, dst, 1, opt); res[0].Status != Unchanged {
		t.Errorf("rerun = %v, want unchanged", res[0].Status)
	}

	// The session continues at the source: the export extends the earlier one.
	put(t, s, transcript+`{"type":"user","uuid":"u3","parentUuid":"a2","message":{"role":"user","content":"more"}}`+"\n", t0.Add(time.Hour))
	if res, _ = Copy(src, dst, 1, opt); res[0].Status != Updated {
		t.Errorf("after growth = %v, want updated", res[0].Status)
	}
}

func TestCopyLargeFileAcrossChunks(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	s := filepath.Join(src, "projects", "a", "big.jsonl")
	d := filepath.Join(dst, "projects", "a", "big.jsonl")
	big := strings.Repeat(strings.Repeat("x", 999)+"\n", 3500) // ~3.5 MB: several I/O chunks
	put(t, s, big, t0)

	var seen []Status
	opt := Options{OnResult: func(r Result) { seen = append(seen, r.Status) }}
	run := func() Status {
		t.Helper()
		res, err := Copy(src, dst, 1, opt)
		if err != nil || len(res) != 1 || res[0].Err != nil {
			t.Fatalf("Copy = %+v, %v", res, err)
		}
		return res[0].Status
	}

	if st := run(); st != Copied || read(t, d) != big {
		t.Fatalf("first = %v", st)
	}
	if st := run(); st != Unchanged {
		t.Errorf("second = %v", st)
	}

	grown := big + "tail\n"
	put(t, s, grown, t0.Add(time.Hour))
	if st := run(); st != Updated || read(t, d) != grown {
		t.Errorf("grown = %v", st)
	}

	// One byte differs deep inside an earlier chunk: nothing is touched.
	b := []byte(grown)
	b[3_000_000] = 'Y'
	put(t, s, string(b), t0.Add(2*time.Hour))
	if st := run(); st != Diverged || read(t, d) != grown {
		t.Errorf("diverged = %v", st)
	}

	if len(seen) != 4 {
		t.Errorf("OnResult called %d times, want 4", len(seen))
	}
	_ = filepath.WalkDir(dst, func(p string, de fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".ccgp-tmp") {
			t.Errorf("temp file left behind: %s", p)
		}
		return nil
	})
}

func TestCopyEmptyTranscript(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	put(t, filepath.Join(src, "projects", "a", "e.jsonl"), "", t0)
	res, err := Copy(src, dst, 1, Options{})
	if err != nil || res[0].Err != nil || res[0].Status != Copied {
		t.Fatalf("Copy = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "projects", "a", "e.jsonl")); err != nil {
		t.Errorf("empty transcript not created: %v", err)
	}
}

func TestCopyProjectOnly(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	p := filepath.Join(src, "projects")
	put(t, filepath.Join(p, "D--dev-here", "1.jsonl"), "one\n", t0)
	put(t, filepath.Join(p, "D--dev-here", "2.jsonl"), "two\n", t0.Add(time.Hour))
	put(t, filepath.Join(p, "D--dev-here", "3.jsonl"), "three\n", t0.Add(2*time.Hour))
	put(t, filepath.Join(p, "D--dev-other", "9.jsonl"), "other\n", t0.Add(5*time.Hour)) // newer, but another project

	res, err := Copy(src, dst, 2, Options{Project: "d--dev-here"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Session.ID != "3" || res[1].Session.ID != "2" {
		t.Fatalf("copied %+v, want the 2 newest sessions of D--dev-here", res)
	}
	if _, err := os.Stat(filepath.Join(dst, "projects", "D--dev-other")); err == nil {
		t.Error("another project was copied")
	}

	res, err = Copy(src, dst, 5, Options{Project: "D--dev-missing"})
	if err != nil || len(res) != 0 {
		t.Errorf("unknown project: %+v, %v; want nothing", res, err)
	}
}

func TestCopyReportsStartAndProgress(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	big := strings.Repeat(strings.Repeat("x", 999)+"\n", 3500) // several I/O chunks
	put(t, filepath.Join(src, "projects", "a", "1.jsonl"), big, t0)
	put(t, filepath.Join(src, "projects", "a", "2.jsonl"), big, t0.Add(time.Hour))

	var starts []string
	reports := map[string][]int64{} // session id -> bytes reported, in order
	totals := map[string]int64{}
	opt := Options{
		ProgressEvery: -1, // after every read, independent of the clock
		OnStart: func(i, n int, s Session) {
			starts = append(starts, fmt.Sprintf("%s:%d/%d", s.ID, i, n))
		},
		OnProgress: func(s Session, done, total int64) {
			reports[s.ID] = append(reports[s.ID], done)
			totals[s.ID] = total
		},
	}
	if _, err := Copy(src, dst, 2, opt); err != nil {
		t.Fatal(err)
	}
	if strings.Join(starts, " ") != "2:1/2 1:2/2" {
		t.Errorf("starts = %v", starts)
	}
	for _, id := range []string{"1", "2"} {
		got := reports[id]
		if len(got) < 2 {
			t.Fatalf("session %s: %d progress reports, want one per chunk", id, len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i] <= got[i-1] {
				t.Errorf("session %s: progress %v is not increasing", id, got)
				break
			}
		}
		if last := got[len(got)-1]; last != int64(len(big)) || totals[id] != int64(len(big)) {
			t.Errorf("session %s: last report %d of %d, want %d", id, last, totals[id], len(big))
		}
	}
}

func TestProgressIsThrottled(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	big := strings.Repeat(strings.Repeat("x", 999)+"\n", 3500)
	put(t, filepath.Join(src, "projects", "a", "1.jsonl"), big, t0)

	calls := 0
	opt := Options{ProgressEvery: time.Hour, OnProgress: func(Session, int64, int64) { calls++ }}
	if _, err := Copy(src, dst, 1, opt); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("OnProgress called %d times within the interval, want 0", calls)
	}
}

func TestProgressWhileStrippingThinking(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	line := `{"type":"assistant","uuid":"u","message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("y", 900) + `"}]}}` + "\n"
	think := `{"type":"assistant","uuid":"t","parentUuid":"u","message":{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"}]}}` + "\n"
	put(t, filepath.Join(src, "projects", "a", "1.jsonl"), strings.Repeat(line, 3500)+think, t0)

	var last, total int64
	opt := Options{StripThinking: true, ProgressEvery: -1, OnProgress: func(_ Session, done, tot int64) { last, total = done, tot }}
	res, err := Copy(src, dst, 1, opt)
	if err != nil || len(res) != 1 || res[0].Err != nil {
		t.Fatalf("Copy = %+v, %v", res, err)
	}
	if res[0].Removed != 1 {
		t.Errorf("removed %d thinking blocks, want 1", res[0].Removed)
	}
	if total == 0 || last != total {
		t.Errorf("progress ended at %d of %d, want the whole transcript", last, total)
	}
	if got := read(t, filepath.Join(dst, "projects", "a", "1.jsonl")); strings.Contains(got, `"thinking"`) {
		t.Error("thinking block survived")
	}
}

// A plain sidecar place wrote is not read again: it is recognised by its size
// and time. A stripped one is never trusted that way.
func TestUnchangedSidecarsAreNotReadAgain(t *testing.T) {
	// corrupt rewrites the destination copy with other content of the same
	// length and the source's time: indistinguishable from the copy without
	// reading it.
	corrupt := func(t *testing.T, srcFile, dstFile string) {
		t.Helper()
		b, err := os.ReadFile(srcFile)
		if err != nil {
			t.Fatal(err)
		}
		rot := append([]byte(nil), b...)
		rot[0] ^= 0xff
		info, err := os.Stat(srcFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dstFile, rot, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dstFile, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a plain sidecar is skipped, even by --force", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		srcFile := filepath.Join(src, "projects", "a", sid, "tool-results", "r.txt")
		dstFile := filepath.Join(dst, "projects", "a", sid, "tool-results", "r.txt")
		put(t, filepath.Join(src, "projects", "a", sid+".jsonl"), entry(sid, "one"), t0)
		put(t, srcFile, "the result", t0)
		res, err := Copy(src, dst, 1, Options{})
		if err != nil || len(res) != 1 || res[0].Status != Copied || res[0].Files != 1 {
			t.Fatalf("first copy = %+v, %v", res, err)
		}

		if res, _ = Copy(src, dst, 1, Options{}); res[0].Status != Unchanged || res[0].Files != 0 {
			t.Fatalf("a rerun over the unchanged session = %+v, want no sidecar written", res[0])
		}
		// Same size, same time, other content: the copy is trusted, not read,
		// so even --force leaves the disguised content alone.
		corrupt(t, srcFile, dstFile)
		if res, _ = Copy(src, dst, 1, Options{Force: true}); res[0].Status != Unchanged || res[0].Files != 0 {
			t.Fatalf("a forced rerun over the disguised sidecar = %+v, want it unread", res[0])
		}
		if got := read(t, dstFile); got == "the result" {
			t.Error("the disguised sidecar was read and written again")
		}
		// Its time gives it away: the full comparison runs, and --force
		// replaces it.
		mtime := t0.Add(time.Minute)
		if err := os.Chtimes(dstFile, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		if res, _ = Copy(src, dst, 1, Options{Force: true}); res[0].Files != 1 {
			t.Fatalf("a forced rerun = %+v, want the sidecar replaced", res[0])
		}
		if got := read(t, dstFile); got != "the result" {
			t.Errorf("sidecar = %q, want the source's content", got)
		}
	})

	t.Run("a stripped sidecar is read again", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		srcFile := filepath.Join(src, "projects", "a", sid, "subagents", "ag.jsonl")
		dstFile := filepath.Join(dst, "projects", "a", sid, "subagents", "ag.jsonl")
		put(t, filepath.Join(src, "projects", "a", sid+".jsonl"), entry(sid, "one"), t0)
		put(t, srcFile, entry(sid, "agent"), t0)
		if res, err := Copy(src, dst, 1, Options{StripThinking: true}); err != nil || res[0].Files != 1 {
			t.Fatalf("first copy = %+v, %v", res, err)
		}

		// The copy of a stripped file is not trusted by size and time: it is
		// compared in full, and --force replaces it.
		corrupt(t, srcFile, dstFile)
		res, _ := Copy(src, dst, 1, Options{StripThinking: true, Force: true})
		if res[0].Status != Unchanged || res[0].Files != 1 {
			t.Fatalf("a forced rerun = %+v, want the stripped sidecar compared and replaced", res[0])
		}
		if got := read(t, dstFile); got != entry(sid, "agent") {
			t.Errorf("sidecar = %q, want the source's content", got)
		}
	})
}

// plainLineFn is a jsonl.LineFn of fn, without notes.
func plainLineFn(fn func([]byte) []byte) jsonl.LineFn {
	if fn == nil {
		return nil
	}
	return func(b []byte) ([]byte, any) { return fn(b), nil }
}

const slug = "golden-swimming-bengio"

// taskJSON is a task file as Claude Code writes it.
func taskJSON(id, status, note string) string {
	return `{"id":"` + id + `","subject":"s` + id + `","description":"` + note + `","status":"` + status + `","blocks":[],"blockedBy":[]}`
}

func putFiles(t *testing.T, dir string, files map[string]string, mtime time.Time) {
	t.Helper()
	for name, body := range files {
		put(t, filepath.Join(dir, name), body, mtime)
	}
}

func names(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return strings.Join(out, " ")
}

// slugEntry is a transcript line of session id that names its slug.
func slugEntry(id, slug, text string) string {
	return `{"type":"user","sessionId":"` + id + `","slug":"` + slug + `","message":{"content":"` + text + `"}}` + "\n"
}

// fresh has, at the source, a session with three tasks, two file versions and
// a plan, and nothing at the destination.
func fresh(t *testing.T) pair {
	t.Helper()
	p := pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "a", sid+".jsonl")
	p.d = filepath.Join(p.dst, "projects", "a", sid+".jsonl")
	put(t, p.s, slugEntry(sid, slug, "one"), t0)
	putFiles(t, filepath.Join(p.src, "tasks", sid), map[string]string{
		"1.json": taskJSON("1", "completed", "done"), "2.json": taskJSON("2", "in_progress", "doing"),
		"3.json": taskJSON("3", "pending", "to do"), ".highwatermark": "3", ".lock": "",
	}, t0)
	putFiles(t, filepath.Join(p.src, "file-history", sid), map[string]string{"000878b2dc7b972d@v1": "package a\n", "000878b2dc7b972d@v2": "package b\n"}, t0)
	put(t, filepath.Join(p.src, "plans", slug+".md"), "# the plan\n", t0)
	put(t, filepath.Join(p.src, "plans", "another-session-plan.md"), "# not this session's\n", t0)
	put(t, filepath.Join(p.src, "session-env", sid, "env.sh"), "export X=1\n", t0)
	return p
}

func tasksOf(p pair, id string) string { return filepath.Join(p.dst, "tasks", id) }

func historyOf(p pair, id string) string { return filepath.Join(p.dst, "file-history", id) }

// A copied session has the tasks, the file versions and the plan of the
// original; the lock, the session environment and other sessions' plans stay.
func TestArtifactsComeWithTheSession(t *testing.T) {
	p := fresh(t)
	r := copyOne(t, p, Options{})
	if r.Status != Copied || r.Tasks != 4 || r.History != 2 || r.Plans != 1 {
		t.Fatalf("result = %+v, want copied with 3 tasks and the mark, 2 versions, 1 plan", r)
	}
	if got := names(t, tasksOf(p, sid)); got != ".highwatermark 1.json 2.json 3.json" {
		t.Errorf("task files = %q (the lock is each Claude Code's own)", got)
	}
	for name, want := range map[string]string{"2.json": taskJSON("2", "in_progress", "doing"), ".highwatermark": "3"} {
		if got := read(t, filepath.Join(tasksOf(p, sid), name)); got != want {
			t.Errorf("%s = %q", name, got)
		}
	}
	if got := names(t, historyOf(p, sid)); got != "000878b2dc7b972d@v1 000878b2dc7b972d@v2" || read(t, filepath.Join(historyOf(p, sid), "000878b2dc7b972d@v2")) != "package b\n" {
		t.Errorf("file versions = %q", got)
	}
	if got := names(t, filepath.Join(p.dst, "plans")); got != slug+".md" || read(t, filepath.Join(p.dst, "plans", slug+".md")) != "# the plan\n" {
		t.Errorf("plans = %q", got)
	}
	if exists(filepath.Join(p.dst, "session-env")) {
		t.Error("the session environment was copied")
	}
	// Again: nothing to do, and the file versions are not read again.
	if r := copyOne(t, p, Options{}); r.Status != Unchanged || r.Tasks+r.History+r.Plans != 0 {
		t.Errorf("rerun = %+v", r)
	}
}

// The source goes on: a task changes, one is added, one is deleted, a file
// gets a new version, the plan is rewritten. The copy follows; the deleted
// task is kept aside, not lost.
func TestArtifactsFollowTheSource(t *testing.T) {
	p := fresh(t)
	copyOne(t, p, Options{})
	later := t0.Add(time.Hour)
	put(t, p.s, slugEntry(sid, slug, "one")+slugEntry(sid, slug, "two"), later)
	putFiles(t, filepath.Join(p.src, "tasks", sid), map[string]string{"2.json": taskJSON("2", "completed", "doing"), "4.json": taskJSON("4", "pending", "new"), ".highwatermark": "4"}, later)
	if err := os.Remove(filepath.Join(p.src, "tasks", sid, "3.json")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(p.src, "file-history", sid, "000878b2dc7b972d@v3"), "package c\n", later)
	put(t, filepath.Join(p.src, "plans", slug+".md"), "# the plan, revised\n", later)

	r := copyOne(t, p, Options{})
	if r.Status != Updated || r.Tasks != 4 || r.History != 1 || r.Plans != 1 {
		t.Fatalf("result = %+v, want updated: task 2 changed, 4 added, the mark, 3 taken out; 1 version; the plan", r)
	}
	got := names(t, tasksOf(p, sid))
	if !strings.Contains(got, "4.json") || strings.Contains(" "+got+" ", " 3.json ") || !strings.Contains(got, "3.json.bak-") {
		t.Errorf("task files = %q", got)
	}
	if strings.Contains(got, "2.json.bak-") || read(t, filepath.Join(tasksOf(p, sid), "2.json")) != taskJSON("2", "completed", "doing") {
		t.Errorf("task 2: an older copy is no backup: %q", got)
	}
	if read(t, filepath.Join(p.dst, "plans", slug+".md")) != "# the plan, revised\n" || strings.Contains(names(t, filepath.Join(p.dst, "plans")), ".bak-") {
		t.Errorf("plans = %q", names(t, filepath.Join(p.dst, "plans")))
	}
}

// A session copied before its artifacts were (the transcript is the same
// already) gets them on the next run.
func TestArtifactsReachASessionCopiedWithoutThem(t *testing.T) {
	p := fresh(t)
	put(t, p.d, slugEntry(sid, slug, "one"), t0)
	if r := copyOne(t, p, Options{}); r.Status != Unchanged || r.Tasks != 4 || r.History != 2 || r.Plans != 1 {
		t.Fatalf("result = %+v, want unchanged with the artifacts brought", r)
	}
}

// A destination that went its own way keeps its own tasks, versions and plan.
func TestArtifactsOfASessionLeftAloneAreLeftAlone(t *testing.T) {
	for name, dst := range map[string]string{"diverged": slugEntry(sid, slug, "other"), "ahead": slugEntry(sid, slug, "one") + slugEntry(sid, slug, "more")} {
		p := fresh(t)
		put(t, p.d, dst, t0.Add(time.Hour))
		putFiles(t, tasksOf(p, sid), map[string]string{"1.json": taskJSON("1", "pending", "mine"), "9.json": taskJSON("9", "pending", "mine")}, t0.Add(time.Hour))
		put(t, filepath.Join(p.dst, "plans", slug+".md"), "# mine\n", t0.Add(time.Hour))
		r := copyOne(t, p, Options{})
		if !r.Status.Skipped() || r.Tasks+r.History+r.Plans != 0 {
			t.Errorf("%s: result = %+v", name, r)
		}
		if got := names(t, tasksOf(p, sid)); got != "1.json 9.json" || read(t, filepath.Join(tasksOf(p, sid), "1.json")) != taskJSON("1", "pending", "mine") {
			t.Errorf("%s: the destination's tasks were touched: %q", name, got)
		}
		if exists(historyOf(p, sid)) || read(t, filepath.Join(p.dst, "plans", slug+".md")) != "# mine\n" {
			t.Errorf("%s: the destination's versions or plan were touched", name)
		}
	}
}

// --fork: the artifacts go to the fork: its own task list (numbers as they
// are, ids in the texts as the fork has them), its own file versions (byte for
// byte) and a plan under a slug of its own, which its transcript names. The
// original's are not touched.
func TestForkTakesTheArtifactsAsItsOwn(t *testing.T) {
	p := fresh(t)
	put(t, p.s, `{"uuid":"`+fu1+`","sessionId":"`+sid+`","slug":"`+slug+`","planFilePath":"C:\\x\\plans\\`+slug+`.md"}`+"\n", t0)
	putFiles(t, filepath.Join(p.src, "tasks", sid), map[string]string{"1.json": taskJSON("1", "pending", "see "+fu1+" of "+sid)}, t0)
	put(t, filepath.Join(p.src, "file-history", sid, "0021876a64fde694@v1"), "a file that names "+fu1+" and "+sid+"\n", t0)
	put(t, filepath.Join(p.src, "plans", slug+".md"), "# plan of "+sid+"\n", t0)
	put(t, p.d, "x\n", t0)
	putFiles(t, tasksOf(p, sid), map[string]string{"7.json": taskJSON("7", "pending", "the original's")}, t0)
	put(t, filepath.Join(p.dst, "plans", slug+".md"), "# the original's plan\n", t0)

	r := copyOne(t, p, Options{Fork: true})
	fid := forkids.ForkID(sid)
	fslug := slug + "-" + fid[:8]
	if r.Status != Forked || r.Tasks != 4 || r.History != 3 || r.Plans != 1 {
		t.Fatalf("result = %+v", r)
	}
	if got, want := read(t, filepath.Join(tasksOf(p, fid), "1.json")), taskJSON("1", "pending", "see "+forkids.ForkUUID(fid, fu1)+" of "+fid); got != want {
		t.Errorf("task 1 of the fork = %q, want %q", got, want)
	}
	if got := read(t, filepath.Join(historyOf(p, fid), "0021876a64fde694@v1")); got != "a file that names "+fu1+" and "+sid+"\n" {
		t.Errorf("a file version was changed: %q", got)
	}
	if got := read(t, filepath.Join(p.dst, "plans", fslug+".md")); got != "# plan of "+fid+"\n" {
		t.Errorf("the fork's plan = %q", got)
	}
	tr := read(t, filepath.Join(p.dst, "projects", "a", fid+".jsonl"))
	if !strings.Contains(tr, `"slug":"`+fslug+`"`) || !strings.Contains(tr, `plans\\`+fslug+`.md"`) || strings.Contains(tr, `"`+slug+`"`) {
		t.Errorf("the fork's transcript names the slug %q: %s", slug, tr)
	}
	if got := names(t, tasksOf(p, sid)); got != "7.json" || read(t, filepath.Join(p.dst, "plans", slug+".md")) != "# the original's plan\n" || exists(historyOf(p, sid)) {
		t.Errorf("the original's artifacts were touched: %q", got)
	}
	// Again: nothing to do (the fork's slug is not given another suffix).
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Unchanged || r.Tasks+r.History+r.Plans != 0 {
		t.Errorf("rerun = %+v", r)
	}
	// The fork was continued: its transcript diverged, its artifacts are its own.
	put(t, filepath.Join(p.dst, "projects", "a", fid+".jsonl"), "continued\n", t0.Add(time.Hour))
	putFiles(t, tasksOf(p, fid), map[string]string{"1.json": taskJSON("1", "completed", "by the fork")}, t0.Add(time.Hour))
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Diverged || r.Tasks+r.History+r.Plans != 0 {
		t.Errorf("after the fork went on = %+v", r)
	}
	if got := read(t, filepath.Join(tasksOf(p, fid), "1.json")); got != taskJSON("1", "completed", "by the fork") {
		t.Errorf("the fork's own task was overwritten: %q", got)
	}
}

// --force replaces the destination: its tasks, versions and plan give way too,
// each kept aside when it is newer than the source's or not the source's.
func TestForceReplacesTheArtifactsAndKeepsTheOldOnes(t *testing.T) {
	p := fresh(t)
	newer := t0.Add(time.Hour)
	put(t, p.d, slugEntry(sid, slug, "other"), newer)
	putFiles(t, tasksOf(p, sid), map[string]string{"1.json": taskJSON("1", "pending", "newer here"), "8.json": taskJSON("8", "pending", "only here")}, newer)
	put(t, filepath.Join(historyOf(p, sid), "000878b2dc7b972d@v1"), "package z\n", newer)
	put(t, filepath.Join(p.dst, "plans", slug+".md"), "# newer here\n", newer)

	r := copyOne(t, p, Options{Force: true})
	if r.Status != Replaced || r.Tasks != 5 || r.History != 2 || r.Plans != 1 {
		t.Fatalf("result = %+v, want replaced: 3 tasks, the mark, task 8 taken out; 2 versions; the plan", r)
	}
	got := names(t, tasksOf(p, sid))
	for _, want := range []string{"1.json", "1.json.bak-", "8.json.bak-", "2.json", "3.json", ".highwatermark"} {
		if !strings.Contains(got, want) {
			t.Errorf("task files %q lack %s", got, want)
		}
	}
	if strings.Contains(" "+got+" ", " 8.json ") || read(t, filepath.Join(tasksOf(p, sid), "1.json")) != taskJSON("1", "completed", "done") {
		t.Errorf("task files = %q", got)
	}
	if h := names(t, historyOf(p, sid)); !strings.Contains(h, "@v1.bak-") || read(t, filepath.Join(historyOf(p, sid), "000878b2dc7b972d@v1")) != "package a\n" {
		t.Errorf("file versions = %q", h)
	}
	if pl := names(t, filepath.Join(p.dst, "plans")); !strings.Contains(pl, slug+".md.bak-") || read(t, filepath.Join(p.dst, "plans", slug+".md")) != "# the plan\n" {
		t.Errorf("plans = %q", pl)
	}
}

// Without --force a file version the destination holds differently is left.
func TestAFileVersionHeldDifferentlyIsLeftAlone(t *testing.T) {
	p := fresh(t)
	put(t, p.d, slugEntry(sid, slug, "one"), t0)
	put(t, filepath.Join(historyOf(p, sid), "000878b2dc7b972d@v1"), "package z\n", t0.Add(time.Hour))
	if r := copyOne(t, p, Options{}); r.Status != Unchanged || r.History != 1 {
		t.Fatalf("result = %+v, want only the missing version written", r)
	}
	if got := read(t, filepath.Join(historyOf(p, sid), "000878b2dc7b972d@v1")); got != "package z\n" {
		t.Errorf("the destination's version was overwritten: %q", got)
	}
}

// A session without artifacts at the source leaves the destination's alone.
func TestNoArtifactsAtTheSourceChangeNothing(t *testing.T) {
	p := fresh(t)
	for _, d := range []string{"tasks", "file-history", "plans"} {
		if err := os.RemoveAll(filepath.Join(p.src, d)); err != nil {
			t.Fatal(err)
		}
	}
	put(t, p.d, slugEntry(sid, slug, "one"), t0)
	putFiles(t, tasksOf(p, sid), map[string]string{"1.json": taskJSON("1", "pending", "kept")}, t0)
	put(t, filepath.Join(p.dst, "plans", slug+".md"), "# kept\n", t0)
	if r := copyOne(t, p, Options{}); r.Status != Unchanged || r.Tasks+r.History+r.Plans != 0 {
		t.Fatalf("result = %+v", r)
	}
	if got := names(t, tasksOf(p, sid)); got != "1.json" || read(t, filepath.Join(p.dst, "plans", slug+".md")) != "# kept\n" {
		t.Errorf("task files = %q", got)
	}
}

func TestTranscriptSlugs(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one.jsonl")
	put(t, one, entry(sid, "no slug yet")+slugEntry(sid, slug, "x")+slugEntry(sid, slug, "y"), t0)
	if got := strings.Join(transcriptSlugs(one), " "); got != slug {
		t.Errorf("slugs = %q", got)
	}
	// A slug named only at the end of a long transcript is found too.
	long := filepath.Join(dir, "long.jsonl")
	put(t, long, strings.Repeat(entry(sid, strings.Repeat("x", 1000)), 9000)+slugEntry(sid, "late-slug-here", "z"), t0)
	if got := strings.Join(transcriptSlugs(long), " "); got != "late-slug-here" {
		t.Errorf("slugs of a long transcript = %q", got)
	}
	if got := transcriptSlugs(filepath.Join(dir, "missing.jsonl")); got != nil {
		t.Errorf("missing file: %v", got)
	}
}

// A slug inside a longer word, or the fork's own slug, is not replaced.
func TestReplaceWord(t *testing.T) {
	for in, want := range map[string]string{
		`"slug":"` + slug + `"`:              `"slug":"` + slug + `-12345678"`,
		`plans\\` + slug + `.md and ` + slug: `plans\\` + slug + `-12345678.md and ` + slug + `-12345678`,
		slug + "-12345678":                   slug + "-12345678",
		"very-" + slug:                       "very-" + slug,
		slug + "_x":                          slug + "_x",
		"nothing here":                       "nothing here",
	} {
		if got := string(replaceWord([]byte(in), slug, slug+"-12345678")); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
