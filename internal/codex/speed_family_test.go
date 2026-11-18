package codex

import "testing"

func TestModelFamily(t *testing.T) {
	for slug, want := range map[string]string{"gpt-6-astra": "astra", "gpt-6.2-astra": "astra", "gpt-9.1-astra": "astra", "gpt-6.2-sol": "sol", "gpt-6.2-astra-extra": "", "other-astra": ""} {
		if got := ModelFamily(slug); got != want {
			t.Errorf("ModelFamily(%q) = %q, want %q", slug, got, want)
		}
	}
}
