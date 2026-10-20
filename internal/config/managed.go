package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/securefile"
)

// A profile may contain one block rewritten by `ccgp models apply`:
//
//	## ccgp:managed-models begin <profile>
//	...
//	## ccgp:managed-models end <profile>
//
// Everything outside the markers is left byte-for-byte untouched.
const (
	managedBegin = "## ccgp:managed-models begin "
	managedEnd   = "## ccgp:managed-models end "
)

// ManagedModels is the content of a managed block.
type ManagedModels struct {
	DefaultModel  string
	Rules         []ModelRule
	ContextWindow int64 // 0: omit
}

// Render returns the block body, one line per element, indented by indent.
func (m ManagedModels) Render(indent string) []string {
	lines := []string{"default_model: " + m.DefaultModel}
	if m.ContextWindow > 0 {
		lines = append(lines, "context_window: "+strconv.FormatInt(m.ContextWindow, 10))
	}
	lines = append(lines, "models: [")
	for _, r := range m.Rules {
		lines = append(lines,
			"    {",
			"        match: "+r.Match,
			"        model: "+r.Model,
			"    }")
	}
	lines = append(lines, "]")
	for i := range lines {
		lines[i] = indent + lines[i]
	}
	return lines
}

// ManagedMarkers returns the marker lines to paste into a profile by hand.
func ManagedMarkers(profile string) (begin, end string) {
	return managedBegin + profile, managedEnd + profile
}

// ApplyManaged replaces the managed block of profile in the config file at
// path. It fails without touching the file when the markers are missing.
func ApplyManaged(path, profile string, m ManagedModels) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := ReplaceManaged(string(data), profile, m)
	if err != nil {
		return err
	}
	return securefile.WriteFile(path, []byte(out))
}

// ReplaceManaged is ApplyManaged on text. Line endings of the input are kept.
func ReplaceManaged(text, profile string, m ManagedModels) (string, error) {
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
	}
	lines := strings.Split(text, nl)
	begin, end := -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case t == strings.TrimSpace(managedBegin+profile):
			begin = i
		case t == strings.TrimSpace(managedEnd+profile):
			end = i
		}
	}
	if begin < 0 || end < 0 || end < begin {
		b, e := ManagedMarkers(profile)
		return "", fmt.Errorf("profile %q has no managed block: add the lines %q and %q inside the profile around "+
			"default_model/models (ccgp rewrites only what is between them)", profile, b, e)
	}
	indent := lines[begin][:len(lines[begin])-len(strings.TrimLeft(lines[begin], " \t"))]
	body := m.Render(indent)

	next := make([]string, 0, len(lines)-(end-begin-1)+len(body))
	next = append(next, lines[:begin+1]...)
	next = append(next, body...)
	next = append(next, lines[end:]...)
	return strings.Join(next, nl), nil
}

// HasManaged reports whether the profile has a managed block in text.
func HasManaged(text, profile string) bool {
	_, err := ReplaceManaged(text, profile, ManagedModels{})
	return err == nil
}

// AddManaged inserts an empty managed block into profile's object so that
// ReplaceManaged can fill it. Existing default_model, context_window and
// models entries of that profile are removed (the block replaces them); all
// other lines stay. Fails when the profile object cannot be located.
func AddManaged(text, profile string) (string, error) {
	nl := "\n"
	if strings.Contains(text, "\r\n") {
		nl = "\r\n"
	}
	lines := strings.Split(text, nl)
	indentOf := func(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

	nameAt := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "name: "+profile {
			// A provider may share the name: require a "provider:" line in the object.
			if o, c, ok := objectAround(lines, i); ok && objectHasKey(lines[o+1:c], "provider:") {
				nameAt = i
				break
			}
		}
	}
	if nameAt < 0 {
		return "", fmt.Errorf("profile %q not found in the config", profile)
	}
	open, closeAt, _ := objectAround(lines, nameAt)
	keyIndent := indentOf(lines[nameAt])

	drop := map[int]bool{}
	first := -1
	for i := open + 1; i < closeAt; i++ {
		if indentOf(lines[i]) != keyIndent {
			continue
		}
		t := strings.TrimSpace(lines[i])
		end := i
		switch {
		case strings.HasPrefix(t, "default_model:"), strings.HasPrefix(t, "context_window:"):
		case t == "models: [":
			for end = i + 1; end < closeAt; end++ {
				if indentOf(lines[end]) == keyIndent && strings.TrimSpace(lines[end]) == "]" {
					break
				}
			}
			if end >= closeAt {
				return "", fmt.Errorf("profile %q: unterminated models list", profile)
			}
		default:
			continue
		}
		if first < 0 {
			first = i
		}
		for j := i; j <= end; j++ {
			drop[j] = true
		}
		i = end
	}
	if first < 0 {
		first = closeAt
	}

	b, e := ManagedMarkers(profile)
	out := make([]string, 0, len(lines)+2)
	for i, l := range lines {
		if i == first {
			out = append(out, keyIndent+b, keyIndent+e)
		}
		if !drop[i] {
			out = append(out, l)
		}
	}
	return strings.Join(out, nl), nil
}

// objectAround finds the { ... } lines enclosing line i (braces alone on their lines).
func objectAround(lines []string, i int) (open, closeAt int, ok bool) {
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

func objectHasKey(lines []string, key string) bool {
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), key) {
			return true
		}
	}
	return false
}

// EnsureManaged adds a managed block to profile in the file at path when it
// has none. It reports whether the file was changed.
func EnsureManaged(path, profile string) (added bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if HasManaged(string(data), profile) {
		return false, nil
	}
	out, err := AddManaged(string(data), profile)
	if err != nil {
		return false, err
	}
	return true, securefile.WriteFile(path, []byte(out))
}
