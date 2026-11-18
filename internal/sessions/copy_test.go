package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions/forkids"
	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions/jsonl"
)

func failedPair(t *testing.T, src string) pair {
	t.Helper()
	p := pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "a", sid+".jsonl")
	p.d = filepath.Join(p.dst, "projects", "a", sid+".jsonl")
	put(t, p.s, src, t0)
	return p
}

func TestCopyLeavesOutTheFailedEnd(t *testing.T) {
	p := failedPair(t, kept+failedEnd)
	r := copyOne(t, p, Options{})
	if r.Status != Copied || r.Failed != 3 || read(t, p.d) != kept {
		t.Fatalf("got %v, %d failed, %q", r.Status, r.Failed, read(t, p.d))
	}

	// Again: nothing to do.
	if r = copyOne(t, p, Options{}); r.Status != Unchanged || read(t, p.d) != kept {
		t.Fatalf("rerun: %v, %q", r.Status, read(t, p.d))
	}

	// The session went on: the errors are no longer its end and come along.
	put(t, p.s, kept+failedEnd+lPrompt+lReply, t0.Add(time.Hour))
	if r = copyOne(t, p, Options{}); r.Status != Updated || r.Failed != 0 || read(t, p.d) != kept+failedEnd+lPrompt+lReply {
		t.Fatalf("after more: %v, %d failed, %q", r.Status, r.Failed, read(t, p.d))
	}
}

func TestCopyKeepsAnEarlierCopyThatHasTheFailedEnd(t *testing.T) {
	for _, have := range []string{
		kept + failedEnd,                                 // all of it
		kept + lRequest + lRequest + lError,              // a part
		kept + failedEnd[:len(failedEnd)-len(lDuration)], // all but the last line
	} {
		p := failedPair(t, kept+failedEnd)
		put(t, p.d, have, t0)
		if r := copyOne(t, p, Options{}); r.Status != Unchanged || read(t, p.d) != have {
			t.Errorf("have %d bytes: %v, %d bytes now", len(have), r.Status, len(read(t, p.d)))
		}
	}
}

func TestCopyDestinationThatDivergesInTheFailedEnd(t *testing.T) {
	p := failedPair(t, kept+failedEnd)
	have := kept + lPrompt
	put(t, p.d, have, t0)
	if r := copyOne(t, p, Options{}); r.Status != Diverged || read(t, p.d) != have {
		t.Errorf("got %v, %q", r.Status, read(t, p.d))
	}
	r := copyOne(t, p, Options{Force: true})
	if r.Status != Replaced || r.Failed != 3 || read(t, p.d) != kept || read(t, r.Backup) != have {
		t.Errorf("force: %v, %d failed, %q", r.Status, r.Failed, read(t, p.d))
	}
}

func TestCopyLeavesOutTheFailedEndWhileStrippingThinking(t *testing.T) {
	thinking := `{"type":"assistant","uuid":"th","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"s"}]}}` + "\n"
	reply := `{"type":"assistant","uuid":"a1","parentUuid":"th","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}` + "\n"
	// The error hangs off the dropped thinking line too.
	errLine := strings.Replace(lError, `"parentUuid":"at1"`, `"parentUuid":"th"`, 1)
	src := lPrompt + thinking + reply + errLine + lDuration
	p := failedPair(t, src)
	r := copyOne(t, p, Options{StripThinking: true})
	want, _, err := jsonl.StripThinking([]byte(lPrompt + thinking + reply))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != Copied || r.Failed != 1 || r.Removed != 1 || read(t, p.d) != string(want) {
		t.Fatalf("got %v, %d failed, %d removed, %q", r.Status, r.Failed, r.Removed, read(t, p.d))
	}
	// A copy made before, with its end, stays as it is.
	full, _, err := jsonl.StripThinking([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	put(t, p.d, string(full), t0)
	if r = copyOne(t, p, Options{StripThinking: true}); r.Status != Unchanged || read(t, p.d) != string(full) {
		t.Errorf("earlier copy: %v, %q", r.Status, read(t, p.d))
	}
}

func TestForkLeavesOutTheFailedEnd(t *testing.T) {
	p := failedPair(t, entry(sid, "one")+kept+failedEnd)
	put(t, p.d, entry(sid, "one")+entry(sid, "from-destination"), t0)
	r := copyOne(t, p, Options{Fork: true})
	fork := filepath.Join(p.dst, "projects", "a", r.Fork+".jsonl")
	got := read(t, fork)
	if r.Status != Forked || r.Failed != 3 || strings.Contains(got, "API Error") || !strings.HasSuffix(got, lState) {
		t.Fatalf("got %v, %d failed, %q", r.Status, r.Failed, got)
	}
	if strings.Count(got, "\n") != 1+strings.Count(kept, "\n") {
		t.Errorf("fork has %d lines", strings.Count(got, "\n"))
	}
}

func TestSubagentTranscriptsKeepTheirEnd(t *testing.T) {
	p := failedPair(t, kept)
	sub := filepath.Join(strings.TrimSuffix(p.s, ".jsonl"), "subagents", "agent-a1.jsonl")
	put(t, sub, kept+failedEnd, t0)
	copyOne(t, p, Options{})
	if got := read(t, filepath.Join(strings.TrimSuffix(p.d, ".jsonl"), "subagents", "agent-a1.jsonl")); got != kept+failedEnd {
		t.Errorf("subagent transcript: %q", got)
	}
}

func TestPlacerComparesPastTheLimit(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(dst, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		limit  int64
		stream string
		want   Status
		file   string
	}{
		{3, "abcdefgh", Unchanged, "abcdef"},
		{8, "abcdefghij", Updated, "abcdefgh"},
		{3, "abcxyz", 0, "abcdef"}, // diverged
		{3, "abcd", Ahead, "abcdef"},
	} {
		p, err := newPlacer(dst, t0)
		if err != nil {
			t.Fatal(err)
		}
		p.compareFrom(c.limit)
		_, werr := p.Write([]byte(c.stream))
		if c.want == 0 {
			if !errors.Is(werr, errDiverged) {
				t.Errorf("%q: %v, want diverged", c.stream, werr)
			}
		} else if werr != nil {
			t.Fatal(werr)
		} else if st, err := p.finish(); err != nil || st != c.want {
			t.Errorf("%q: %v, %v; want %v", c.stream, st, err, c.want)
		}
		p.close()
		if got, _ := os.ReadFile(dst); !bytes.Equal(got, []byte(c.file)) {
			t.Errorf("%q: file %q, want %q", c.stream, got, c.file)
		}
		_ = os.WriteFile(dst, []byte("abcdef"), 0o600)
	}
}

func TestSwapReaderMarksTheOutputAtTheCut(t *testing.T) {
	double := func(b []byte) ([]byte, error) { return append(append([]byte{}, b...), b...), nil }
	for _, size := range []int{1, 3, 4096} {
		got := int64(-1)
		s := testSwapReader(strings.NewReader("a\nbb\nc\n"), double)
		s.cut, s.mark = 5, func(n int64) { got = n }
		var out strings.Builder
		if _, err := io.CopyBuffer(struct{ io.Writer }{&out}, struct{ io.Reader }{s}, make([]byte, size)); err != nil {
			t.Fatal(err)
		}
		if got != 10 || out.String() != "a\na\nbb\nbb\nc\nc\n" {
			t.Errorf("buffer %d: mark %d, %q", size, got, out.String())
		}
	}
}

// Many files of a session's folder are copied at once, each to its place.
func TestCopyManySidecarFiles(t *testing.T) {
	p := pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "a", sid+".jsonl")
	p.d = filepath.Join(p.dst, "projects", "a", sid+".jsonl")
	put(t, p.s, entry(sid, "one"), t0)
	side := strings.TrimSuffix(p.s, ".jsonl")
	for i := range 37 {
		put(t, filepath.Join(side, "tool-results", fmt.Sprintf("t%02d.txt", i)), fmt.Sprintf("result %d of %s", i, sid), t0)
	}
	r := copyOne(t, p, Options{})
	if r.Files != 37 {
		t.Fatalf("files = %d", r.Files)
	}
	for i := range 37 {
		got := read(t, filepath.Join(strings.TrimSuffix(p.d, ".jsonl"), "tool-results", fmt.Sprintf("t%02d.txt", i)))
		if got != fmt.Sprintf("result %d of %s", i, sid) {
			t.Errorf("t%02d: %q", i, got)
		}
	}
	if r = copyOne(t, p, Options{}); r.Status != Unchanged || r.Files != 0 {
		t.Errorf("rerun: %v, %d files", r.Status, r.Files)
	}
}

// A sidecar file that cannot be written fails the session; the others may
// have been written.
func TestCopySidecarFailure(t *testing.T) {
	p := pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "a", sid+".jsonl")
	put(t, p.s, entry(sid, "one"), t0)
	side := strings.TrimSuffix(p.s, ".jsonl")
	for i := range 9 {
		put(t, filepath.Join(side, "tool-results", fmt.Sprintf("t%d.txt", i)), "x", t0)
	}
	// A directory where a file must go.
	if err := os.MkdirAll(filepath.Join(p.dst, "projects", "a", sid, "tool-results", "t4.txt", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := Copy(p.src, p.dst, 1, Options{})
	if err != nil || len(res) != 1 || res[0].Err == nil {
		t.Fatalf("Copy = %+v, %v; want the session failed", res, err)
	}
}

// A copy large enough to strip on several goroutines writes what the one-pass
// strip writes.
func TestParallelStripMatchesTheOnePassStrip(t *testing.T) {
	r := rand.New(rand.NewSource(12))
	var b strings.Builder
	for b.Len() < 3*jsonl.ParallelFrom {
		b.WriteString(thinkingTranscript(r, 200))
		b.WriteString(`{"pad":"` + strings.Repeat("p", r.Intn(30000)) + `"}` + "\n")
	}
	in := []byte(b.String())
	want, wantN := refStrip(in)
	p := pair{src: t.TempDir(), dst: t.TempDir()}
	p.s = filepath.Join(p.src, "projects", "a", sid+".jsonl")
	p.d = filepath.Join(p.dst, "projects", "a", sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p.s), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.s, in, 0o600); err != nil {
		t.Fatal(err)
	}
	res := copyOne(t, p, Options{StripThinking: true})
	if got := read(t, p.d); got != string(want) || res.Removed != wantN {
		t.Errorf("parallel strip: %d bytes, %d removed; want %d, %d", len(got), res.Removed, len(want), wantN)
	}
}

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
	fid := forkids.ForkID(sid)
	m := &forkids.IDs{FID: fid}
	nu1, nu2, npr := forkids.ForkUUID(fid, fu1), forkids.ForkUUID(fid, strings.ToLower(fu2)), forkids.ForkUUID(fid, fpr)
	ntool := string(m.Line([]byte(ftool)))
	nagent, ntask, nword := m.MapShort(fagent), m.MapShort(ftask), m.MapShort(fword)

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
	if got := read(t, filepath.Join(side, "tool-results", m.MapShort(fname)+".txt")); got != "unnamed output" {
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

// The source grows with new messages, tool calls and tasks between runs: the
// fork is extended, its earlier bytes stay as they were.
func TestAForkGrowsWithItsSource(t *testing.T) {
	p := forkSource(t)
	if r := copyOne(t, p, Options{Fork: true}); r.Status != Forked {
		t.Fatalf("first = %+v", r)
	}
	fork := filepath.Join(p.dst, "projects", "a", forkids.ForkID(sid)+".jsonl")
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
	m := &forkids.IDs{FID: forkids.ForkID(sid)}
	if !strings.Contains(tail, `"parentUuid":"`+forkids.ForkUUID(m.FID, strings.ToLower(fu2))+`"`) {
		t.Errorf("the new line does not link to the fork's message: %s", tail)
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
	fork := filepath.Join(p.dst, "projects", "a", forkids.ForkID(sid)+".jsonl")
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

// replaceWord replaces old where it stands alone: not inside a longer run of
// letters, digits, '-' and '_' (another slug, or the fork's own, which begins
// with it).
func replaceWord(b []byte, old, as string) []byte {
	if out, changed := forkids.ReplaceWordTo(nil, b, old, as); changed {
		return out
	}
	return b
}

const transcript = `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"ccgp1:xx"}]}}
{"type":"assistant","uuid":"a2","parentUuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"a < b & c"}]}}
{"type":"assistant","uuid":"a3","parentUuid":"a2","message":{"role":"assistant","content":[{"type":"redacted_thinking","data":"zz"},{"type":"tool_use","id":"t1","name":"X","input":{}}]}}
{"type":"user","uuid":"u2","parentUuid":"a3","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"last-prompt","leafUuid":"a1"}
not json
`

// kept is a conversation whose last real line is a tool result's hook
// attachment, followed by the state Claude Code writes before the next request.
const kept = lPrompt + lState + lReply + lResult + lAttach + lState

// Lines as Claude Code writes them, cut down to what FailedTail looks at.
const (
	lPrompt    = `{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}` + "\n"
	lReply     = `{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}` + "\n"
	lResult    = `{"type":"user","uuid":"u2","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}` + "\n"
	lAttach    = `{"type":"attachment","uuid":"at1","parentUuid":"u2","attachment":{"type":"hook_success"}}` + "\n"
	lState     = `{"type":"last-prompt","lastPrompt":"hi","leafUuid":"u1"}` + "\n" + `{"type":"ai-title","aiTitle":"Greeting"}` + "\n"
	lRequest   = `{"type":"api-request","id":"r1","base":"r0"}` + "\n"
	lError     = `{"type":"assistant","uuid":"e1","parentUuid":"at1","isApiErrorMessage":true,"apiErrorStatus":502,"message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"API Error: 502 codex: usage_limit_reached"}]}}` + "\n"
	lDuration  = `{"type":"system","subtype":"turn_duration","uuid":"d1","parentUuid":"e1"}` + "\n"
	lInterrupt = `{"type":"user","uuid":"i1","parentUuid":"d1","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}` + "\n"
	lTick      = `{"type":"user","uuid":"t1","parentUuid":"s1","isMeta":true,"turnOrigin":"scheduled","message":{"role":"user","content":"# tick"}}` + "\n"
)

// failedEnd is what came after kept: the request that failed, then two
// scheduled prompts that failed too.
var failedEnd = lRequest + lRequest + lError + lDuration + scheduledTurn() + scheduledTurn()

// scheduledTurn is a turn of a scheduled prompt that got an API error.
func scheduledTurn() string {
	return `{"type":"queue-operation","operation":"enqueue","content":"# tick"}` + "\n" +
		`{"type":"queue-operation","operation":"dequeue"}` + "\n" +
		`{"type":"system","subtype":"scheduled_task_fire","uuid":"s1","parentUuid":"d1"}` + "\n" +
		`{"type":"api-request-blob","hash":"h"}` + "\n" + lRequest +
		lTick + lState + lRequest + lRequest + lError + lDuration
}

// thinkingTranscript is a random transcript: lines of thinking only (dropped),
// thinking with text, chains of dropped lines, links of every kind to them,
// text that only mentions thinking, lines that are not JSON, CRLF ends.
func thinkingTranscript(r *rand.Rand, n int) string {
	var b strings.Builder
	parent := "null"
	for i := range n {
		id := fmt.Sprintf("u%d", i)
		p := parent
		if r.Intn(4) == 0 && i > 2 {
			p = fmt.Sprintf(`"u%d"`, r.Intn(i))
		}
		eol := "\n"
		if r.Intn(20) == 0 {
			eol = "\r\n"
		}
		switch r.Intn(9) {
		case 0, 1:
			fmt.Fprintf(&b, `{"type":"assistant","uuid":%q,"parentUuid":%s,"message":{"content":[{"type":"thinking","thinking":"hm %d","signature":"s"}]}}`, id, p, i)
		case 2:
			fmt.Fprintf(&b, `{"uuid":%q,"parentUuid":%s,"message":{"content":[{"type":"redacted_thinking","data":"x"},{"type":"text","text":"<b>&</b>"}]}}`, id, p)
		case 3:
			fmt.Fprintf(&b, `{"uuid":%q,"logicalParentUuid":%s,"parentUuid":%s,"message":{"content":"plain"}}`, id, p, p)
		case 4:
			fmt.Fprintf(&b, `{"type":"last-prompt","leafUuid":%s}`, p)
		case 5:
			fmt.Fprintf(&b, `{"uuid":%q,"parentUuid":%s,"message":{"content":[{"type":"text","text":"about \"thinking\": nothing"}]}}`, id, p)
		case 6:
			b.WriteString(`not json thinking"`)
		case 7:
			fmt.Fprintf(&b, `{"message":{"content":[{"type":"thinking","thinking":"no uuid"}]},"parentUuid":%s}`, p)
		default:
			fmt.Fprintf(&b, `{"uuid":%q,"parentUuid":%s,"message":{"content":[{"type":"tool_use","id":"t%d"}]}}`, id, p, i)
		}
		b.WriteString(eol)
		parent = fmt.Sprintf("%q", id)
	}
	return b.String()
}

// refStrip is StripThinking as it was, line by line with refStripLine.
func refStrip(data []byte) ([]byte, int) {
	redirect := map[string]json.RawMessage{}
	var out bytes.Buffer
	removed := 0
	for _, seg := range bytes.SplitAfter(data, []byte("\n")) {
		if len(seg) == 0 {
			continue
		}
		line := bytes.TrimRight(seg, "\r\n")
		rewritten, drop, n, err := refStripLine(line, redirect)
		if err != nil {
			panic(err)
		}
		removed += n
		switch {
		case drop:
		case rewritten != nil:
			out.Write(append(rewritten, seg[len(line):]...))
		default:
			out.Write(seg)
		}
	}
	return out.Bytes(), removed
}

// refStripLine is the one-pass strip that PreStrip and finish replace, kept as
// the reference they must agree with.
func refStripLine(line []byte, redirect map[string]json.RawMessage) (rewritten []byte, drop bool, removed int, err error) {
	if len(line) == 0 || line[0] != '{' {
		return nil, false, 0, nil
	}
	if !bytes.Contains(line, thinkingMark) && !linksTo(line, redirect) {
		return nil, false, 0, nil
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(line, &top) != nil {
		return nil, false, 0, nil // not ours to judge
	}

	changed := false
	for _, k := range linkKeys {
		var id string
		if raw, ok := top[k]; ok && json.Unmarshal(raw, &id) == nil {
			if to, ok := redirect[id]; ok {
				top[k] = to
				changed = true
			}
		}
	}

	if rawMsg, ok := top["message"]; ok {
		var msg map[string]json.RawMessage
		var blocks []json.RawMessage
		if json.Unmarshal(rawMsg, &msg) == nil && json.Unmarshal(msg["content"], &blocks) == nil && len(blocks) > 0 {
			kept := make([]json.RawMessage, 0, len(blocks))
			for _, b := range blocks {
				if isThinking(b) {
					removed++
				} else {
					kept = append(kept, b)
				}
			}
			if removed > 0 {
				if len(kept) == 0 {
					var id string
					if raw, ok := top["uuid"]; ok && json.Unmarshal(raw, &id) == nil {
						parent := top["parentUuid"]
						if parent == nil {
							parent = json.RawMessage("null")
						}
						redirect[id] = parent
					}
					return nil, true, removed, nil
				}
				if msg["content"], err = encode(kept); err != nil {
					return nil, false, 0, err
				}
				if top["message"], err = encode(msg); err != nil {
					return nil, false, 0, err
				}
				changed = true
			}
		}
	}

	if !changed {
		return nil, false, 0, nil
	}
	rewritten, err = encode(top)
	return rewritten, false, removed, err
}

// thinkingMark appears in the raw text of every line that has a thinking or
// redacted_thinking block, so other lines need no JSON parsing.
var thinkingMark = []byte(`thinking"`)

// linksTo reports whether line may refer to a dropped line through one of the
// link keys. It over-approximates (a hit only costs a full parse) but never
// misses a key whose value is a plain string.
func linksTo(line []byte, redirect map[string]json.RawMessage) bool {
	if len(redirect) == 0 {
		return false
	}
	for _, k := range linkKeys {
		mark := []byte(`"` + k + `"`)
		for rest := line; ; {
			i := bytes.Index(rest, mark)
			if i < 0 {
				break
			}
			rest = rest[i+len(mark):]
			v := bytes.TrimLeft(rest, " \t")
			if len(v) == 0 || v[0] != ':' {
				continue
			}
			v = bytes.TrimLeft(v[1:], " \t")
			if len(v) == 0 || v[0] != '"' {
				continue
			}
			v = v[1:]
			end := bytes.IndexByte(v, '"')
			if end < 0 {
				return true // malformed: let the parser decide
			}
			if _, ok := redirect[string(v[:end])]; ok {
				return true
			}
			if bytes.IndexByte(v[:end], '\\') >= 0 {
				return true // escaped value: not a plain uuid
			}
		}
	}
	return false
}

// linkKeys hold the uuid of another transcript line.
var linkKeys = []string{"parentUuid", "logicalParentUuid", "leafUuid"}

func isThinking(block json.RawMessage) bool {
	var b struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(block, &b) == nil && (b.Type == "thinking" || b.Type == "redacted_thinking")
}

// encode marshals without HTML escaping, which would alter the text.
func encode(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
