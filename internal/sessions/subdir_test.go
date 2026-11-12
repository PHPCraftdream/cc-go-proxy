package sessions

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
// one Claude Code moved into the folder of dir's worktree.
func moved(t *testing.T) (src, dst, dir string) {
	t.Helper()
	src, dst, dir = t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "proj")
	put(t, filepath.Join(src, "projects", home, sid+".jsonl"), cwdEntry(sid, dir, "old"), t0)
	put(t, filepath.Join(src, "projects", worktree, sid2+".jsonl"), cwdEntry(sid2, filepath.Join(dir, "worktrees", "x"), "new"), t0.Add(time.Hour))
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
	// A sibling directory whose folder name starts the same, and a folder
	// whose transcript names no directory.
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
	put(t, filepath.Join(src, "projects", worktree, sid+".jsonl"), cwdEntry(sid, filepath.Join(dir, "worktrees", "x"), "old")+entry(sid, "more"), t0.Add(2*time.Hour))
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
	if !exists(filepath.Join(dst, "projects", home, forkID(sid2)+".jsonl")) {
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
	fid := forkID(sid2)
	got := read(t, filepath.Join(dst, "projects", home, fid+".jsonl"))
	if want := cwdEntry(fid, dir, "begun here") + relocatedEntry(fid, dir); got != want {
		t.Errorf("fork =\n%s\nwant\n%s", got, want)
	}
	if got := read(t, filepath.Join(dst, "projects", home, fid, "subagents", "agent-a1.jsonl")); got != entry(fid, "sub") {
		t.Errorf("subagent transcript = %q", got)
	}
}
