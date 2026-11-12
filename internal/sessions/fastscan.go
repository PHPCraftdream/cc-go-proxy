package sessions

import (
	"bytes"
	"strings"
)

// Hand-written finders for the ids of a fork. A transcript runs to gigabytes,
// and the regular expressions these replace (kept in fastscan_test.go, which
// proves both agree) start with a character class, so Go's engine tried them at
// every byte: twice in the first pass and three times in the second. These jump
// from anchor to anchor ('-' of a uuid, '_' of an API id) with bytes.IndexByte.

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// uuidAt reports a uuid (8-4-4-4-12 hex) at b[i:].
func uuidAt(b []byte, i int) bool {
	if i < 0 || i+36 > len(b) || b[i+8] != '-' || b[i+13] != '-' || b[i+18] != '-' || b[i+23] != '-' {
		return false
	}
	for k := i; k < i+36; k++ {
		if k == i+8 || k == i+13 || k == i+18 || k == i+23 {
			continue
		}
		if !isHex(b[k]) {
			return false
		}
	}
	return true
}

// eachUUID calls fn with the start of every uuid in b, leftmost first and not
// overlapping, as a regular expression finds them. It jumps to each '-' that
// could be the first dash of one.
func eachUUID(b []byte, fn func(at int)) {
	for i := 0; i+36 <= len(b); {
		j := bytes.IndexByte(b[i+8:], '-')
		if j < 0 {
			return
		}
		at := i + j
		if uuidAt(b, at) {
			fn(at)
			i = at + 36
			continue
		}
		i = at + 1
	}
}

// collectUUIDValues adds to set every uuid that is a whole JSON string in b. A
// closing quote is not also the opening one of the next ("a""b" has one value).
func collectUUIDValues(b []byte, set map[string]bool) {
	closed := -1 // the closing quote of the last value
	eachUUID(b, func(at int) {
		if at > 0 && at+36 < len(b) && b[at-1] == '"' && b[at+36] == '"' && at-1 != closed {
			set[strings.ToLower(string(b[at:at+36]))] = true
			closed = at + 36
		}
	})
}

// mapUUIDs gives each uuid of b that is in set its fork id.
func (m *forkIDs) mapUUIDs(b []byte) []byte {
	var out []byte
	last := 0
	eachUUID(b, func(at int) {
		s := strings.ToLower(string(b[at : at+36]))
		if !m.uuids[s] {
			return
		}
		out = append(append(out, b[last:at]...), forkUUID(m.fid, s)...)
		last = at + 36
	})
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}

// apiPrefixes are the prefixes of the ids an API gives out: the Anthropic API's
// (toolu_01…, 24 characters after the '_', starting with 0) and the Responses
// API's (call_…, resp_…, 20 to 64 characters with a digit among them).
var apiPrefixes = map[string]bool{"srvtoolu": true, "toolu": true, "msg": true, "req": true, "call": false, "resp": false, "fc": false, "rs": false}

// mapAPIIDs gives each API id of b its fork id: the prefix, and for the
// Anthropic API the 0 and the next character, stay; the rest is derived in the
// alphabet it was in. An id must stand alone: no letter or digit right after
// it, none right before (but the letter of a JSON escape: \ncall_…).
func (m *forkIDs) mapAPIIDs(b []byte) []byte {
	var out []byte
	last := 0
	for i := 0; i < len(b); {
		j := bytes.IndexByte(b[i:], '_')
		if j < 0 {
			break
		}
		u := i + j
		i = u + 1
		start, anthropic, ok := apiPrefixBefore(b, u)
		if !ok {
			continue
		}
		end := u + 1
		for end < len(b) && isAlnum(b[end]) {
			end++
		}
		body := b[u+1 : end]
		keep := u + 1 // where the derived part begins
		if anthropic {
			if len(body) != 24 || body[0] != '0' {
				continue
			}
			keep = u + 3
		} else if len(body) < 20 || len(body) > 64 || bytes.IndexFunc(body, func(r rune) bool { return r >= '0' && r <= '9' }) < 0 {
			continue
		}
		alphabet := base62
		if !anthropic && len(bytes.Trim(body, hexDigits)) == 0 {
			alphabet = hexDigits
		}
		out = append(append(out, b[last:keep]...), derive(m.fid, string(b[start:end]), alphabet, end-keep)...)
		last = end
		i = end
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}

// apiPrefixBefore finds the API prefix that ends at b[u] ('_'): the longest
// one the letters before it end with, as a regular expression scanning from
// the left finds it, and whether it stands alone on its left.
func apiPrefixBefore(b []byte, u int) (start int, anthropic, ok bool) {
	r := u
	for r > 0 && u-r < 8 && b[r-1] >= 'a' && b[r-1] <= 'z' {
		r--
	}
	for s := r; s < u; s++ {
		a, known := apiPrefixes[string(b[s:u])]
		if !known {
			continue
		}
		if s > 0 && isAlnum(b[s-1]) && (s < 2 || b[s-2] != '\\' || strings.IndexByte("nrtbf", b[s-1]) < 0) {
			return 0, false, false
		}
		return s, a, true
	}
	return 0, false, false
}

// shortFieldKeys are the keys shortField matches, as plain text: a line
// without any of them (most lines; every one has a sessionId) is not searched.
var shortFieldKeys = [][]byte{
	[]byte(`agentId"`), []byte(`agent_id"`), []byte(`askId"`), []byte(`askid"`), []byte(`ask_Id"`), []byte(`ask_id"`),
	[]byte(`ash_id"`), []byte(`ashId"`), []byte(`hell_id"`), []byte(`hellId"`),
}

func hasShortField(line []byte) bool {
	for _, k := range shortFieldKeys {
		if bytes.Contains(line, k) {
			return true
		}
	}
	return false
}
