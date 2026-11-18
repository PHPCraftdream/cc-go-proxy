package cfgedit

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

var sampleManaged = ManagedModels{
	Rules: []config.ModelRule{
		{Match: "*fable*", Model: "gpt-6.1-astra", ContextWindow: 1000000},
		{Match: "*opus*", Model: "gpt-6.1-sol", ContextWindow: 500000},
		{Match: "*sonnet*", Model: "gpt-6.1-luna", ContextWindow: 800000},
		{Match: "*haiku*", Model: "gpt-6-luna", ContextWindow: 272000},
		{Match: "*", Model: "gpt-6.1-luna", ContextWindow: 800000},
	},
}

func TestReplaceManagedOnTemplate(t *testing.T) {
	text := string(config.DefaultTemplate())
	out, err := ReplaceManaged(text, "codex", sampleManaged)
	if err != nil {
		t.Fatal(err)
	}
	// The result still loads and carries the new mapping.
	fc, err := config.LoadFile(writeKtav(t, out))
	if err != nil {
		t.Fatalf("result does not load: %v\n%s", err, out)
	}
	var pf config.Profile
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
	out, err := ReplaceManaged(text, "p", ManagedModels{Rules: []config.ModelRule{{Match: "*", Model: "m", ContextWindow: 123}}})
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
	if HasManaged(string(config.DefaultTemplate()), "codex_api") {
		t.Error("HasManaged true for a profile without a block")
	}
	if !HasManaged(string(config.DefaultTemplate()), "codex") {
		t.Error("template codex profile has no managed block")
	}
}

func TestApplyManagedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, config.DefaultTemplate(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyManaged(p, "codex", sampleManaged); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadFile(p); err != nil {
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
		_, err := config.LoadFile(writeKtav(t, base+tt.extra+"    }\n]\n"))
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
	fc, err := config.LoadFile(p)
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

func TestAddManagedOddSpacingLoaderValidated(t *testing.T) {
	tests := []struct {
		name    string
		profile string
	}{
		{
			name:    "name value spacing",
			profile: "        name:  work\n        provider: p1\n",
		},
		{
			name:    "provider key spacing",
			profile: "        name: work\n        provider : p1\n",
		},
		{
			name:    "mapping key spacing",
			profile: "        name: work\n        provider: p1\n        default_model : old-model\n        context_window : 123456\n",
		},
		{
			name: "models list spacing",
			profile: "        name: work\n        provider: p1\n" +
				"        default_model : old-model\n        context_window : 123456\n" +
				"        models : [\n            {\n                match: *old*\n" +
				"                model: old-rule-model\n            }\n        ]\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := "providers: [\n    {\n        name: p1\n        type: codex_api\n        ## provider comment\n    }\n" +
				"    {\n        name: p2\n        type: codex\n        ## other provider comment\n    }\n]\n" +
				"profiles: [\n    {\n        ## profile comment\n" + tt.profile + "    }\n" +
				"    {\n        name: other\n        provider: p2\n        default_model: preserve\n    }\n]\n"
			if _, err := config.LoadFile(writeKtav(t, text)); err != nil {
				t.Fatalf("input fixture does not load: %v\n%s", err, text)
			}
			out, err := AddManaged(text, "work")
			if err != nil {
				t.Fatal(err)
			}
			out, err = ReplaceManaged(out, "work", sampleManaged)
			if err != nil {
				t.Fatal(err)
			}
			fc, err := config.LoadFile(writeKtav(t, out))
			if err != nil {
				t.Fatalf("result does not load: %v\n%s", err, out)
			}
			work := profilePtr(fc, "work")
			if work == nil || !HasManaged(out, "work") {
				t.Fatalf("managed block not added to work:\n%s", out)
			}
			if work.DefaultModel != "" || work.ContextWindow != 0 || !reflect.DeepEqual(work.Models, sampleManaged.Rules) {
				t.Errorf("work profile mapping = %+v, want managed rules %+v", work, sampleManaged.Rules)
			}
			for _, obsolete := range []string{"default_model : old-model", "context_window : 123456", "models : [", "old-rule-model"} {
				if strings.Contains(out, obsolete) {
					t.Errorf("obsolete mapping %q survived:\n%s", obsolete, out)
				}
			}
			other := profilePtr(fc, "other")
			if other == nil || other.Provider != "p2" || other.DefaultModel != "preserve" {
				t.Errorf("unaffected other profile = %+v", other)
			}
			if len(fc.Providers) != 2 || fc.Providers[0].Name != "p1" || fc.Providers[1].Name != "p2" || fc.Providers[1].Type != "codex" {
				t.Errorf("providers were not preserved: %+v", fc.Providers)
			}
			for _, comment := range []string{"## provider comment", "## other provider comment", "## profile comment"} {
				if !strings.Contains(out, comment) {
					t.Errorf("lost comment %q:\n%s", comment, out)
				}
			}
		})
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

func TestSetProfileKeyText(t *testing.T) {
	got, err := SetProfileKeyText(peakBase, "zai", "default_model", "glm-5.3")
	want := strings.Replace(peakBase, "        name: zai\n        provider: zai\n", "        name: zai\n        provider: zai\n        default_model: glm-5.3\n", 1)
	if err != nil || got != want {
		t.Fatalf("add: %v\n%s\nwant\n%s", err, got, want)
	}
	pf := profilePtr(mustLoad(t, got), "zai")
	if pf.DefaultModel != "glm-5.3" {
		t.Errorf("loaded back: %+v", pf)
	}

	// A second key goes after the provider line too; numbers read back as numbers.
	both, err := SetProfileKeyText(got, "zai", "context_window", "1000000")
	if err != nil {
		t.Fatal(err)
	}
	both, err = SetProfileKeyText(both, "zai", "compact_percent", "70")
	if err != nil {
		t.Fatal(err)
	}
	pf = profilePtr(mustLoad(t, both), "zai")
	if pf.ContextWindow != 1000000 || pf.CompactPercent != 70 || pf.DefaultModel != "glm-5.3" {
		t.Errorf("loaded back: %+v", pf)
	}

	// Replacing keeps the place; removing every line gives the original text back.
	replaced, err := SetProfileKeyText(both, "zai", "default_model", "glm-5.3-flash")
	if err != nil || !strings.Contains(replaced, "        default_model: glm-5.3-flash\n") || strings.Contains(replaced, "default_model: glm-5.3\n") || strings.Count(replaced, "default_model:") != 1 {
		t.Fatalf("replace: %v\n%s", err, replaced)
	}
	back := replaced
	for _, k := range []string{"default_model", "context_window", "compact_percent"} {
		if back, err = SetProfileKeyText(back, "zai", k, ""); err != nil {
			t.Fatal(err)
		}
	}
	if back != peakBase {
		t.Errorf("after removing every key:\n%s", back)
	}
	if same, err := SetProfileKeyText(peakBase, "zai", "default_model", "  "); err != nil || same != peakBase {
		t.Errorf("removing a key that is not there changed the text: %v\n%s", err, same)
	}

	// Only the named profile changes.
	if !strings.Contains(got, "    {\n        name: codex\n        provider: codex\n    }\n") {
		t.Errorf("the other profile was touched:\n%s", got)
	}
}

func TestSetProfileKeyTextSwitchesTheProvider(t *testing.T) {
	got, err := SetProfileKeyText(peakBase, "zai", "provider", "codex")
	if err != nil || strings.Count(got, "provider: codex") != 2 || strings.Contains(got, "provider: zai") {
		t.Fatalf("switch: %v\n%s", err, got)
	}
	if pf := profilePtr(mustLoad(t, got), "zai"); pf.Provider != "codex" {
		t.Errorf("loaded back: %+v", pf)
	}
	if _, err := SetProfileKeyText(peakBase, "zai", "provider", ""); err == nil {
		t.Error("a profile without a provider was accepted")
	}
}

func TestSetProfileKeyTextRejects(t *testing.T) {
	for name, call := range map[string]func() (string, error){
		"unknown profile": func() (string, error) { return SetProfileKeyText(peakBase, "nope", "default_model", "m") },
		"unknown key":     func() (string, error) { return SetProfileKeyText(peakBase, "zai", "models", "m") },
		"proxy has its own editor": func() (string, error) {
			return SetProfileKeyText(peakBase, "zai", "proxy", "none")
		},
		"two lines": func() (string, error) { return SetProfileKeyText(peakBase, "zai", "default_model", "a\nb") },
		"a provider cannot be emptied": func() (string, error) {
			return SetProfileKeyText(peakBase, "codex", "provider", "")
		},
	} {
		if got, err := call(); err == nil {
			t.Errorf("%s: accepted:\n%s", name, got)
		}
	}
}

func TestSetProviderKeyText(t *testing.T) {
	// base_url is replaced in place; effort_map and its inner keys are not touched.
	got, err := SetProviderKeyText(peakBase, "zai", "base_url", "https://example.internal/api")
	want := strings.Replace(peakBase, "base_url: https://api.z.ai/api/anthropic", "base_url: https://example.internal/api", 1)
	if err != nil || got != want {
		t.Fatalf("replace: %v\n%s", err, got)
	}
	// api_key_env is added after base_url, before the nested effort_map.
	keyed, err := SetProviderKeyText(peakBase, "zai", "api_key_env", "ZAI_KEY")
	want = strings.Replace(peakBase, "        base_url: https://api.z.ai/api/anthropic\n", "        base_url: https://api.z.ai/api/anthropic\n        api_key_env: ZAI_KEY\n", 1)
	if err != nil || keyed != want {
		t.Fatalf("add: %v\n%s", err, keyed)
	}
	if pr := providerPtr(mustLoad(t, keyed), "zai"); pr.APIKeyEnv != "ZAI_KEY" || pr.BaseURL != "https://api.z.ai/api/anthropic" {
		t.Errorf("loaded back: %+v", pr)
	}
	if back, err := SetProviderKeyText(keyed, "zai", "api_key_env", ""); err != nil || back != peakBase {
		t.Errorf("remove: %v\n%s", err, back)
	}
	// A provider with only name and type gets the key after its type.
	codex, err := SetProviderKeyText(peakBase, "codex", "api_key_env", "K")
	if err != nil || !strings.Contains(codex, "        name: codex\n        type: codex\n        api_key_env: K\n    }\n") {
		t.Errorf("codex: %v\n%s", err, codex)
	}
	if _, err := SetProviderKeyText(peakBase, "nope", "base_url", "x"); err == nil {
		t.Error("unknown provider accepted")
	}
	if _, err := SetProviderKeyText(peakBase, "zai", "type", "codex"); err == nil {
		t.Error("the type of a provider was changed")
	}
	if _, err := SetProviderKeyText("listen.port: 60093\n", "zai", "base_url", "x"); err == nil {
		t.Error("a config without providers was edited")
	}
}

func TestSetKeyTextKeepsCommentsAndLineEndings(t *testing.T) {
	base := strings.Replace(peakBase, "        provider: zai\n", "        provider: zai\n        ## default_model: kept as a comment\n", 1)
	got, err := SetProfileKeyText(base, "zai", "default_model", "m")
	if err != nil || !strings.Contains(got, "        ## default_model: kept as a comment\n") || strings.Count(got, "default_model: m") != 1 {
		t.Fatalf("comment: %v\n%s", err, got)
	}
	crlf := strings.ReplaceAll(peakBase, "\n", "\r\n")
	set, err := SetProviderKeyText(crlf, "codex", "api_key_env", "K")
	if err != nil || !strings.Contains(set, "        type: codex\r\n        api_key_env: K\r\n") || strings.Contains(strings.ReplaceAll(set, "\r\n", ""), "\n") {
		t.Fatalf("CRLF add: %v\n%q", err, set)
	}
	repl, err := SetProviderKeyText(set, "codex", "api_key_env", "OTHER")
	if err != nil || !strings.Contains(repl, "        api_key_env: OTHER\r\n") {
		t.Errorf("CRLF replace: %v\n%q", err, repl)
	}
	mustLoad(t, repl)
}

func writeKtav(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.ktav")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const peakBase = `## config
providers: [
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
        effort_map: {
            low: low
        }
    }
    {
        name: codex
        type: codex
    }
]
profiles: [
    {
        name: zai
        provider: zai
    }
    {
        name: codex
        provider: codex
    }
]
`

const codexProviderKtav = "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\n"
