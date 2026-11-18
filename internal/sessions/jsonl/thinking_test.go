package jsonl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
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

func TestStripSplitMatchesTheOnePassStrip(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	for range 200 {
		in := []byte(thinkingTranscript(r, 1+r.Intn(60)))
		want, wantN := refStrip(in)
		got, n, err := StripThinking(in)
		if err != nil || !bytes.Equal(got, want) || n != wantN {
			t.Fatalf("got %d removed, %v:\n%s\nwant %d:\n%s\nfrom:\n%s", n, err, got, wantN, want, in)
		}
	}
}

// The link values found on a worker say what linksTo said of the line.
func TestLinkSpansAgreeWithLinksTo(t *testing.T) {
	r := rand.New(rand.NewSource(13))
	pieces := []string{`"parentUuid"`, `"logicalParentUuid"`, `"leafUuid"`, `:`, ` : `, "\t", `"`, `"gone"`, `"kept"`, `gone`,
		`"go\"ne"`, `null`, `,`, `{`, `}`, `"parentUuid":"gone"`, `"leafUuid": "kept"`, `x`}
	redirect := map[string]json.RawMessage{"gone": json.RawMessage(`"p"`)}
	for range 20000 {
		var b strings.Builder
		for range r.Intn(10) {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		line := []byte(b.String())
		spans, must := linkSpans(line)
		hit := must
		for _, sp := range spans {
			if _, ok := redirect[string(line[sp[0]:sp[1]])]; ok {
				hit = true
			}
		}
		if want := linksTo(line, redirect); hit != want {
			t.Fatalf("%s: spans say %v, linksTo %v", line, hit, want)
		}
	}
}
