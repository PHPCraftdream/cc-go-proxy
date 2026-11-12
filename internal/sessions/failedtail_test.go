package sessions

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lines as Claude Code writes them, cut down to what failedTail looks at.
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

// scheduledTurn is a turn of a scheduled prompt that got an API error.
func scheduledTurn() string {
	return `{"type":"queue-operation","operation":"enqueue","content":"# tick"}` + "\n" +
		`{"type":"queue-operation","operation":"dequeue"}` + "\n" +
		`{"type":"system","subtype":"scheduled_task_fire","uuid":"s1","parentUuid":"d1"}` + "\n" +
		`{"type":"api-request-blob","hash":"h"}` + "\n" + lRequest +
		lTick + lState + lRequest + lRequest + lError + lDuration
}

// kept is a conversation whose last real line is a tool result's hook
// attachment, followed by the state Claude Code writes before the next request.
const kept = lPrompt + lState + lReply + lResult + lAttach + lState

// failedEnd is what came after kept: the request that failed, then two
// scheduled prompts that failed too.
var failedEnd = lRequest + lRequest + lError + lDuration + scheduledTurn() + scheduledTurn()

func tailOf(t *testing.T, s string) (int64, int) {
	t.Helper()
	cut, failed, err := failedTail(strings.NewReader(s), int64(len(s)))
	if err != nil {
		t.Fatal(err)
	}
	return cut, failed
}

func TestClassifyTail(t *testing.T) {
	for _, c := range []struct {
		line string
		want tailKind
	}{
		{lError, tailFailed},
		{lDuration, tailNoise},
		{`{"type":"system","subtype":"scheduled_task_fire","uuid":"x"}`, tailNoise},
		{`{"type":"system","subtype":"api_error","uuid":"x"}`, tailNoise},
		{`{"type":"system","subtype":"stop_hook_summary","uuid":"x"}`, tailNoise},
		{`{"type":"system","subtype":"away_summary","uuid":"x"}`, tailNoise},
		{lTick, tailNoise},
		{lRequest, tailNoise},
		{`{"type":"api-request-blob","hash":"h"}`, tailNoise},
		{`{"type":"queue-operation","operation":"dequeue"}`, tailNoise},
		{`{"type":"ai-title","aiTitle":"x"}`, tailState},
		{`{"type":"last-prompt","lastPrompt":"x","leafUuid":"u1"}`, tailState},
		{`{"type":"relocated","relocatedCwd":"D:\\x"}`, tailState},
		{"", tailState},
		{`{"type":"user","uuid":"x","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`, tailNoise},
		{`{"type":"user","uuid":"x","message":{"content":"[Request interrupted by user for tool use]"}}`, tailNoise},
		{`{"type":"user","uuid":"x","message":{"content":[{"type":"text","text":"[Request interrupted by user]"},{"type":"text","text":"and more"}]}}`, tailUseful},
		{`{"type":"user","uuid":"x","message":{"content":"[Request interrupted by user] but go on"}}`, tailUseful},
		{`{"type":"api-request-blob","hash":"h","message":"[Request interrupted by user]"}`, tailNoise},
		{`{"type":"user","uuid":"x","message":"[Request interrupted by user]"}`, tailUseful},
		{lPrompt, tailUseful},
		{lReply, tailUseful},
		{lResult, tailUseful},
		{lAttach, tailUseful},
		{`{"type":"user","uuid":"x","isMeta":true,"message":{"content":"caveat"}}`, tailUseful},
		{`{"type":"user","uuid":"x","turnOrigin":"task_notification","message":{"content":"done"}}`, tailUseful},
		{`{"type":"system","subtype":"compact_boundary","uuid":"x"}`, tailUseful},
		{`{"type":"system","subtype":"informational","uuid":"x"}`, tailUseful},
		{`{"type":"assistant","uuid":"x","isApiErrorMessage":false}`, tailUseful},
		{`not json`, tailUseful},
	} {
		if got := classifyTail([]byte(strings.TrimRight(c.line, "\n"))); got != c.want {
			t.Errorf("%s: got %d, want %d", c.line, got, c.want)
		}
	}
}

func TestFailedTail(t *testing.T) {
	for _, c := range []struct {
		name       string
		in         string
		keep       string // what stays, "" for all of in
		wantFailed int
	}{
		{"failed end after a tool result", kept + failedEnd, kept, 3},
		{"one error", kept + lRequest + lError + lDuration, kept, 1},
		{"a typed prompt that failed keeps its prompt and state", lPrompt + lReply + lPrompt + lState + lRequest + lError, lPrompt + lReply + lPrompt + lState, 1},
		{"no error", kept + lRequest + lDuration, "", 0},
		{"an error answered later", kept + failedEnd + lPrompt + lReply, "", 0},
		{"nothing but failures", failedEnd, "", 0},
		{"empty", "", "", 0},
		{"a line it cannot read ends the failed end", kept + "garbage\n" + failedEnd, kept + "garbage\n", 3},
		// Seen live: errors, then a /compact the user stopped with Esc.
		{"an interrupt after the errors", kept + failedEnd + lRequest + lInterrupt + lState, kept, 3},
		{"an interrupt alone", kept + lInterrupt + lState, "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			keep := c.keep
			if keep == "" {
				keep = c.in
			}
			cut, failed := tailOf(t, c.in)
			if cut != int64(len(keep)) || failed != c.wantFailed {
				t.Errorf("got cut %d, %d failed; want %d, %d", cut, failed, len(keep), c.wantFailed)
			}
		})
	}
}

// smallWindows makes failedTail look at 1 KiB first and at most at 16 KiB.
func smallWindows(t *testing.T) {
	w, m := failedTailWindow, failedTailMax
	failedTailWindow, failedTailMax = 1<<10, 16<<10
	t.Cleanup(func() { failedTailWindow, failedTailMax = w, m })
}

// blob is a request record with n bytes of payload.
func blob(n int64) string {
	return `{"type":"api-request-blob","hash":"h","message":"` + strings.Repeat("x", int(n)) + `"}` + "\n"
}

func TestFailedTailLongerThanTheFirstLook(t *testing.T) {
	smallWindows(t)
	in := kept + lRequest + blob(5<<10) + lError + lDuration
	if cut, failed := tailOf(t, in); cut != int64(len(kept)) || failed != 1 {
		t.Errorf("got cut %d, %d failed; want %d, 1", cut, failed, len(kept))
	}
}

func TestFailedTailBeyondTheLimitIsKept(t *testing.T) {
	smallWindows(t)
	in := kept + blob(failedTailMax) + lError
	if cut, failed := tailOf(t, in); cut != int64(len(in)) || failed != 0 {
		t.Errorf("got cut %d, %d failed; want all of %d", cut, failed, len(in))
	}
}

// ---- through Copy ----

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
	want, _, err := StripThinking([]byte(lPrompt + thinking + reply))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != Copied || r.Failed != 1 || r.Removed != 1 || read(t, p.d) != string(want) {
		t.Fatalf("got %v, %d failed, %d removed, %q", r.Status, r.Failed, r.Removed, read(t, p.d))
	}
	// A copy made before, with its end, stays as it is.
	full, _, err := StripThinking([]byte(src))
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
