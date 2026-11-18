package forkids

import (
	"bufio"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/sessions/jsonl"
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

// Different forks map an id differently; the session id stays the fork's.
func TestForkIDsDependOnTheFork(t *testing.T) {
	a, b := &IDs{FID: ForkID("one")}, &IDs{FID: ForkID("two")}
	if string(a.Line([]byte(ftool))) == string(b.Line([]byte(ftool))) || a.MapShort(fagent) == b.MapShort(fagent) {
		t.Error("two forks got the same ids")
	}
	if got := string(a.Line([]byte(`"sessionId":"` + a.FID + `"`))); got != `"sessionId":"`+a.FID+`"` {
		t.Errorf("the fork id was mapped again: %s", got)
	}
	// An id glued to a longer run is not that id.
	long := ftool + "X"
	if got := string(a.Line([]byte(long))); got != long {
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
	m, err := New(sid, ForkID(sid), tr, filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(m.Line([]byte(`see ` + fu1 + `, run ` + external + `, deployment done, ` + fword + ` too`)))
	want := `see ` + ForkUUID(m.FID, fu1) + `, run ` + external + `, deployment done, ` + m.MapShort(fword) + ` too`
	if got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	if m.short["deployment"] || !m.short[fword] || m.uuids[external] || !m.uuids[fu1] {
		t.Errorf("short %v, uuids %v", m.short, m.uuids)
	}
	for id, want := range map[string]bool{"a004a671": true, "b01bxagzb": true, "bxagzbcde": true, "123456": false, "abcdefgh": false, "benchmarks": false} {
		m := &IDs{short: map[string]bool{}}
		if m.add(id); m.short[id] != want {
			t.Errorf("add(%q) kept %v, want %v", id, m.short[id], want)
		}
	}
}

// A session run through ccgp on Codex holds the Responses API's ids: tool
// calls call_<32 hex>, replies resp_<50 hex>. They are the session's too.
func TestForkReplacesCodexIDs(t *testing.T) {
	const (
		call = "call_f2bafbdac980443f9b296d95129b00b6"
		resp = "resp_0679676010f4e302016af510224ba087d1bd075c135a7fb41d"
	)
	m := &IDs{FID: ForkID(sid)}
	in := `{"message":{"id":"` + resp + `","content":[{"type":"tool_use","id":"` + call + `"}]}}` + "\n" +
		`{"content":[{"type":"tool_result","tool_use_id":"` + call + `"}],"text":"<tool-use-id>` + call + `</tool-use-id>\n` + call + ` done"}` + "\n"
	got := string(m.Line([]byte(in)))
	ncall, nresp := string(m.Line([]byte(call))), string(m.Line([]byte(resp)))
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
		if got := string(m.Line([]byte(keep))); got != keep {
			t.Errorf("%s -> %s", keep, got)
		}
	}
}

// The ids of a large file found on several goroutines are those found line by line.
func TestForkIDsParallelScanMatchesSequential(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	var b strings.Builder
	for b.Len() < 6*(jsonl.ParallelFrom/4) {
		u := fmt.Sprintf("%08x-0000-4000-8000-%012x", r.Uint32(), r.Int63n(1<<40))
		fmt.Fprintf(&b, `{"uuid":"%s","slug":"calm-%d-river","agentId":"a%016x","text":"with ID: b%07dq done"}`+"\n",
			u, r.Intn(5), r.Uint64(), r.Intn(1e7))
		b.WriteString(strings.Repeat("z", r.Intn(2000)) + "\n")
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	par := &IDs{FID: ForkID(sid), uuids: map[string]bool{}, short: map[string]bool{}, Slugs: map[string]string{}}
	if err := par.scan(path); err != nil {
		t.Fatal(err)
	}
	seq := &IDs{FID: ForkID(sid), uuids: map[string]bool{}, short: map[string]bool{}, Slugs: map[string]string{}}
	br := bufio.NewReader(strings.NewReader(b.String()))
	var long []byte
	for {
		line, err := jsonl.ReadLine(br, &long)
		seq.scanLine(line)
		if err != nil {
			break
		}
	}
	if fmt.Sprint(par.uuids) != fmt.Sprint(seq.uuids) || fmt.Sprint(par.short) != fmt.Sprint(seq.short) ||
		fmt.Sprint(par.Slugs) != fmt.Sprint(seq.Slugs) || len(seq.uuids) < 100 || len(seq.short) < 100 || len(seq.Slugs) != 5 {
		t.Errorf("parallel %d/%d/%d ids, sequential %d/%d/%d", len(par.uuids), len(par.short), len(par.Slugs),
			len(seq.uuids), len(seq.short), len(seq.Slugs))
	}
}

// After prepare, a short id is told by its shape first and mapped once; the
// result is what the map alone says.
func TestPreparedShortIDs(t *testing.T) {
	m := &IDs{FID: ForkID(sid), short: map[string]bool{}}
	for _, id := range []string{"a0123456789abcdef", "bk7f3m2q1", "bqwertyui", "abc123"} {
		m.add(id)
	}
	plain := &IDs{FID: m.FID, short: m.short}
	m.prepare()
	for _, w := range []string{"a0123456789abcdef", "bk7f3m2q1", "bqwertyui", "abc123", "abc124", "a0123456789abcdeg",
		"deployment", "b", "", strings.Repeat("a", 40), "bk7f3m2q"} {
		if m.isShort([]byte(w)) != plain.isShort([]byte(w)) {
			t.Errorf("%q: prepared %v, plain %v", w, m.isShort([]byte(w)), plain.isShort([]byte(w)))
		}
		if plain.short[w] && m.shortFor(w) != plain.MapShort(w) {
			t.Errorf("%q: %s, want %s", w, m.shortFor(w), plain.MapShort(w))
		}
	}
	text := []byte(`{"agentId":"a0123456789abcdef","t":"bk7f3m2q1 and\nbqwertyui, abc123x abc123"}`)
	if got, want := string(m.Line(text)), string(plain.Line(text)); got != want {
		t.Errorf("line: %s, want %s", got, want)
	}
}

// replaceWord replaces old where it stands alone: not inside a longer run of
// letters, digits, '-' and '_' (another slug, or the fork's own, which begins
// with it).
func replaceWord(b []byte, old, as string) []byte {
	if out, changed := ReplaceWordTo(nil, b, old, as); changed {
		return out
	}
	return b
}

const sid = "11111111-2222-4333-8444-555555555555"

// mapUUIDs gives each uuid of b that is in set its fork id.
func (m *IDs) mapUUIDs(b []byte) []byte {
	if out, changed := m.mapUUIDsTo(nil, b); changed {
		return out
	}
	return b
}

// mapAPIIDs gives each API id of b its fork id: the prefix, and for the
// Anthropic API the 0 and the next character, stay; the rest is derived in the
// alphabet it was in. An id must stand alone: no letter or digit right after
// it, none right before (but the letter of a JSON escape: \ncall_…).
func (m *IDs) mapAPIIDs(b []byte) []byte {
	if out, changed := m.mapAPIIDsTo(nil, b); changed {
		return out
	}
	return b
}

// shortIDs replaces each run of letters and digits that is a known short id. A
// run glued to a JSON escape (\n, \t, …) is the id after it.
func (m *IDs) shortIDs(b []byte) []byte {
	if out, changed := m.shortIDsTo(nil, b); changed {
		return out
	}
	return b
}

func put(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// slugField is the slug a transcript line names: three words Claude Code
// makes up for the session, the name of its plan file.
var slugField = regexp.MustCompile(`"slug"\s*:\s*"([A-Za-z0-9][A-Za-z0-9_-]{1,80})"`)
