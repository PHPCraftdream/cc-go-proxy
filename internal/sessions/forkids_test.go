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
	fname  = "beafulydc" // a task output named by letters only, mentioned nowhere
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
	put(t, filepath.Join(side, "tool-results", fname+".txt"), "unnamed output", t0)
	put(t, filepath.Join(side, "notes.md"), "a name without a digit outside tool-results", t0)
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
	if got := read(t, filepath.Join(side, "tool-results", m.mapShort(fname)+".txt")); got != "unnamed output" {
		t.Errorf("letters-only task output = %q", got)
	}
	if got := read(t, filepath.Join(side, "notes.md")); got != "a name without a digit outside tool-results" {
		t.Errorf("notes.md = %q", got)
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
	if all := strings.Join(names, " "); len(names) != 8 || strings.Contains(all, fagent) || strings.Contains(all, ftask) || strings.Contains(all, fname) {
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

// Only the session's own uuids (a field's value somewhere in it) are
// replaced; one that is only mentioned in text points elsewhere and stays.
// A word in an id field is not a short id; a task id of letters is.
func TestForkIDsKnowWhatIsTheSessions(t *testing.T) {
	const external = "dddddddd-0000-4000-8000-000000000009"
	dir := t.TempDir()
	tr := filepath.Join(dir, "s.jsonl")
	put(t, tr, `{"uuid":"`+fu1+`","sessionId":"`+sid+`","task_id":"deployment","backgroundTaskId":"`+fword+`"}`+"\n"+
		`{"message":{"content":"see `+fu1+`, run `+external+`, deployment done, `+fword+` too"}}`+"\n", t0)
	m, err := newForkIDs(sid, forkID(sid), tr, filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(m.line([]byte(`see ` + fu1 + `, run ` + external + `, deployment done, ` + fword + ` too`)))
	want := `see ` + forkUUID(m.fid, fu1) + `, run ` + external + `, deployment done, ` + m.mapShort(fword) + ` too`
	if got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	if m.short["deployment"] || !m.short[fword] || m.uuids[external] || !m.uuids[fu1] {
		t.Errorf("short %v, uuids %v", m.short, m.uuids)
	}
	for id, want := range map[string]bool{"a004a671": true, "b01bxagzb": true, "bxagzbcde": true, "123456": false, "abcdefgh": false, "benchmarks": false} {
		m := &forkIDs{short: map[string]bool{}}
		if m.add(id); m.short[id] != want {
			t.Errorf("add(%q) kept %v, want %v", id, m.short[id], want)
		}
	}
}

// The source grows with new messages, tool calls and tasks between runs: the
// fork is extended, its earlier bytes stay as they were.
func TestAForkGrowsWithItsSource(t *testing.T) {
	p := forkSource(t)
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Forked {
		t.Fatalf("first = %+v", r)
	}
	fork := filepath.Join(p.dst, "projects", "a", forkID(sid)+".jsonl")
	before := read(t, fork)
	const (
		u3    = "eeeeeeee-0000-4000-8000-000000000004"
		tool2 = "toolu_01ZzZzZzZzZzZzZzZzZzZzZz"
		task2 = "b9xk3m2q1"
	)
	more := `{"parentUuid":"` + fu2 + `","uuid":"` + u3 + `","sessionId":"` + sid + `","message":{"content":[{"type":"tool_use","id":"` + tool2 + `"}]},"toolUseResult":{"backgroundTaskId":"` + task2 + `"}}` + "\n"
	put(t, p.s, read(t, p.s)+more, t0.Add(3*time.Hour))
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Forked {
		t.Fatalf("after growth = %+v, want the fork extended", r)
	}
	after := read(t, fork)
	if !strings.HasPrefix(after, before) || len(after) == len(before) {
		t.Fatal("the fork was not extended as it was")
	}
	tail := after[len(before):]
	for _, old := range []string{sid, fu2, u3, tool2, task2} {
		if strings.Contains(tail, old) {
			t.Errorf("the new line still holds %s: %s", old, tail)
		}
	}
	m := &forkIDs{fid: forkID(sid)}
	if !strings.Contains(tail, `"parentUuid":"`+forkUUID(m.fid, strings.ToLower(fu2))+`"`) {
		t.Errorf("the new line does not link to the fork's message: %s", tail)
	}
}

// A session run through ccgp on Codex holds the Responses API's ids: tool
// calls call_<32 hex>, replies resp_<50 hex>. They are the session's too.
func TestForkReplacesCodexIDs(t *testing.T) {
	const (
		call = "call_f2bafbdac980443f9b296d95129b00b6"
		resp = "resp_0679676010f4e302016af510224ba087d1bd075c135a7fb41d"
	)
	m := &forkIDs{fid: forkID(sid)}
	in := `{"message":{"id":"` + resp + `","content":[{"type":"tool_use","id":"` + call + `"}]}}` + "\n" +
		`{"content":[{"type":"tool_result","tool_use_id":"` + call + `"}],"text":"<tool-use-id>` + call + `</tool-use-id>\n` + call + ` done"}` + "\n"
	got := string(m.line([]byte(in)))
	ncall, nresp := string(m.line([]byte(call))), string(m.line([]byte(resp)))
	if strings.Contains(got, call) || strings.Contains(got, resp) || strings.Count(got, ncall) != 4 || strings.Count(got, nresp) != 1 {
		t.Errorf("line = %s\n(call -> %s, resp -> %s)", got, ncall, nresp)
	}
	if !regexp.MustCompile(`^call_[0-9a-f]{32}$`).MatchString(ncall) || !regexp.MustCompile(`^resp_[0-9a-f]{50}$`).MatchString(nresp) || ncall == call {
		t.Errorf("forms: %s %s", ncall, nresp)
	}
	// Names in code, ids inside longer words, and over-long runs stay.
	for _, keep := range []string{
		"call_backgroundHandlersNow", // no digit: a name
		"recall_f2bafbdac980443f9b296d95129b00b6",
		"call_" + strings.Repeat("a1", 33), // 66 characters: no id of the API
	} {
		if got := string(m.line([]byte(keep))); got != keep {
			t.Errorf("%s -> %s", keep, got)
		}
	}
}

// Claude Code names a background task in text long before the field that
// holds its id: the id is the session's from the first mention, and a fork
// synced in between is extended, not diverged, once the field comes.
func TestATaskIDNamedInTextFirstIsKnownFromThere(t *testing.T) {
	const (
		task  = "bo1nq9t9c"
		agent = "abedd0edd7fd1834e"
	)
	p := both(t)
	put(t, p.d, "x\n", t0)
	first := `{"type":"user","sessionId":"` + sid + `","message":{"content":"Command running in background with ID: ` + task +
		`. Output is being written to: C:\Temp\` + sid + `\tasks\` + task + `.output; agentId: ` + agent + ` (use SendMessage)"}}` + "\n"
	put(t, p.s, first, t0.Add(2*time.Hour))
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Forked {
		t.Fatalf("first = %+v", r)
	}
	fork := filepath.Join(p.dst, "projects", "a", forkID(sid)+".jsonl")
	before := read(t, fork)
	if strings.Contains(before, task) || strings.Contains(before, agent) {
		t.Fatalf("ids named only in text were kept: %s", before)
	}
	more := `{"type":"user","sessionId":"` + sid + `","message":{"content":"<task-notification>\n<task-id>` + task +
		`</task-id>"},"toolUseResult":{"backgroundTaskId":"` + task + `","agentId":"` + agent + `"}}` + "\n"
	put(t, p.s, first+more, t0.Add(3*time.Hour))
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Forked {
		t.Fatalf("after the field came = %+v, want the fork extended", r)
	}
	after := read(t, fork)
	if !strings.HasPrefix(after, before) || strings.Contains(after, task) || strings.Contains(after, agent) {
		t.Errorf("fork before:\n%s\nafter:\n%s", before, after)
	}
}
