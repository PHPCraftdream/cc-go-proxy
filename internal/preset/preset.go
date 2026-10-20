// Package preset holds built-in model mappings for providers that have no
// model catalog of their own, so `ccgp models apply` can write them into the
// config. Numbers come from the providers' documentation (see each preset).
package preset

import (
	"net/url"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
)

// Preset is a named, ordered mapping of Claude tiers to backend models.
type Preset struct {
	Name  string
	Rules []config.ModelRule
	// Source documents where the model names and windows come from.
	Source string
	// EffortMap converts Claude Code effort levels (low, medium, high, xhigh,
	// max) to the levels the provider accepts; nil: pass through.
	EffortMap map[string]string
}

// ZAI is z.ai's GLM family behind its Anthropic-compatible endpoint
// (https://api.z.ai/api/anthropic). Per docs.z.ai, glm-5.3 and glm-5.3-flash
// have a 1M context window (128K max output for glm-5.3). Fable gets the
// full model; Opus and everything below it the fast one.
var ZAI = Preset{
	Name: "zai",
	Rules: []config.ModelRule{
		{Match: "*fable*", Model: "glm-5.3", ContextWindow: 1_000_000},
		{Match: "*opus*", Model: "glm-5.3-flash", ContextWindow: 1_000_000},
		{Match: "*sonnet*", Model: "glm-5.3-flash", ContextWindow: 1_000_000},
		{Match: "*haiku*", Model: "glm-5.3-flash", ContextWindow: 1_000_000},
		{Match: "*", Model: "glm-5.3-flash", ContextWindow: 1_000_000},
	},
	Source: "docs.z.ai/guides/overview/overview, docs.z.ai/devpack/tool/claude",
	// z.ai takes only three effort levels: low, high and max.
	EffortMap: map[string]string{
		"low": "low", "medium": "high", "high": "max", "xhigh": "max", "max": "max",
	},
}

// ZAIBaseURL is the Anthropic-compatible endpoint Claude Code uses with z.ai.
const ZAIBaseURL = "https://api.z.ai/api/anthropic"

// ForBaseURL returns the preset for a provider base URL, matched by host.
func ForBaseURL(rawURL string) (Preset, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Preset{}, false
	}
	switch strings.ToLower(u.Hostname()) {
	case "api.z.ai", "z.ai", "open.bigmodel.cn":
		return ZAI, true
	}
	return Preset{}, false
}
