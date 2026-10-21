package sessions

import (
	"encoding/json"
	"strings"
	"testing"
)

const transcript = `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"ccgp1:xx"}]}}
{"type":"assistant","uuid":"a2","parentUuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"a < b & c"}]}}
{"type":"assistant","uuid":"a3","parentUuid":"a2","message":{"role":"assistant","content":[{"type":"redacted_thinking","data":"zz"},{"type":"tool_use","id":"t1","name":"X","input":{}}]}}
{"type":"user","uuid":"u2","parentUuid":"a3","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"last-prompt","leafUuid":"a1"}
not json
`

func lines(s string) []string { return strings.Split(strings.TrimSuffix(s, "\n"), "\n") }

func parse(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("%q: %v", line, err)
	}
	return m
}

func TestStripThinkingRelinksChain(t *testing.T) {
	out, removed, err := StripThinking([]byte(transcript))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	in, got := lines(transcript), lines(string(out))
	if len(got) != len(in)-1 {
		t.Fatalf("%d lines, want %d (the thinking-only line is dropped)", len(got), len(in)-1)
	}

	// Untouched lines are byte-identical.
	for _, i := range []int{0, 4} {
		j := i
		if i > 1 {
			j = i - 1
		}
		if got[j] != in[i] {
			t.Errorf("line %d changed:\n%s\n%s", i, in[i], got[j])
		}
	}
	if got[len(got)-1] != "not json" {
		t.Errorf("non-JSON line = %q", got[len(got)-1])
	}

	a2 := parse(t, got[1])
	if a2["uuid"] != "a2" || a2["parentUuid"] != "u1" {
		t.Errorf("a2 not re-attached to u1: %v", a2)
	}
	if !strings.Contains(got[1], "a < b & c") {
		t.Errorf("text was escaped: %s", got[1])
	}

	a3 := parse(t, got[2])
	blocks := a3["message"].(map[string]any)["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "tool_use" || a3["parentUuid"] != "a2" {
		t.Errorf("a3 = %v", a3)
	}

	if lp := parse(t, got[len(got)-2]); lp["leafUuid"] != "u1" {
		t.Errorf("last-prompt leaf = %v, want u1", lp["leafUuid"])
	}
}

func TestStripThinkingChainsOfDroppedLines(t *testing.T) {
	in := `{"type":"assistant","uuid":"t1","parentUuid":null,"message":{"content":[{"type":"thinking","thinking":"a","signature":"s"}]}}
{"type":"assistant","uuid":"t2","parentUuid":"t1","message":{"content":[{"type":"thinking","thinking":"b","signature":"s"}]}}
{"type":"assistant","uuid":"x","parentUuid":"t2","message":{"content":[{"type":"text","text":"hello"}]}}
`
	out, removed, err := StripThinking([]byte(in))
	if err != nil || removed != 2 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	got := lines(string(out))
	if len(got) != 1 {
		t.Fatalf("lines = %v", got)
	}
	if x := parse(t, got[0]); x["parentUuid"] != nil {
		t.Errorf("parentUuid = %v, want null (the dropped lines were roots)", x["parentUuid"])
	}
}

func TestStripThinkingKeepsPlainTranscriptIntact(t *testing.T) {
	in := "{\"type\":\"user\",\"uuid\":\"u\",\"message\":{\"content\":\"hi\"}}\r\n{\"type\":\"assistant\",\"uuid\":\"a\",\"parentUuid\":\"u\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"x\"}]}}\r\n"
	out, removed, err := StripThinking([]byte(in))
	if err != nil || removed != 0 || string(out) != in {
		t.Errorf("out=%q removed=%d err=%v", out, removed, err)
	}
	if out, removed, err = StripThinking(nil); err != nil || removed != 0 || len(out) != 0 {
		t.Errorf("empty: %q %d %v", out, removed, err)
	}
}

func TestStripThinkingStreamMatchesSliceAndHandlesLongLines(t *testing.T) {
	long := strings.Repeat("y", 3<<20) // longer than the reader's buffer
	in := transcript + `{"type":"assistant","uuid":"big","parentUuid":"a1","message":{"content":[{"type":"text","text":"` + long + `"}]}}` + "\n"

	want, n1, err := StripThinking([]byte(in))
	if err != nil || n1 != 2 {
		t.Fatalf("StripThinking = %d, %v", n1, err)
	}
	var out strings.Builder
	n2, err := StripThinkingStream(strings.NewReader(in), &out)
	if err != nil || n2 != n1 || out.String() != string(want) {
		t.Fatalf("stream differs: n=%d err=%v", n2, err)
	}
	// The long line was re-attached to the surviving parent.
	last := lines(out.String())
	big := parse(t, last[len(last)-1])
	if big["uuid"] != "big" || big["parentUuid"] != "u1" {
		t.Errorf("long line = %v / %v", big["uuid"], big["parentUuid"])
	}
}

func TestLinksTo(t *testing.T) {
	redirect := map[string]json.RawMessage{"gone": json.RawMessage("null")}
	for line, want := range map[string]bool{
		`{"parentUuid":"gone","x":1}`:          true,
		`{"parentUuid": "gone"}`:               true,
		`{"parentUuid":"kept"}`:                false,
		`{"leafUuid":"gone"}`:                  true,
		`{"logicalParentUuid" : "gone"}`:       true,
		`{"text":"a \"parentUuid\":\"gone\""}`: false,
		`{"parentUuid":null,"uuid":"gone"}`:    false,
		`{"type":"user"}`:                      false,
	} {
		if got := linksTo([]byte(line), redirect); got != want {
			t.Errorf("linksTo(%s) = %v, want %v", line, got, want)
		}
	}
	if linksTo([]byte(`{"parentUuid":"gone"}`), nil) {
		t.Error("no redirects, yet a link was reported")
	}
}
