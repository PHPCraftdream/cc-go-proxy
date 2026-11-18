package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DefaultCodexClientVersion is the Codex CLI version ccgp presents to the Codex
// backend until config.ktav holds its own `codex.client_version` (written there
// on first use). The backend lists a model generation only to a client at
// least as new as the one that brought it (gpt-6.2-* needs 0.169.0).
const DefaultCodexClientVersion = "0.169.0"

var (
	clientVersionRe   = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	clientVersionLine = regexp.MustCompile(`^codex\.client_version\s*::?`)
	codexBlockLine    = regexp.MustCompile(`^codex\s*:`)
)

// ValidClientVersion reports whether v is MAJOR.MINOR.PATCH.
func ValidClientVersion(v string) bool { return clientVersionRe.MatchString(v) }

// CompareClientVersions compares two MAJOR.MINOR.PATCH versions (-1, 0, 1).
func CompareClientVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// CodexClientVersion is the version sent to the Codex backend: the config's,
// else the built-in default (env-only mode, or a config not yet seeded).
func (c *Config) CodexClientVersion() string {
	if c != nil && c.CodexClientVersionSet != "" {
		return c.CodexClientVersionSet
	}
	return DefaultCodexClientVersion
}

// HasCodexProvider reports whether any provider is of type codex.
func (c *Config) HasCodexProvider() bool {
	for _, p := range c.Providers {
		if p.Type == ProviderTypeCodex {
			return true
		}
	}
	return false
}

// CodexClientVersionText returns the `codex.client_version` value in config text.
func CodexClientVersionText(text string) (string, bool) {
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimRight(l, "\r")
		if loc := clientVersionLine.FindStringIndex(l); loc != nil {
			return strings.TrimSpace(l[loc[1]:]), true
		}
	}
	return "", false
}

// SetCodexClientVersionText writes `codex.client_version:: v` into config text:
// the line is rewritten, or added after the listen and connections settings.
// Only raise: a v not newer than the current value leaves the text unchanged.
func SetCodexClientVersionText(text, v string) (string, error) {
	if !ValidClientVersion(v) {
		return "", fmt.Errorf("invalid Codex client version %q (MAJOR.MINOR.PATCH)", v)
	}
	if cur, ok := CodexClientVersionText(text); ok && ValidClientVersion(cur) && CompareClientVersions(v, cur) <= 0 {
		return text, nil
	}
	lines := strings.Split(text, "\n")
	// `::` keeps the value a string whatever it looks like.
	line := "codex.client_version:: " + v
	last := -1
	for i, l := range lines {
		trimmed := strings.TrimRight(l, "\r")
		switch {
		case clientVersionLine.MatchString(trimmed):
			lines[i] = line + l[len(trimmed):]
			return strings.Join(lines, "\n"), nil
		case codexBlockLine.MatchString(trimmed):
			return "", fmt.Errorf("config has a `codex:` block; set client_version there by hand")
		case strings.HasPrefix(trimmed, "listen."), strings.HasPrefix(trimmed, "connections."):
			last = i
		}
	}
	if strings.HasSuffix(lines[0], "\r") {
		line += "\r"
	}
	at := last + 1
	if last < 0 {
		for at < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[at]), "#") {
			at++
		}
	}
	out := append([]string{}, lines[:at]...)
	out = append(out, line)
	return strings.Join(append(out, lines[at:]...), "\n"), nil
}

// RaiseCodexClientVersion raises `codex.client_version` in the config file to v
// (never lowers it). It returns the version the file held before ("" when it
// had none) and whether the file changed.
func RaiseCodexClientVersion(path, v string) (from string, changed bool, err error) {
	backup, err := EditFile(path, func(text string) (string, error) {
		from, _ = CodexClientVersionText(text)
		return SetCodexClientVersionText(text, v)
	})
	return from, backup != "", err
}

// SeedCodexClientVersion writes the built-in default into a config that has a
// codex provider but no `codex.client_version` yet; from then on the config's
// value is the one used. A no-op in env-only mode.
func SeedCodexClientVersion(cfg *Config) (bool, error) {
	if cfg == nil || cfg.IsEnvMode() || cfg.CodexClientVersionSet != "" || !cfg.HasCodexProvider() {
		return false, nil
	}
	_, changed, err := RaiseCodexClientVersion(cfg.ConfigFile, DefaultCodexClientVersion)
	if err == nil {
		cfg.CodexClientVersionSet = DefaultCodexClientVersion
	}
	return changed, err
}
