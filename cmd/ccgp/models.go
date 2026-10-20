package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/claude-code-proxy/proxy/internal/codex"
	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/oauth"
	"github.com/claude-code-proxy/proxy/internal/preset"
)

// models implements `ccgp models [apply] [-v] [profile]`.
func models(cfg *config.Config, args []string) error {
	apply, verbose := false, false
flags:
	for len(args) > 0 {
		switch args[0] {
		case "apply":
			apply = true
		case "-v", "--verbose":
			verbose = true
		default:
			break flags
		}
		args = args[1:]
	}
	if len(args) > 1 {
		return errors.New("usage: ccgp models [apply] [-v] [profile]")
	}
	if cfg.IsEnvMode() {
		return errors.New("models needs config.ktav (env-only mode has no managed profiles)")
	}

	profiles, err := managedProfiles(cfg, args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// One catalog per codex provider; anthropic_compat providers use built-in presets.
	byProvider := map[string]*providerCatalog{}
	for _, pf := range profiles {
		if cfg.Providers[pf.Provider].Type != config.ProviderTypeCodex {
			continue
		}
		if _, ok := byProvider[pf.Provider]; ok {
			continue
		}
		pc, err := loadCatalog(ctx, cfg, pf.Provider)
		if err != nil {
			return fmt.Errorf("provider %q: %w", pf.Provider, err)
		}
		byProvider[pf.Provider] = pc
	}

	for _, name := range sortedKeys(byProvider) {
		pc := byProvider[name]
		fmt.Printf("provider %s: %d models in the account catalog\n", name, len(pc.models))
		for _, m := range pc.models {
			hidden := ""
			if m.Hidden {
				hidden = "  (hidden)"
			}
			fmt.Printf("  %-28s %s%s\n", m.Slug, windowText(m.ContextWindow), hidden)
			if verbose {
				printRaw(m)
			}
		}
	}
	for _, pf := range profiles {
		fmt.Println()
		var rules []config.ModelRule
		if pc := byProvider[pf.Provider]; pc != nil {
			printSelection(pf.Name, pc.sel)
			rules = managedFrom(pc.sel).Rules
		} else {
			p, _ := preset.ForBaseURL(cfg.Providers[pf.Provider].BaseURL)
			printRules(pf.Name, p)
			rules = p.Rules
		}
		if !apply {
			continue
		}
		if err := writeManaged(cfg, pf.Name, rules); err != nil {
			return err
		}
		fmt.Printf("applied to %s (profile %q)\n", cfg.ConfigFile, pf.Name)
	}
	if !apply {
		fmt.Println("\nDry run. Write this into the config with: ccgp models apply")
	}
	return nil
}

// writeManaged writes rules into the profile's managed block, adding the block
// first when the profile has none.
func writeManaged(cfg *config.Config, profile string, rules []config.ModelRule) error {
	added, err := config.EnsureManaged(cfg.ConfigFile, profile)
	if err != nil {
		return err
	}
	if added {
		fmt.Printf("added a managed block to profile %q (its previous default_model and models were replaced)\n", profile)
	}
	return config.ApplyManaged(cfg.ConfigFile, profile, config.ManagedModels{Rules: rules})
}

// printRules shows a preset's mapping.
func printRules(profile string, p preset.Preset) {
	fmt.Printf("profile %q mapping (preset %s: %s):\n", profile, p.Name, p.Source)
	for _, r := range p.Rules {
		fmt.Printf("  %-12s -> %-16s %d\n", r.Match, r.Model, r.ContextWindow)
	}
}

type providerCatalog struct {
	models []codex.Model
	sel    codex.Selection
}

// managedProfiles returns the profiles whose mapping ccgp can generate: codex
// profiles (live catalog) and anthropic_compat profiles with a built-in preset.
// With only set, just that profile.
func managedProfiles(cfg *config.Config, only []string) ([]*config.Profile, error) {
	var out []*config.Profile
	for _, name := range sortedKeys(cfg.Profiles) {
		pf := cfg.Profiles[name]
		pr := cfg.Providers[pf.Provider]
		ok := pr.Type == config.ProviderTypeCodex
		if pr.Type == config.ProviderTypeAnthropicCompat {
			_, ok = preset.ForBaseURL(pr.BaseURL)
		}
		if !ok || (len(only) == 1 && only[0] != name) {
			continue
		}
		out = append(out, pf)
	}
	if len(out) == 0 {
		if len(only) == 1 {
			return nil, fmt.Errorf("no generated profile named %q (configured: %s; mappings can be generated for codex providers and anthropic_compat providers with a preset)", only[0], names(cfg.Profiles))
		}
		return nil, errors.New("no profile uses a codex provider or a provider with a built-in preset")
	}
	return out, nil
}

func loadCatalog(ctx context.Context, cfg *config.Config, provider string) (*providerCatalog, error) {
	store, err := credsStore()
	if err != nil {
		return nil, err
	}
	sess := &oauth.Session{Store: oauth.Store{Path: store.OAuthPath(provider)}, Client: &oauth.Client{}}
	tok, err := sess.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w (run: ccgp login %s)", err, provider)
	}
	list, err := codex.FetchCatalog(ctx, cfg.Providers[provider].BaseURL, tok)
	if err != nil {
		return nil, err
	}
	sel, err := codex.Pick(list)
	if err != nil {
		return nil, fmt.Errorf("%w (catalog: %s)", err, slugs(list))
	}
	return &providerCatalog{models: list, sel: sel}, nil
}

func slugs(list []codex.Model) string {
	s := make([]string, len(list))
	for i, m := range list {
		s[i] = m.Slug
	}
	return strings.Join(s, ", ")
}

func windowText(w int64) string {
	if w <= 0 {
		return "context: unknown"
	}
	return fmt.Sprintf("context: %d tokens", w)
}

func printSelection(profile string, sel codex.Selection) {
	fmt.Printf("profile %q mapping (model, context window):\n", profile)
	fmt.Printf("  Fable  -> %-16s %d\n", sel.Fable, sel.FableWindow)
	fmt.Printf("  Opus   -> %-16s %d\n", sel.Opus, sel.OpusWindow)
	fmt.Printf("  Sonnet -> %-16s %d\n", sel.Sonnet, sel.SonnetWindow)
	fmt.Printf("  Haiku  -> %-16s %d\n", sel.Haiku, sel.HaikuWindow)
	fmt.Printf("  other  -> %-16s %d\n", sel.Sonnet, sel.SonnetWindow)
	for _, n := range sel.Notes {
		fmt.Printf("  note: %s\n", n)
	}
}

// managedFrom converts a catalog selection into the config block.
func managedFrom(sel codex.Selection) config.ManagedModels {
	return config.ManagedModels{Rules: []config.ModelRule{
		{Match: "*fable*", Model: sel.Fable, ContextWindow: sel.FableWindow},
		{Match: "*opus*", Model: sel.Opus, ContextWindow: sel.OpusWindow},
		{Match: "*sonnet*", Model: sel.Sonnet, ContextWindow: sel.SonnetWindow},
		{Match: "*haiku*", Model: sel.Haiku, ContextWindow: sel.HaikuWindow},
		// Anything else (custom names) goes to the Sonnet model.
		{Match: "*", Model: sel.Sonnet, ContextWindow: sel.SonnetWindow},
	}}
}

// autoApplyModels refreshes the managed mapping of every codex profile on a
// provider after a fresh login. Best effort: problems are reported, not fatal.
func autoApplyModels(cfg *config.Config, provider string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pc, err := loadCatalog(ctx, cfg, provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccgp: could not read the model catalog (%v); run `ccgp models apply` later\n", err)
		return
	}
	data, err := os.ReadFile(cfg.ConfigFile)
	if err != nil {
		return
	}
	for _, name := range sortedKeys(cfg.Profiles) {
		pf := cfg.Profiles[name]
		if pf.Provider != provider || !config.HasManaged(string(data), name) {
			continue
		}
		if err := config.ApplyManaged(cfg.ConfigFile, name, managedFrom(pc.sel)); err != nil {
			fmt.Fprintf(os.Stderr, "ccgp: update mapping of profile %q: %v\n", name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "Model mapping for profile %q: Fable=%s Opus=%s Sonnet=%s Haiku=%s\n",
			name, pc.sel.Fable, pc.sel.Opus, pc.sel.Sonnet, pc.sel.Haiku)
	}
}

// printRaw lists a catalog entry's short scalar fields (long prompt texts and
// nested structures are skipped), to see which size-related fields a catalog has.
func printRaw(m codex.Model) {
	keys := make([]string, 0, len(m.Raw))
	for k := range m.Raw {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		v := strings.TrimSpace(string(m.Raw[k]))
		if v == "" || len(v) > 80 || strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") {
			continue
		}
		fmt.Printf("      %s = %s\n", k, v)
	}
}

// autoApplyPreset writes a provider's built-in mapping into the managed block
// of every profile on it, after the API key was saved. Best effort: profiles
// without a managed block (hand-written mappings) are left alone.
func autoApplyPreset(cfg *config.Config, provider string) {
	pr := cfg.Providers[provider]
	p, ok := preset.ForBaseURL(pr.BaseURL)
	if !ok {
		return
	}
	data, err := os.ReadFile(cfg.ConfigFile)
	if err != nil {
		return
	}
	for _, name := range sortedKeys(cfg.Profiles) {
		pf := cfg.Profiles[name]
		if pf.Provider != provider || !config.HasManaged(string(data), name) {
			continue
		}
		if err := config.ApplyManaged(cfg.ConfigFile, name, config.ManagedModels{Rules: p.Rules}); err != nil {
			fmt.Fprintf(os.Stderr, "ccgp: update mapping of profile %q: %v\n", name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "Model mapping for profile %q: preset %s (Fable=%s, other tiers=%s)\n", name, p.Name, p.Rules[0].Model, p.Rules[1].Model)
	}
}
