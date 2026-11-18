package config

import (
	"regexp"
	"strconv"
	"strings"
)

const (
	SpeedFast     = "fast"
	SpeedStandard = "standard"
)

func ValidSpeedFamily(family string) bool {
	if family == "" {
		return false
	}
	for _, r := range family {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// SpeedFor returns the configured speed for a logical family; empty means inherit.
func (p *Profile) SpeedFor(family string) string {
	if p == nil {
		return ""
	}
	return p.SpeedRules[family]
}

// EffectiveFast applies a family rule without changing the requested model.
func (p *Profile) EffectiveFast(family string, clientFast bool) bool {
	switch p.SpeedFor(family) {
	case SpeedFast:
		return true
	case SpeedStandard:
		return false
	default:
		return clientFast
	}
}

// DefaultCodexClientVersion is the Codex CLI version ccgp presents to the Codex
// backend until config.ktav holds its own `codex.client_version` (written there
// on first use). The backend lists a model generation only to a client at
// least as new as the one that brought it (gpt-6.2-* needs 0.169.0).
const DefaultCodexClientVersion = "0.169.0"

var clientVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

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
