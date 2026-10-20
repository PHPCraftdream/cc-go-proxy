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
)

// models implements `ccgp models [apply] [profile]`.
func models(cfg *config.Config, args []string) error {
	apply := false
	if len(args) > 0 && args[0] == "apply" {
		apply, args = true, args[1:]
	}
	if len(args) > 1 {
		return errors.New("usage: ccgp models [apply] [profile]")
	}
	if cfg.IsEnvMode() {
		return errors.New("models needs config.ktav (env-only mode has no codex profiles)")
	}

	profiles, err := codexProfiles(cfg, args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// One catalog per provider.
	byProvider := map[string]*providerCatalog{}
	for _, pf := range profiles {
		if _, ok := byProvider[pf.Provider]; ok {
			continue
		}
		pc, err := loadCatalog(ctx, cfg, pf.Provider)
		if err != nil {
			return fmt.Errorf("provider %q: %w", pf.Provider, err)
		}
		byProvider[pf.Provider] = pc
	}

	for name, pc := range byProvider {
		fmt.Printf("provider %s: %d models in the account catalog\n", name, len(pc.models))
		for _, m := range pc.models {
			fmt.Printf("  %-28s %s\n", m.Slug, windowText(m.ContextWindow))
		}
	}
	for _, pf := range profiles {
		sel := byProvider[pf.Provider].sel
		fmt.Println()
		printSelection(pf.Name, sel)
		if !apply {
			continue
		}
		added, err := config.EnsureManaged(cfg.ConfigFile, pf.Name)
		if err != nil {
			return err
		}
		if added {
			fmt.Printf("added a managed block to profile %q (its previous default_model and models were replaced)\n", pf.Name)
		}
		if err := config.ApplyManaged(cfg.ConfigFile, pf.Name, managedFrom(sel)); err != nil {
			return err
		}
		fmt.Printf("applied to %s (profile %q)\n", cfg.ConfigFile, pf.Name)
	}
	if !apply {
		fmt.Println("\nDry run. Write this into the config with: ccgp models apply")
	}
	return nil
}

type providerCatalog struct {
	models []codex.Model
	sel    codex.Selection
}

// codexProfiles returns the codex-backed profiles to process: the named one,
// or all of them.
func codexProfiles(cfg *config.Config, only []string) ([]*config.Profile, error) {
	var out []*config.Profile
	for _, name := range sortedKeys(cfg.Profiles) {
		pf := cfg.Profiles[name]
		if cfg.Providers[pf.Provider].Type != config.ProviderTypeCodex {
			continue
		}
		if len(only) == 1 && only[0] != name {
			continue
		}
		out = append(out, pf)
	}
	if len(out) == 0 {
		if len(only) == 1 {
			return nil, fmt.Errorf("no codex profile named %q (configured: %s)", only[0], names(cfg.Profiles))
		}
		return nil, errors.New("no profile uses a codex provider")
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
	fmt.Printf("profile %q mapping:\n", profile)
	fmt.Printf("  Fable  -> %s\n  Opus   -> %s\n  Sonnet -> %s\n  Haiku  -> %s\n", sel.Fable, sel.Opus, sel.Sonnet, sel.Haiku)
	fmt.Printf("  context_window: %d (smallest of Fable/Opus/Sonnet; Haiku model: %d)\n", sel.ContextWindow, sel.HaikuWindow)
	for _, n := range sel.Notes {
		fmt.Printf("  note: %s\n", n)
	}
}

// managedFrom converts a catalog selection into the config block.
func managedFrom(sel codex.Selection) config.ManagedModels {
	return config.ManagedModels{
		DefaultModel: sel.Sonnet,
		Rules: []config.ModelRule{
			{Match: "*fable*", Model: sel.Fable},
			{Match: "*opus*", Model: sel.Opus},
			{Match: "*sonnet*", Model: sel.Sonnet},
			{Match: "*haiku*", Model: sel.Haiku},
		},
		ContextWindow: sel.ContextWindow,
	}
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
