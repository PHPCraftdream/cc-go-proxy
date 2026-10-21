package sessions

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	var last, calls, total int64
	opt := Options{
		ProgressEvery: time.Nanosecond, // report on every chunk
		OnStart: func(i, n int, s Session) {
			starts = append(starts, s.ID+":"+string(rune('0'+i))+"/"+string(rune('0'+n)))
		},
		OnProgress: func(_ Session, done, tot int64) {
			if done < last && done != 0 && last != total { // restarts with the next session
				t.Errorf("progress went back: %d after %d", done, last)
			}
			last, total = done, tot
			calls++
		},
	}
	if _, err := Copy(src, dst, 2, opt); err != nil {
		t.Fatal(err)
	}
	if strings.Join(starts, " ") != "2:1/2 1:2/2" {
		t.Errorf("starts = %v", starts)
	}
	if calls == 0 || last != total || total != int64(len(big)) {
		t.Errorf("progress calls=%d last=%d total=%d, want the final report to reach %d", calls, last, total, len(big))
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
	opt := Options{StripThinking: true, ProgressEvery: time.Nanosecond, OnProgress: func(_ Session, done, tot int64) { last, total = done, tot }}
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
