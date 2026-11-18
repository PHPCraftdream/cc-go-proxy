// Package cfgtext edits Ktav config text line by line: finding lists and objects,
// reading keys, setting a scalar, keeping the file's line endings.

package cfgtext

import (
	"fmt"
	"strings"
)

// TrimLine strips spaces, tabs and a trailing CR from a raw line so that
// recognition works for LF, CRLF and mixed files alike.
func TrimLine(s string) string { return strings.Trim(s, " \t\r") }

// KeyValue splits a line at its first ':' and trims spaces, tabs and '\r'
// from both sides. Comment lines never match.
func KeyValue(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	key = TrimLine(line[:i])
	if key == "" || strings.HasPrefix(key, "#") {
		return "", "", false
	}
	return key, TrimLine(line[i+1:]), true
}

// IsListHeader reports whether line is `key` + optional spaces + ':' +
// optional spaces + '[', with nothing after the bracket but spaces, tabs or
// a trailing CR. Comment lines never match (their text does not start with
// the key).
func IsListHeader(line, key string) bool {
	rest, ok := strings.CutPrefix(TrimLine(line), key)
	if !ok {
		return false
	}
	rest, ok = strings.CutPrefix(strings.TrimLeft(rest, " \t"), ":")
	if !ok {
		return false
	}
	return TrimLine(rest) == "["
}

// IsCloseBracket reports whether line is a lone ']' (with optional spaces,
// tabs and trailing CR).
func IsCloseBracket(line string) bool { return TrimLine(line) == "]" }

// DominantCRLF reports whether lines (split on "\n") are CRLF-dominant: a
// majority of newline-terminated elements carry their own trailing "\r".
// Only lines[:len(lines)-1] are terminated (strings.Split drops the final
// newline into the last element, which may be empty). Ties go to LF.
func DominantCRLF(lines []string) bool {
	if len(lines) < 2 {
		return false
	}
	term := lines[:len(lines)-1]
	crlf := 0
	for _, l := range term {
		if strings.HasSuffix(l, "\r") {
			crlf++
		}
	}
	return crlf*2 > len(term)
}

// WithEndings appends "\r" to every inserted line for CRLF-dominant files.
// Lines the caller kept from the input are never passed here, so their own
// endings stay byte-identical.
func WithEndings(lines []string, crlf bool) []string {
	if !crlf {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l + "\r"
	}
	return out
}

func IndentOf(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

// ListSpan locates a top-level `key: [ ... ]` list, braces alone on their lines.
type ListSpan struct{ Open, Close int }

func FindList(lines []string, key string) (ListSpan, error) {
	for i, l := range lines {
		if !IsListHeader(l, key) {
			continue
		}
		ind := IndentOf(l)
		for j := i + 1; j < len(lines); j++ {
			if IsCloseBracket(lines[j]) && IndentOf(lines[j]) == ind {
				return ListSpan{i, j}, nil
			}
		}
		return ListSpan{}, fmt.Errorf("config has no closing %q for %q", "]", key+": [")
	}
	return ListSpan{}, fmt.Errorf("config has no %q list", key+": [")
}

// SiblingIndent is the indent of the first object in the list; 4 spaces when
// the list is empty (the default file's style).
func SiblingIndent(lines []string, sp ListSpan) string {
	for i := sp.Open + 1; i < sp.Close; i++ {
		if strings.TrimSpace(lines[i]) == "{" {
			return IndentOf(lines[i])
		}
	}
	return "    "
}

// HasKey reports whether any line's KeyValue key equals key; comments never
// match.
func HasKey(lines []string, key string) bool {
	for _, l := range lines {
		if k, _, ok := KeyValue(l); ok && k == key {
			return true
		}
	}
	return false
}

// FindObjectNamed finds the object in [lo, hi] whose `name:` line is name.
// wantProviderKey distinguishes profiles (have a `provider` key) from
// providers.
func FindObjectNamed(lines []string, lo, hi int, name string, wantProviderKey bool) (open, closeAt int, ok bool) {
	for i := lo + 1; i < hi; i++ {
		k, v, ok := KeyValue(lines[i])
		if !ok || k != "name" || v != name {
			continue
		}
		o, c, found := ObjectAround(lines, i)
		if !found || o < lo || c > hi || HasKey(lines[o+1:c], "provider") != wantProviderKey {
			continue
		}
		return o, c, true
	}
	return 0, 0, false
}

// FindProfileObject locates a profile and reads its `provider:` value.
func FindProfileObject(lines []string, name string) (open, closeAt int, provider string, err error) {
	for i, l := range lines {
		k, v, ok := KeyValue(l)
		if !ok || k != "name" || v != name {
			continue
		}
		o, c, ok := ObjectAround(lines, i)
		if !ok || !HasKey(lines[o+1:c], "provider") {
			continue
		}
		for _, l2 := range lines[o+1 : c] {
			if k, v, ok := KeyValue(l2); ok && k == "provider" {
				provider = v
			}
		}
		return o, c, provider, nil
	}
	return 0, 0, "", fmt.Errorf("profile %q not found in the config", name)
}

// SetScalarKey replaces, removes or adds the line `key: value` at the object's
// own indent (nested objects are skipped). A new line goes after the last line
// whose key is in after, else right after the opening brace.
func SetScalarKey(lines []string, open, closeAt int, key, value string, after []string) (string, error) {
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s: the value must be one line", key)
	}
	value = strings.TrimSpace(value)
	child := IndentOf(lines[open]) + "    "
	for i := open + 1; i < closeAt; i++ {
		if _, _, ok := KeyValue(lines[i]); ok {
			child = IndentOf(lines[i])
			break
		}
	}
	keyAt, afterAt := -1, open
	for i := open + 1; i < closeAt; i++ {
		if IndentOf(lines[i]) != child {
			continue
		}
		k, _, ok := KeyValue(lines[i])
		if !ok {
			continue
		}
		if k == key {
			keyAt = i
		}
		if InList(after, k) {
			afterAt = i
		}
	}
	switch {
	case keyAt >= 0 && value == "":
		return strings.Join(append(lines[:keyAt:keyAt], lines[keyAt+1:]...), "\n"), nil
	case keyAt >= 0:
		cr := lines[keyAt][len(strings.TrimRight(lines[keyAt], "\r")):]
		lines[keyAt] = child + key + ": " + value + cr
		return strings.Join(lines, "\n"), nil
	case value == "":
		return strings.Join(lines, "\n"), nil
	}
	add := WithEndings([]string{child + key + ": " + value}, DominantCRLF(lines))
	out := append(append(append([]string{}, lines[:afterAt+1]...), add...), lines[afterAt+1:]...)
	return strings.Join(out, "\n"), nil
}

// ObjectAround finds the { ... } lines enclosing line i (braces alone on their lines).
func ObjectAround(lines []string, i int) (open, closeAt int, ok bool) {
	for open = i; open >= 0; open-- {
		if strings.TrimSpace(lines[open]) == "{" {
			break
		}
	}
	if open < 0 {
		return 0, 0, false
	}
	indent := lines[open][:len(lines[open])-len(strings.TrimLeft(lines[open], " \t"))]
	for closeAt = i; closeAt < len(lines); closeAt++ {
		if strings.TrimSpace(lines[closeAt]) == "}" && strings.HasPrefix(lines[closeAt], indent) &&
			len(lines[closeAt])-len(strings.TrimLeft(lines[closeAt], " \t")) == len(indent) {
			return open, closeAt, true
		}
	}
	return 0, 0, false
}

func InList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
