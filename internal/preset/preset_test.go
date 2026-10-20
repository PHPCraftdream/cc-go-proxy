package preset

import "testing"

func TestForBaseURL(t *testing.T) {
	for _, u := range []string{ZAIBaseURL, "https://API.z.ai/api/anthropic/", "https://open.bigmodel.cn/api/anthropic"} {
		if p, ok := ForBaseURL(u); !ok || p.Name != "zai" {
			t.Errorf("ForBaseURL(%q) = %v, %v", u, p.Name, ok)
		}
	}
	for _, u := range []string{"", "https://api.openai.com/v1", "https://example.com/z.ai", "::not a url"} {
		if _, ok := ForBaseURL(u); ok {
			t.Errorf("ForBaseURL(%q) matched", u)
		}
	}
}

// Fable gets the full model, Opus and everything below the fast one; every rule
// states its window and the list ends with a catch-all.
func TestZAIMapping(t *testing.T) {
	want := map[string]string{
		"*fable*": "glm-5.3", "*opus*": "glm-5.3-flash", "*sonnet*": "glm-5.3-flash",
		"*haiku*": "glm-5.3-flash", "*": "glm-5.3-flash",
	}
	if len(ZAI.Rules) != len(want) {
		t.Fatalf("rules = %+v", ZAI.Rules)
	}
	for _, r := range ZAI.Rules {
		if want[r.Match] != r.Model || r.ContextWindow != 1_000_000 {
			t.Errorf("rule %+v, want model %q with a 1M window", r, want[r.Match])
		}
	}
	if last := ZAI.Rules[len(ZAI.Rules)-1]; last.Match != "*" {
		t.Errorf("last rule %q is not the catch-all", last.Match)
	}
}
