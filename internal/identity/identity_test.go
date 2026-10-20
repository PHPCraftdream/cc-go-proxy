package identity

import (
	"strings"
	"testing"
)

func TestRewriteReplacesModelIdentity(t *testing.T) {
	in := "intro line\n You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5. Assistant knowledge cutoff is June 2026.\nnext section"
	out := Rewrite(in, "claude-sonnet-5-5", "gpt-6-luna")

	for _, gone := range []string{"Sonnet 5.5.", "named Sonnet", "knowledge cutoff", "June 2026"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q survived:\n%s", gone, out)
		}
	}
	for _, want := range []string{
		"You are powered by the model named gpt-6-luna",
		"The exact model ID is gpt-6-luna.",
		`shows this session as "claude-sonnet-5-5"`,
		"intro line", "next section",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestRewriteVariants(t *testing.T) {
	cases := map[string]string{
		"ID with suffix": "You are powered by the model named Opus 5.5 (1M context). The exact model ID is claude-opus-5-5[1m]. More.",
		"name only":      "You are powered by the model named Haiku 5.5. Other text.",
		"end of text":    "You are powered by the model named Fable 5.1. The exact model ID is claude-fable-5-1.",
		"newline after":  "You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5.\nNext.",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := Rewrite(in, "claude-x", "gpt-6-sol")
			if !strings.Contains(out, "named gpt-6-sol") || strings.Contains(out, "claude-opus") ||
				strings.Contains(out, "claude-sonnet") || strings.Contains(out, "claude-fable") || strings.Contains(out, "Haiku 5.5") {
				t.Errorf("not rewritten:\n%s", out)
			}
			if strings.Contains(in, "More.") && !strings.Contains(out, "More.") {
				t.Error("text after the sentence lost")
			}
			if strings.Contains(in, "Next.") && !strings.Contains(out, "\nNext.") {
				t.Errorf("newline before the next line lost:\n%q", out)
			}
		})
	}
}

func TestRewriteLeavesOtherTextAlone(t *testing.T) {
	for _, in := range []string{
		"",
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"The most recent Claude models are the Claude 5 family.",
	} {
		if out := Rewrite(in, "claude-x", "gpt-6-sol"); out != in {
			t.Errorf("changed %q -> %q", in, out)
		}
	}
	in := "You are powered by the model named Sonnet 5.5."
	if out := Rewrite(in, "claude-x", ""); out != in {
		t.Errorf("empty backend must not rewrite: %q", out)
	}
}

func TestRewriteWithoutRequestedName(t *testing.T) {
	out := Rewrite("You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5.", "", "gpt-6-luna")
	if strings.Contains(out, "display label") || !strings.Contains(out, "gpt-6-luna") {
		t.Errorf("out = %q", out)
	}
}

func TestRewriteContent(t *testing.T) {
	text := "You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5."
	// Plain string.
	if got, _ := RewriteContent(text, "c", "gpt-6-luna").(string); !strings.Contains(got, "named gpt-6-luna") {
		t.Errorf("string content: %q", got)
	}
	// Blocks: text blocks rewritten, others untouched.
	blocks := []any{
		map[string]any{"type": "text", "text": text},
		map[string]any{"type": "tool_result", "content": text},
		"stray",
	}
	out := RewriteContent(blocks, "c", "gpt-6-luna").([]any)
	if !strings.Contains(out[0].(map[string]any)["text"].(string), "named gpt-6-luna") {
		t.Errorf("text block not rewritten: %v", out[0])
	}
	if out[1].(map[string]any)["content"] != text {
		t.Errorf("tool_result altered: %v", out[1])
	}
	// Other types pass through.
	if got := RewriteContent(42, "c", "b"); got != 42 {
		t.Errorf("non-content value changed: %v", got)
	}
	if !Mentions([]byte(`{"a":"You are powered by the model named X"}`)) || Mentions([]byte(`{}`)) {
		t.Error("Mentions wrong")
	}
}
