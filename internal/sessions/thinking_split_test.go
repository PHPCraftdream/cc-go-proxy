package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// A copy large enough to strip on several goroutines writes what the one-pass
// strip writes.
func TestParallelStripMatchesTheOnePassStrip(t *testing.T) {
	r := rand.New(rand.NewSource(12))
	var b strings.Builder
	for b.Len() < 3*parallelFrom {
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
