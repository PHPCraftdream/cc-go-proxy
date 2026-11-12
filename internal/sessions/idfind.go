package sessions

import "bytes"

// Hand-written finders for the short ids and slugs a fork collects, in place
// of regular expressions that ran over every line of a transcript (kept in
// idfind_test.go, which proves both find the same). Each finds what
// FindAllSubmatch did: leftmost first, a match never overlapping the last.

// isSpace is \s of Go's regular expressions.
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r' }

// alnumRun is the length of the run of letters and digits at b[i:].
func alnumRun(b []byte, i int) int {
	j := i
	for j < len(b) && isAlnum(b[j]) {
		j++
	}
	return j - i
}

// fieldValue reads `\s*:\s*"<value>"` at b[i:], the value being what ok says
// of the run up to the next '"'; it returns the value and the end of the match.
func fieldValue(b []byte, i int, ok func([]byte) bool) (value []byte, end int, found bool) {
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	if i >= len(b) || b[i] != ':' {
		return nil, 0, false
	}
	i++
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	if i >= len(b) || b[i] != '"' {
		return nil, 0, false
	}
	i++
	q := bytes.IndexByte(b[i:], '"')
	if q < 0 || !ok(b[i:i+q]) {
		return nil, 0, false
	}
	return b[i : i+q], i + q + 1, true
}

// shortValue is a short id as a field holds it: 6 to 24 letters and digits.
func shortValue(v []byte) bool { return len(v) >= 6 && len(v) <= 24 && alnumRun(v, 0) == len(v) }

// isShortKey tells the keys of a short id: agentId, agent_id, bash_id,
// bashId, shell_id, shellId and any <letters>[Tt]ask[_][Ii]d.
func isShortKey(k []byte) bool {
	switch string(k) {
	case "agentId", "agent_id", "bash_id", "bashId", "shell_id", "shellId":
		return true
	}
	// [A-Za-z]*[Tt]ask_?[Ii]d
	n := len(k)
	if n < 6 || k[n-1] != 'd' || k[n-2] != 'I' && k[n-2] != 'i' {
		return false
	}
	rest := k[:n-2]
	if len(rest) > 0 && rest[len(rest)-1] == '_' {
		rest = rest[:len(rest)-1]
	}
	if len(rest) < 4 || string(rest[len(rest)-3:]) != "ask" || rest[len(rest)-4] != 't' && rest[len(rest)-4] != 'T' {
		return false
	}
	for _, c := range rest[:len(rest)-4] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// eachShortField calls fn with the value of every field of a short id, as
// "<key>"\s*:\s*"<6 to 24 letters and digits>". Each key ends in id, Id (or
// is agentId), so the search jumps to every `d"`.
func eachShortField(b []byte, fn func([]byte)) {
	done := 0 // end of the last match
	for i := 0; ; {
		j := bytes.Index(b[i:], []byte(`d"`))
		if j < 0 {
			return
		}
		close := i + j + 1 // the key's closing quote
		i = close
		start := close - 1
		for start > 0 && (isAlnum(b[start-1]) || b[start-1] == '_') {
			start--
		}
		start-- // the opening quote
		if start < done || start < 0 || b[start] != '"' || !isShortKey(b[start+1:close]) {
			continue
		}
		v, end, ok := fieldValue(b, close+1, shortValue)
		if !ok {
			continue
		}
		fn(v)
		done, i = end, end
	}
}

// shortTextPrefixes name a task or subagent in text: "with ID: b…",
// "<task-id>b…", "agentId: a…", ".../tasks/b….output".
var shortTextPrefixes = [][]byte{[]byte("with ID: "), []byte("<task-id>"), []byte("agentId: "), []byte("tasks")}

// eachShortText calls fn with every id named in text after one of
// shortTextPrefixes (tasks followed by one or more / or \): a whole run of 6
// to 24 letters and digits.
func eachShortText(b []byte, fn func([]byte)) {
	var next [4]int // next occurrence of each prefix at or after i, -1: none left
	for k := range next {
		next[k] = -2 // not looked for yet
	}
	for i := 0; i < len(b); {
		at, p := -1, -1
		for k, pre := range shortTextPrefixes {
			if next[k] == -2 || next[k] >= 0 && next[k] < i {
				next[k] = -1
				if j := bytes.Index(b[i:], pre); j >= 0 {
					next[k] = i + j
				}
			}
			if next[k] >= 0 && (at < 0 || next[k] < at) {
				at, p = next[k], k
			}
		}
		if at < 0 {
			return
		}
		id := at + len(shortTextPrefixes[p])
		if p == 3 { // tasks[\\/]+
			s := id
			for id < len(b) && (b[id] == '/' || b[id] == '\\') {
				id++
			}
			if id == s {
				i = at + 1
				continue
			}
		}
		n := alnumRun(b, id)
		if n < 6 || n > 24 {
			i = at + 1
			continue
		}
		fn(b[id : id+n])
		end := id + n
		switch {
		case bytes.HasPrefix(b[end:], []byte("</task-id>")):
			end += len("</task-id>")
		case bytes.HasPrefix(b[end:], []byte(".output")):
			end += len(".output")
		case end < len(b):
			end++
		}
		i = end
	}
}

// slugValue is a slug: a letter or digit, then 1 to 80 letters, digits, - or _.
func slugValue(v []byte) bool {
	if len(v) < 2 || len(v) > 81 || !isAlnum(v[0]) {
		return false
	}
	for _, c := range v[1:] {
		if !isAlnum(c) && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// eachSlug calls fn with the value of every "slug" field.
func eachSlug(b []byte, fn func([]byte)) {
	key := []byte(`"slug"`)
	for i := 0; ; {
		j := bytes.Index(b[i:], key)
		if j < 0 {
			return
		}
		v, end, ok := fieldValue(b, i+j+len(key), slugValue)
		if !ok {
			i += j + 1
			continue
		}
		fn(v)
		i = end
	}
}
