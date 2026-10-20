// Package identity corrects the model identity that Claude Code writes into
// its system prompt, so a model served through ccgp is told what it really is
// instead of repeating the Claude name it was addressed by.
package identity

import (
	"regexp"
	"strings"
)

var (
	// "You are powered by the model named Sonnet 5.5. The exact model ID is claude-sonnet-5-5."
	namedWithID = regexp.MustCompile(`You are powered by the model named [^\n]*?\. The exact model ID is \S+?\.(?:\s|$)`)
	// "You are powered by the model named Sonnet 5.5." (no ID sentence)
	namedOnly = regexp.MustCompile(`You are powered by the model named [^\n]*?\.(?:\s|$)`)
	// "Assistant knowledge cutoff is June 2026." describes the Claude model.
	cutoff = regexp.MustCompile(`[ \t]*Assistant knowledge cutoff is [^\n.]+\.`)
)

// Rewrite replaces the model identity sentences in a Claude Code system prompt
// with the backend model that actually answers. requested is the Claude model
// name Claude Code asked for; text without those sentences is returned unchanged.
func Rewrite(text, requested, backend string) string {
	if backend == "" || !strings.Contains(text, "You are powered by the model") {
		return text
	}
	statement := "You are powered by the model named " + backend + ", reached through a Claude Code gateway. " +
		"The exact model ID is " + backend + ". " +
		"Claude Code shows this session as \"" + requested + "\"; that is only a display label, not your identity."
	if requested == "" {
		statement = "You are powered by the model named " + backend + ", reached through a Claude Code gateway. " +
			"The exact model ID is " + backend + "."
	}

	repl := func(m string) string {
		// Keep the whitespace that followed the original sentence.
		if trimmed := strings.TrimRight(m, " \t\r\n"); trimmed != m {
			return statement + m[len(trimmed):]
		}
		return statement
	}
	out := namedWithID.ReplaceAllStringFunc(text, repl)
	if out == text {
		out = namedOnly.ReplaceAllStringFunc(text, repl)
	}
	return cutoff.ReplaceAllString(out, "")
}

const marker = "You are powered by the model"

// RewriteContent applies Rewrite to the text of a Claude message content value
// (a string, or a list of blocks): only top-level text blocks are touched, so
// tool results and other structured content are passed through unchanged.
// It returns content itself when nothing needs changing.
func RewriteContent(content any, requested, backend string) any {
	switch c := content.(type) {
	case string:
		return Rewrite(c, requested, backend)
	case []any:
		for _, b := range c {
			m, ok := b.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if s, ok := m["text"].(string); ok {
				m["text"] = Rewrite(s, requested, backend)
			}
		}
	}
	return content
}

// Mentions reports whether raw JSON text may contain the identity sentence.
func Mentions(raw []byte) bool { return strings.Contains(string(raw), marker) }
