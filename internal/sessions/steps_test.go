package sessions

import (
	"bytes"
	"encoding/json"
)

// The steps of the fork and the strip one at a time, as tests read them.

// mapUUIDs gives each uuid of b that is in set its fork id.
func (m *forkIDs) mapUUIDs(b []byte) []byte {
	if out, changed := m.mapUUIDsTo(nil, b); changed {
		return out
	}
	return b
}

// mapAPIIDs gives each API id of b its fork id: the prefix, and for the
// Anthropic API the 0 and the next character, stay; the rest is derived in the
// alphabet it was in. An id must stand alone: no letter or digit right after
// it, none right before (but the letter of a JSON escape: \ncall_…).
func (m *forkIDs) mapAPIIDs(b []byte) []byte {
	if out, changed := m.mapAPIIDsTo(nil, b); changed {
		return out
	}
	return b
}

// replaceWord replaces old where it stands alone: not inside a longer run of
// letters, digits, '-' and '_' (another slug, or the fork's own, which begins
// with it).
func replaceWord(b []byte, old, as string) []byte {
	if out, changed := replaceWordTo(nil, b, old, as); changed {
		return out
	}
	return b
}

// shortIDs replaces each run of letters and digits that is a known short id. A
// run glued to a JSON escape (\n, \t, …) is the id after it.
func (m *forkIDs) shortIDs(b []byte) []byte {
	if out, changed := m.shortIDsTo(nil, b); changed {
		return out
	}
	return b
}

// plainLineFn is a lineFn of fn, without notes.
func plainLineFn(fn func([]byte) []byte) lineFn {
	if fn == nil {
		return nil
	}
	return func(b []byte) ([]byte, any) { return fn(b), nil }
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
