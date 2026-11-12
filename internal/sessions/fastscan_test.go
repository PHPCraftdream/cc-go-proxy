package sessions

import (
	"bytes"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// The regular expressions the finders of fastscan.go replace, and how they
// were used: the reference the finders must agree with, byte for byte.
var (
	refUUID      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	refUUIDValue = regexp.MustCompile(`"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
	refAPIID     = regexp.MustCompile(`(?:srvtoolu|toolu|msg|req)_0[0-9A-Za-z]{23}`)
	refCodexID   = regexp.MustCompile(`(?:call|resp|fc|rs)_[0-9A-Za-z]{20,64}`)
)

func refReplaceAt(b []byte, idx [][]int, fn func([]byte) []byte) []byte {
	if len(idx) == 0 {
		return b
	}
	var out []byte
	last := 0
	for _, ix := range idx {
		if ix[1] < len(b) && isAlnum(b[ix[1]]) {
			continue
		}
		if s := ix[0]; s > 0 && isAlnum(b[s-1]) && (s < 2 || b[s-2] != '\\' || strings.IndexByte("nrtbf", b[s-1]) < 0) {
			continue
		}
		out = append(out, b[last:ix[0]]...)
		out = append(out, fn(b[ix[0]:ix[1]])...)
		last = ix[1]
	}
	return append(out, b[last:]...)
}

func (m *forkIDs) refMapUUIDs(b []byte) []byte {
	return refUUID.ReplaceAllFunc(b, func(u []byte) []byte {
		s := strings.ToLower(string(u))
		if !m.uuids[s] {
			return u
		}
		return []byte(forkUUID(m.fid, s))
	})
}

func (m *forkIDs) refMapAPIIDs(b []byte) []byte {
	b = refReplaceAt(b, refAPIID.FindAllIndex(b, -1), func(id []byte) []byte {
		i := bytes.IndexByte(id, '_') + 3
		return append(append([]byte{}, id[:i]...), derive(m.fid, string(id), base62, len(id)-i)...)
	})
	return refReplaceAt(b, refCodexID.FindAllIndex(b, -1), func(id []byte) []byte {
		i := bytes.IndexByte(id, '_') + 1
		body := id[i:]
		if bytes.IndexFunc(body, func(r rune) bool { return r >= '0' && r <= '9' }) < 0 {
			return id
		}
		alphabet := base62
		if len(bytes.Trim(body, hexDigits)) == 0 {
			alphabet = hexDigits
		}
		return append(append([]byte{}, id[:i]...), derive(m.fid, string(id), alphabet, len(body))...)
	})
}

func refUUIDValues(b []byte) map[string]bool {
	set := map[string]bool{}
	for _, sm := range refUUIDValue.FindAllSubmatch(b, -1) {
		set[strings.ToLower(string(sm[1]))] = true
	}
	return set
}

// fastCases are the edges: uuids glued, upper case, cut short, in quotes or
// not; API ids of every prefix, too short, too long, inside a word, after a
// JSON escape, without a digit.
var fastCases = []string{
	"",
	`"` + fu1 + `"`,
	fu1 + fu1,
	fu1 + "-" + fu1,
	strings.ToUpper(fu1),
	fu1[:35],
	`x"` + fu1 + `"y and "` + fu2 + `" and ` + fpr,
	"aaaaaaaa-0000-4000-8000-00000000000g",
	"--------" + fu1,
	ftool + " " + fmsg + " " + freq + " srv" + ftool,
	"x" + ftool,
	ftool + "x",
	`\n` + ftool + `\t` + fmsg,
	`\\n` + ftool,
	"2" + ftool,
	"toolu_1AbCdEfGhIjKlMnOpQrStUv",
	"toolu_0AbCdEfGhIjKlMnOpQrStU",
	"call_f2bafbdac980443f9b296d95129b00b6 resp_0679676010f4e302016af510224ba087d1bd075c135a7fb41d",
	"recall_f2bafbdac980443f9b296d95129b00b6",
	"call_backgroundHandlersNow",
	"call_" + strings.Repeat("a1", 32),
	"call_" + strings.Repeat("a1", 33),
	"fc_abcdefghij0123456789 rs_ABCDEFGHIJ0123456789",
	"__call___resp_ _msg_ toolu_",
	"<tool-use-id>call_b2a468f96d0000000000000000000000</tool-use-id>",
	"xsrvtoolu_" + ftool[6:],
	"msg_" + ftool[6:] + "_" + ftool,
}

// fastAlphabet builds random lines out of the pieces the finders look at.
var fastAlphabet = []string{
	"a", "f", "0", "9", "Z", "-", "_", `"`, `\`, "n", " ", "\n",
	"toolu", "srvtoolu", "msg", "req", "call", "resp", "fc", "rs", "re",
	"01", "aaaaaaaa", "0000", "4000", "8000", "000000000001", fu1, strings.ToUpper(fu1),
	"f2bafbdac980443f9b296d95129b00b6", "AbCdEfGhIjKlMnOpQrStUv",
}

func fastSamples(t *testing.T) []string {
	t.Helper()
	r := rand.New(rand.NewSource(1))
	out := append([]string{}, fastCases...)
	for n := 0; n < 20000; n++ {
		var b strings.Builder
		for k := r.Intn(30); k >= 0; k-- {
			b.WriteString(fastAlphabet[r.Intn(len(fastAlphabet))])
		}
		out = append(out, b.String())
	}
	return out
}

func TestFastFindersAgreeWithTheRegularExpressions(t *testing.T) {
	m := &forkIDs{fid: forkID(sid), uuids: map[string]bool{fu1: true, strings.ToLower(fu2): true, "aaaaaaaa-0000-4000-8000-000000000000": true}}
	for _, s := range fastSamples(t) {
		in := []byte(s)
		if got, want := string(m.mapUUIDs(append([]byte{}, in...))), string(m.refMapUUIDs(append([]byte{}, in...))); got != want {
			t.Fatalf("uuids of %q:\n got %q\nwant %q", s, got, want)
		}
		if got, want := string(m.mapAPIIDs(append([]byte{}, in...))), string(m.refMapAPIIDs(append([]byte{}, in...))); got != want {
			t.Fatalf("api ids of %q:\n got %q\nwant %q", s, got, want)
		}
		got := map[string]bool{}
		collectUUIDValues(in, got)
		want := refUUIDValues(in)
		if len(got) != len(want) {
			t.Fatalf("uuid values of %q: got %v, want %v", s, got, want)
		}
		for k := range want {
			if !got[k] {
				t.Fatalf("uuid values of %q: got %v, want %v", s, got, want)
			}
		}
	}
}
