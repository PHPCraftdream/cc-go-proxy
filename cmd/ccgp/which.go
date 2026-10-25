package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
)

// routeInfo describes where a Claude model name is routed in a profile.
type routeInfo struct {
	Profile       string `json:"profile"`
	Provider      string `json:"provider"`
	ProviderType  string `json:"providerType"`
	Requested     string `json:"requested"`
	BackendModel  string `json:"backendModel"`
	ContextWindow string `json:"contextWindow"` // "unknown" when not configured
	MatchedBy     string `json:"matchedBy"`     // empty when not derivable
}

// whichRoute resolves the model routing of a profile the same way the server
// endpoint /call/<token>/model does: per-profile config, then MapModel.
func whichRoute(cfg *config.Config, profile, model string) (routeInfo, error) {
	pf, ok := cfg.Profiles[profile]
	if !ok {
		return routeInfo{}, fmt.Errorf("unknown profile %q (configured: %s)", profile, names(cfg.Profiles))
	}
	// Env mode: the base config is already the per-profile config (tier
	// overrides stay active); file mode: build it like profileResolver does.
	pcfg := cfg
	if !cfg.IsEnvMode() {
		var err error
		pcfg, err = cfg.ForProfile(profile, "")
		if err != nil {
			return routeInfo{}, err
		}
	}
	backend := converter.MapModel(model, pcfg)
	window := pf.ContextWindowFor(model)
	return routeInfo{
		Profile:       profile,
		Provider:      pcfg.ProviderName,
		ProviderType:  pcfg.ProviderType,
		Requested:     model,
		BackendModel:  backend,
		ContextWindow: contextWindowText(window),
		MatchedBy:     matchedBy(pcfg, model),
	}, nil
}

// windowText renders a context window; 0 means the config says nothing.
func contextWindowText(w int64) string {
	if w <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", w)
}

// matchedBy mirrors the precedence of converter.MapModel without touching
// internal/converter: first matching rule, then default_model, then the
// haiku/sonnet/opus tier defaults, then passthrough.
func matchedBy(pcfg *config.Config, model string) string {
	name := strings.ToLower(model)
	for _, r := range pcfg.ModelRules {
		if ok, _ := path.Match(strings.ToLower(r.Match), name); ok {
			return "rule " + r.Match
		}
	}
	if pcfg.DefaultModel != "" {
		return "default_model"
	}
	if strings.Contains(name, "haiku") || strings.Contains(name, "sonnet") || strings.Contains(name, "opus") {
		return "tier setting"
	}
	return "passthrough"
}

func whichCmd(args []string) error {
	profile, model, jsonOut, err := parseWhichArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ri, err := whichRoute(cfg, profile, model)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(os.Stdout, ri)
	}
	return renderRouteText(os.Stdout, ri)
}

// renderRouteText prints the route as aligned label: value lines.
func renderRouteText(w io.Writer, ri routeInfo) error {
	rows := [][2]string{
		{"profile", ri.Profile},
		{"provider", ri.Provider + " (" + ri.ProviderType + ")"},
		{"requested", ri.Requested},
		{"backend model", ri.BackendModel},
		{"context window", ri.ContextWindow},
	}
	if ri.MatchedBy != "" {
		rows = append(rows, [2]string{"matched by", ri.MatchedBy})
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "%-15s %s\n", r[0]+":", r[1]); err != nil {
			return err
		}
	}
	return nil
}

// parseWhichArgs splits the --which arguments into profile, model and --json.
func parseWhichArgs(args []string) (profile, model string, jsonOut bool, err error) {
	var rest []string
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) != 2 {
		return "", "", false, errors.New("usage: ccgp --which <profile> <model> [--json]")
	}
	return rest[0], rest[1], jsonOut, nil
}
