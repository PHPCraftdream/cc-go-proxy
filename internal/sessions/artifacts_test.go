package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func tasksOf(p pair, id string) string   { return filepath.Join(p.dst, "tasks", id) }
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
	fid := forkID(sid)
	fslug := slug + "-" + fid[:8]
	if r.Status != Forked || r.Tasks != 4 || r.History != 3 || r.Plans != 1 {
		t.Fatalf("result = %+v", r)
	}
	if got, want := read(t, filepath.Join(tasksOf(p, fid), "1.json")), taskJSON("1", "pending", "see "+forkUUID(fid, fu1)+" of "+fid); got != want {
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
