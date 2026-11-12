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
	put(t, filepath.Join(dst, "projects", worktree, forkID(sid2)+".jsonl"), "an earlier fork\n", t0)
	res, err := Copy(src, dst, 1, Options{Project: home, Within: dir, Fork: true})
	if err != nil || len(res) != 1 {
		t.Fatalf("results = %+v, %v", res, err)
	}
	r := res[0]
	if r.Status != Elsewhere || r.Fork != forkID(sid2) || r.Other != worktree || !r.Status.Skipped() {
		t.Errorf("result = %+v, want the fork elsewhere in %s", r, worktree)
	}
	if exists(filepath.Join(dst, "projects", home, forkID(sid2)+".jsonl")) {
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
	fid := forkID(sid2)
	want := cwdEntry(fid, dir, "begun here") +
		`{"sessionId":"` + fid + `","relocatedCwd":` + string(home2) + `,"type":"relocated"}` + "\n" +
		strings.Replace(quote, "%s", fid, 1)
	if got := read(t, filepath.Join(dst, "projects", home, fid+".jsonl")); got != want {
		t.Errorf("fork =\n%s\nwant\n%s", got, want)
	}
}
