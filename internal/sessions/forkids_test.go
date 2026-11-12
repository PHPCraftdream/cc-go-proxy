package sessions

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	fu1    = "aaaaaaaa-0000-4000-8000-000000000001"
	fu2    = "AAAAAAAA-0000-4000-8000-000000000002"
	fpr    = "bbbbbbbb-0000-4000-8000-000000000003"
	ftool  = "toolu_01AbCdEfGhIjKlMnOpQrStUv"
	fmsg   = "msg_01ZyXwVuTsRqPoNmLkJiHgFe"
	freq   = "req_011CaBcDeFgHiJkLmNoPqRsT"
	fagent = "a004a671eb6e46d71"
	ftask  = "b01bxagzb"
	fword  = "bxagzbcde" // a task id without a digit
)

// session writes a transcript that names every kind of id, and a folder with
// the files those ids name.
func forkSource(t *testing.T) pair {
	t.Helper()
	p := both(t)
	lines := []string{
		`{"parentUuid":null,"uuid":"` + fu1 + `","promptId":"` + fpr + `","sessionId":"` + sid + `","type":"user","message":{"content":"see ` + fu1 + ` and the text"}}`,
		`{"parentUuid":"` + fu1 + `","uuid":"` + fu2 + `","sessionId":"` + sid + `","requestId":"` + freq + `","message":{"id":"` + fmsg + `","content":[{"type":"tool_use","id":"` + ftool + `","name":"Agent"}]}}`,
		`{"type":"user","toolUseResult":{"agentId":"` + fagent + `","backgroundTaskId":"` + ftask + `","taskId":"7"},"message":{"content":[{"type":"tool_result","tool_use_id":"` + ftool + `","content":"agentId: ` + fagent + `\nsaved to tool-results\\` + ftask + `.txt\n` + ftask + ` done; task 7"}]}}`,
		`{"type":"progress","backgroundTaskId":"` + fword + `","toolUseID":"` + ftool + `","text":"` + fword + ` finished; toolu_1 and msg_1 are not ids"}`,
	}
	put(t, p.s, strings.Join(lines, "\n")+"\n", t0.Add(2*time.Hour))
	put(t, p.d, "x\n", t0) // diverged at once
	side := strings.TrimSuffix(p.s, ".jsonl")
	put(t, filepath.Join(side, "subagents", "agent-"+fagent+".jsonl"), `{"sessionId":"`+sid+`","agentId":"`+fagent+`","uuid":"`+fu1+`"}`+"\n", t0)
	put(t, filepath.Join(side, "subagents", "agent-"+fagent+".meta.json"), `{"agentId":"`+fagent+`","toolUseId":"`+ftool+`"}`, t0)
	put(t, filepath.Join(side, "tool-results", ftask+".txt"), "output of "+ftask+" for "+ftool, t0)
	put(t, filepath.Join(side, "tool-results", ftool+".txt"), "result", t0)
	put(t, filepath.Join(side, "tool-results", "diff_sdk.txt"), "plain name", t0)
	put(t, filepath.Join(side, "image.png"), "\x89PNG "+ftool, t0)
	return p
}

func TestForkGivesEveryIdAndFileNameItsOwn(t *testing.T) {
	p := forkSource(t)
	r := copyOne(t, p, Options{Fork: true})
	if r.Status != Forked {
		t.Fatalf("result = %+v", r)
	}
	fid := forkID(sid)
	m := &forkIDs{fid: fid}
	nu1, nu2, npr := forkUUID(fid, fu1), forkUUID(fid, strings.ToLower(fu2)), forkUUID(fid, fpr)
	ntool := string(m.line([]byte(ftool)))
	nagent, ntask, nword := m.mapShort(fagent), m.mapShort(ftask), m.mapShort(fword)

	for old, nu := range map[string]string{ftool: ntool, fagent: nagent, ftask: ntask, fword: nword} {
		if nu == old || len(nu) != len(old) || nu[:1] != old[:1] {
			t.Errorf("%s -> %s: want another id of its form", old, nu)
		}
	}
	if !regexp.MustCompile(`^toolu_01[0-9A-Za-z]{22}$`).MatchString(ntool) || !regexp.MustCompile(`^a[0-9a-f]{16}$`).MatchString(nagent) {
		t.Errorf("forms: %s %s", ntool, nagent)
	}

	dir := filepath.Join(p.dst, "projects", "a")
	got := read(t, filepath.Join(dir, fid+".jsonl"))
	for _, old := range []string{sid, fu1, strings.ToLower(fu2), fu2, fpr, ftool, fmsg, freq, fagent, ftask, fword} {
		if strings.Contains(got, old) {
			t.Errorf("the fork still holds %s:\n%s", old, got)
		}
	}
	for _, want := range []string{
		`"uuid":"` + nu1 + `","promptId":"` + npr + `","sessionId":"` + fid + `"`, "see " + nu1 + " and the text",
		`"parentUuid":"` + nu1 + `","uuid":"` + nu2 + `"`,
		`"tool_use_id":"` + ntool + `"`, `"agentId":"` + nagent + `"`, `"backgroundTaskId":"` + ntask + `","taskId":"7"`,
		`agentId: ` + nagent + `\nsaved to tool-results\\` + ntask + `.txt\n` + ntask + ` done; task 7`,
		`"backgroundTaskId":"` + nword + `"`, nword + ` finished; toolu_1 and msg_1 are not ids`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the fork lacks %s:\n%s", want, got)
		}
	}

	side := filepath.Join(dir, fid)
	if got := read(t, filepath.Join(side, "subagents", "agent-"+nagent+".jsonl")); got != `{"sessionId":"`+fid+`","agentId":"`+nagent+`","uuid":"`+nu1+`"}`+"\n" {
		t.Errorf("subagent transcript = %q", got)
	}
	if got := read(t, filepath.Join(side, "subagents", "agent-"+nagent+".meta.json")); got != `{"agentId":"`+nagent+`","toolUseId":"`+ntool+`"}` {
		t.Errorf("subagent meta = %q", got)
	}
	if got := read(t, filepath.Join(side, "tool-results", ntask+".txt")); got != "output of "+ntask+" for "+ntool {
		t.Errorf("task output = %q", got)
	}
	if got := read(t, filepath.Join(side, "tool-results", ntool+".txt")); got != "result" {
		t.Errorf("tool result = %q", got)
	}
	if got := read(t, filepath.Join(side, "tool-results", "diff_sdk.txt")); got != "plain name" {
		t.Errorf("plain file = %q", got)
	}
	if got := read(t, filepath.Join(side, "image.png")); got != "\x89PNG "+ftool {
		t.Errorf("a binary file was changed: %q", got)
	}
	var names []string
	_ = filepath.WalkDir(side, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, d.Name())
		}
		return nil
	})
	if len(names) != 6 || strings.Contains(strings.Join(names, " "), fagent) || strings.Contains(strings.Join(names, " "), ftask) {
		t.Errorf("files of the fork: %v", names)
	}

	// Again: the same bytes, nothing to write.
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Unchanged || r.Files != 0 {
		t.Errorf("rerun = %+v", r)
	}
}

// Different forks map an id differently; the session id stays the fork's.
func TestForkIDsDependOnTheFork(t *testing.T) {
	a, b := &forkIDs{fid: forkID("one")}, &forkIDs{fid: forkID("two")}
	if string(a.line([]byte(ftool))) == string(b.line([]byte(ftool))) || a.mapShort(fagent) == b.mapShort(fagent) {
		t.Error("two forks got the same ids")
	}
	if got := string(a.line([]byte(`"sessionId":"` + a.fid + `"`))); got != `"sessionId":"`+a.fid+`"` {
		t.Errorf("the fork id was mapped again: %s", got)
	}
	// An id glued to a longer run is not that id.
	long := ftool + "X"
	if got := string(a.line([]byte(long))); got != long {
		t.Errorf("a longer token was mapped: %s", got)
	}
}
