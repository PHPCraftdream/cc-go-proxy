package config

import (
	"strings"
	"testing"
)

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
