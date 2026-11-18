package cfgtext

import (
	"testing"
)

func TestIsListHeader(t *testing.T) {
	trueCases := []string{
		"profiles: [",
		"profiles:  [",
		"profiles : [",
		"\tprofiles: [\r",
		"    profiles: [",
	}
	for _, line := range trueCases {
		if !IsListHeader(line, "profiles") {
			t.Errorf("IsListHeader(%q, profiles) = false, want true", line)
		}
	}
	falseCases := []string{
		"profiles: []",
		"profiles [",
		"profilesX: [",
		"# profiles: [",
		"## profiles: [",
		"profiles: [ x",
		"",
	}
	for _, line := range falseCases {
		if IsListHeader(line, "profiles") {
			t.Errorf("IsListHeader(%q, profiles) = true, want false", line)
		}
	}
	if IsListHeader("profiles: [", "providers") {
		t.Error(`IsListHeader("profiles: [", providers) = true, want false`)
	}
}

func TestKeyValue(t *testing.T) {
	tests := []struct {
		line, key, val string
		ok             bool
	}{
		{"listen.host: 1", "listen.host", "1", true},
		{"name:  work  ", "name", "work", true},
		{"provider :  p1", "provider", "p1", true},
		{"provider\t: p1", "provider", "p1", true},
		{"## comment: x", "", "", false},
		{"no colon here", "", "", false},
	}
	for _, tt := range tests {
		k, v, ok := KeyValue(tt.line)
		if ok != tt.ok || k != tt.key || v != tt.val {
			t.Errorf("KeyValue(%q) = %q, %q, %v; want %q, %q, %v", tt.line, k, v, ok, tt.key, tt.val, tt.ok)
		}
	}
}

func TestDominantCRLF(t *testing.T) {
	crlf := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "x\r"
		}
		return out
	}
	lf := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "x"
		}
		return out
	}
	// 7 CRLF lines + 6 LF lines, all newline-terminated (split's last element
	// is the empty tail after the final "\n").
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"7 CRLF + 6 LF + trailing newline", append(append(crlf(7), lf(6)...), ""), true},
		{"6 CRLF + 7 LF + trailing newline", append(append(crlf(6), lf(7)...), ""), false},
		{"tie", append(append(crlf(6), lf(6)...), ""), false},
		{"single line no newline", []string{"x"}, false},
	}
	for _, tt := range tests {
		if got := DominantCRLF(tt.lines); got != tt.want {
			t.Errorf("%s: DominantCRLF = %v, want %v", tt.name, got, tt.want)
		}
	}
}
