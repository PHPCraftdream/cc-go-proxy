package jsonl

import (
	"strings"
	"testing"
)

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
	cut, failed, err := FailedTail(strings.NewReader(s), int64(len(s)))
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

// smallWindows makes FailedTail look at 1 KiB first and at most at 16 KiB.
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
