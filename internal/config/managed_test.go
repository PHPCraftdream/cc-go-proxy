package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var sampleManaged = ManagedModels{
	Rules: []ModelRule{
		{Match: "*fable*", Model: "gpt-6.1-astra", ContextWindow: 1000000},
		{Match: "*opus*", Model: "gpt-6.1-sol", ContextWindow: 500000},
		{Match: "*sonnet*", Model: "gpt-6.1-luna", ContextWindow: 800000},
		{Match: "*haiku*", Model: "gpt-6-luna", ContextWindow: 272000},
		{Match: "*", Model: "gpt-6.1-luna", ContextWindow: 800000},
	},
}

func TestReplaceManagedOnTemplate(t *testing.T) {
	text := string(DefaultTemplate())
	out, err := ReplaceManaged(text, "codex", sampleManaged)
	if err != nil {
		t.Fatal(err)
	}
	// The result still loads and carries the new mapping.
	fc, err := loadFile(writeKtav(t, out))
	if err != nil {
		t.Fatalf("result does not load: %v\n%s", err, out)
	}
	var pf Profile
	for _, p := range fc.Profiles {
		if p.Name == "codex" {
			pf = p
		}
	}
	if len(pf.Models) != 5 || pf.Models[3].Match != "*haiku*" || pf.Models[3].Model != "gpt-6-luna" ||
		pf.Models[3].ContextWindow != 272000 || pf.Models[4].Match != "*" || pf.DefaultModel != "" {
		t.Errorf("codex profile = %+v", pf)
	}
	// Every rule carries its own window; the smallest one is what the launcher uses.
	if got := pf.EffectiveContextWindow(); got != 272000 {
		t.Errorf("EffectiveContextWindow = %d, want 272000", got)
	}
	// Outside the markers nothing changes: strip the block from both and compare.
	cut := func(s string) string {
		i := strings.Index(s, "ccgp:managed-models begin codex")
		j := strings.Index(s, "ccgp:managed-models end codex")
		return s[:i] + s[j:]
	}
	if cut(text) != cut(out) {
		t.Error("text outside the managed block changed")
	}
}

func TestReplaceManagedKeepsCRLFAndIndent(t *testing.T) {
	text := "a: 1\r\n    ## ccgp:managed-models begin p\r\n    old: x\r\n    ## ccgp:managed-models end p\r\nb: 2\r\n"
	out, err := ReplaceManaged(text, "p", ManagedModels{Rules: []ModelRule{{Match: "*", Model: "m", ContextWindow: 123}}})
	if err != nil {
		t.Fatal(err)
	}
	want := "a: 1\r\n    ## ccgp:managed-models begin p\r\n" +
		"    models: [\r\n        {\r\n            match: *\r\n            model: m\r\n            context_window: 123\r\n        }\r\n    ]\r\n" +
		"    ## ccgp:managed-models end p\r\nb: 2\r\n"
	if out != want {
		t.Errorf("got %q\nwant %q", out, want)
	}
}

func TestReplaceManagedMissingMarkers(t *testing.T) {
	_, err := ReplaceManaged("name: x\n", "codex", sampleManaged)
	if err == nil || !strings.Contains(err.Error(), "ccgp:managed-models begin codex") {
		t.Fatalf("err = %v", err)
	}
	if HasManaged("name: x\n", "codex") {
		t.Error("HasManaged true without markers")
	}
	// Markers of another profile do not count.
	if HasManaged(string(DefaultTemplate()), "codex_api") {
		t.Error("HasManaged true for a profile without a block")
	}
	if !HasManaged(string(DefaultTemplate()), "codex") {
		t.Error("template codex profile has no managed block")
	}
}

func TestApplyManagedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, DefaultTemplate(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyManaged(p, "codex", sampleManaged); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFile(p); err != nil {
		t.Fatalf("file does not load: %v", err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "model: gpt-6.1-sol") {
		t.Error("mapping not written")
	}
	// A failed apply leaves the file as it was.
	before, _ := os.ReadFile(p)
	if err := ApplyManaged(p, "nope", sampleManaged); err == nil {
		t.Fatal("apply for unknown profile succeeded")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Error("file changed after failed apply")
	}
}

func TestProfileCompactValidation(t *testing.T) {
	base := "providers: [\n    {\n        name: p\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: w\n        provider: p\n"
	for _, tt := range []struct {
		extra string
		bad   bool
	}{
		{"        context_window: 800000\n        compact_percent: 70\n", false},
		{"        compact_percent: 101\n", true},
		{"        context_window: -5\n", true},
	} {
		_, err := loadFile(writeKtav(t, base+tt.extra+"    }\n]\n"))
		if (err != nil) != tt.bad {
			t.Errorf("%q: err = %v, bad = %v", tt.extra, err, tt.bad)
		}
	}
}

const oldStyleConfig = `listen.port: 60093

providers: [
    {
        name: codex
        type: codex
    }
]

profiles: [
    {
        name: codex
        provider: codex
        ## keep me
        default_model: gpt-5
        models: [
            {
                match: *haiku*
                model: gpt-5-mini
            }
        ]
        compact_percent: 70
    }
    {
        name: other
        provider: codex
        default_model: keep
    }
]
`

func TestEnsureManagedMigratesOldProfile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, []byte(oldStyleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := EnsureManaged(p, "codex")
	if err != nil || !added {
		t.Fatalf("EnsureManaged = %v, %v", added, err)
	}
	if err := ApplyManaged(p, "codex", sampleManaged); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	text := string(b)
	for _, keep := range []string{"## keep me", "compact_percent: 70", "name: other", "default_model: keep"} {
		if !strings.Contains(text, keep) {
			t.Errorf("lost %q:\n%s", keep, text)
		}
	}
	if strings.Contains(text, "gpt-5-mini") || strings.Contains(text, "default_model: gpt-5\n") {
		t.Errorf("old mapping survived:\n%s", text)
	}
	fc, err := loadFile(p)
	if err != nil {
		t.Fatalf("result does not load: %v\n%s", err, text)
	}
	if fc.Profiles[0].DefaultModel != "" || len(fc.Profiles[0].Models) != 5 ||
		fc.Profiles[0].CompactPercent != 70 || fc.Profiles[1].DefaultModel != "keep" {
		t.Errorf("profiles = %+v", fc.Profiles)
	}
	// Idempotent once the block exists.
	if added, err := EnsureManaged(p, "codex"); err != nil || added {
		t.Errorf("second EnsureManaged = %v, %v", added, err)
	}
}

func TestEnsureManagedProfileWithoutMapping(t *testing.T) {
	text := "profiles: [\n    {\n        name: bare\n        provider: p\n    }\n]\n"
	out, err := AddManaged(text, "bare")
	if err != nil {
		t.Fatal(err)
	}
	if !HasManaged(out, "bare") {
		t.Errorf("no block:\n%s", out)
	}
	if _, err := AddManaged(text, "missing"); err == nil {
		t.Error("unknown profile accepted")
	}
}
