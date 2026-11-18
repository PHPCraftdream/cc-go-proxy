package config

import (
	"strings"
	"testing"
)

const speedBase = "providers: [\n    {\n        name: codex\n        type: codex\n    }\n]\nprofiles: [\n    {\n        name: one\n        provider: codex\n    }\n]\n"

func speedRulesOf(t *testing.T, text string) map[string]string {
	t.Helper()
	fc, err := loadFile(writeKtav(t, text))
	if err != nil {
		t.Fatalf("config does not load: %v\n%s", err, text)
	}
	return fc.Profiles[0].SpeedRules
}

func TestSetProfileSpeedTextReplacesAValue(t *testing.T) {
	text, err := SetProfileSpeedText(speedBase, "one", "astra", SpeedFast)
	if err != nil {
		t.Fatal(err)
	}
	if text, err = SetProfileSpeedText(text, "one", "astra", SpeedStandard); err != nil {
		t.Fatal(err)
	}
	rules := speedRulesOf(t, text)
	if len(rules) != 1 || rules["astra"] != SpeedStandard || strings.Count(text, "astra:") != 1 {
		t.Errorf("rules %v\n%s", rules, text)
	}
}

func TestSetProfileSpeedTextKeepsLineEndings(t *testing.T) {
	crlf := strings.ReplaceAll(speedBase, "\n", "\r\n")
	text, err := SetProfileSpeedText(crlf, "one", "sol", SpeedFast)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(text, "\n") != strings.Count(text, "\r\n") {
		t.Errorf("a bare LF was written:\n%q", text)
	}
	if rules := speedRulesOf(t, text); rules["sol"] != SpeedFast {
		t.Errorf("rules %v", rules)
	}
	text, err = SetProfileSpeedText(text, "one", "sol", "")
	if err != nil || strings.Contains(text, "speed_rules") || strings.Count(text, "\n") != strings.Count(text, "\r\n") {
		t.Errorf("removal: %q, %v", text, err)
	}
}

func TestSetProfileSpeedTextRemovalKeepsOthersAndComments(t *testing.T) {
	text := speedBase
	for _, family := range []string{"astra", "sol"} {
		var err error
		if text, err = SetProfileSpeedText(text, "one", family, SpeedFast); err != nil {
			t.Fatal(err)
		}
	}
	text, err := SetProfileSpeedText(text, "one", "astra", "")
	if err != nil {
		t.Fatal(err)
	}
	if rules := speedRulesOf(t, text); len(rules) != 1 || rules["sol"] != SpeedFast {
		t.Fatalf("rules %v\n%s", rules, text)
	}
	// A comment inside the block outlives the block when the last rule goes.
	text = strings.Replace(text, "sol: fast", "## keep me\n            sol: fast", 1)
	text, err = SetProfileSpeedText(text, "one", "sol", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "## keep me") || strings.Contains(text, "speed_rules") {
		t.Errorf("after the last removal:\n%s", text)
	}
	if rules := speedRulesOf(t, text); len(rules) != 0 {
		t.Errorf("rules %v", rules)
	}
}

func TestSetProfileSpeedTextRefusals(t *testing.T) {
	inline := strings.Replace(speedBase, "        provider: codex\n    }\n]\n", "        provider: codex\n        speed_rules: {}\n    }\n]\n", 1)
	if _, err := SetProfileSpeedText(inline, "one", "astra", SpeedFast); err == nil || !strings.Contains(err.Error(), "multiline object") {
		t.Errorf("inline block: %v", err)
	}
	if _, err := SetProfileSpeedText(speedBase, "missing", "astra", SpeedFast); err == nil {
		t.Error("unknown profile accepted")
	}
	for _, family := range []string{"", "Astra", "as-tra", "astra1", "a b"} {
		if _, err := SetProfileSpeedText(speedBase, "one", family, SpeedFast); err == nil {
			t.Errorf("family %q accepted", family)
		}
	}
	if out, err := SetProfileSpeedText(speedBase, "one", "astra", ""); err != nil || out != speedBase {
		t.Errorf("removing a rule that is not there changed the text: %v", err)
	}
}

func TestSpeedRuleLoadValidation(t *testing.T) {
	for name, rule := range map[string]string{"upper-case family": "Astra: fast", "unknown speed": "astra: turbo"} {
		text := strings.Replace(speedBase, "        provider: codex\n    }\n]\n", "        provider: codex\n        speed_rules: {\n            "+rule+"\n        }\n    }\n]\n", 1)
		if _, err := loadFile(writeKtav(t, text)); err == nil || !strings.Contains(err.Error(), "invalid speed rule") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSpeedRulesNilProfile(t *testing.T) {
	var p *Profile
	if p.SpeedFor("astra") != "" || !p.EffectiveFast("astra", true) || p.EffectiveFast("astra", false) {
		t.Error("a missing profile must inherit the client's request")
	}
}
