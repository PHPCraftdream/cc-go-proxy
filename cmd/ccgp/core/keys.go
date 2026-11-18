package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/tune"
	"github.com/PHPCraftdream/cc-go-proxy/internal/clientver"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex/catalog"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
	peakcfg "github.com/PHPCraftdream/cc-go-proxy/internal/config/peak"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/preset"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
)

const (
	profileSetUsage = "usage: ccgp --profile set <name> <key> <value> | ccgp --profile unset <name> <key>   (keys: default_model, context_window, compact_percent, provider; unset does not take provider)"
	providerUsage   = "usage: ccgp --provider [NAME] [--json] | ccgp --provider set <name> <key> <value> | ccgp --provider unset <name> <key>   (keys: base_url, api_key_env)"
)

// reportEdit says what an edit of config.ktav did, as the other commands do.
func reportEdit(w io.Writer, backup string) {
	if backup == "" {
		_, _ = fmt.Fprintln(w, "config.ktav already had this value")
		return
	}
	_, _ = fmt.Fprintf(w, "config.ktav updated (old one saved as %s); the running server applies it on its next request\n", backup)
}

// profileSet implements `ccgp --profile set|unset <name> <key> [value]`.
func ProfileSet(w io.Writer, cfg *config.Config, args []string, unset bool) error {
	want := 3
	if unset {
		want = 2
	}
	if len(args) != want {
		return errors.New(profileSetUsage)
	}
	name, key := args[0], args[1]
	pf, ok := cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q (configured: %s)", name, common.Names(cfg.Profiles))
	}
	if common.IsClaudeProfile(cfg, pf) {
		return fmt.Errorf("profile %q is a Claude profile: it does not go through ccgp, so it has no settings of ccgp's", name)
	}
	if !slices.Contains(cfgedit.ProfileKeys(), key) {
		return fmt.Errorf("unknown profile key %q (%s)", key, strings.Join(cfgedit.ProfileKeys(), ", "))
	}
	value := ""
	if !unset {
		value = strings.TrimSpace(args[2])
	}
	switch key {
	case "provider":
		if unset || value == "" {
			return errors.New("a profile needs a provider")
		}
		if !slices.Contains(common.ProviderNames(cfg), value) {
			return fmt.Errorf("unknown provider %q (configured: %s)", value, strings.Join(common.ProviderNames(cfg), ", "))
		}
	case "context_window", "compact_percent":
		if value != "" {
			if n, err := strconv.ParseInt(value, 10, 64); err != nil || n < 0 {
				return fmt.Errorf("%s: want a number, got %q", key, value)
			}
		}
	}
	if cfg.IsEnvMode() {
		return errors.New("no config.ktav (env-only mode): nothing to edit")
	}
	backup, err := cfgedit.EditFile(cfg.ConfigFile, func(text string) (string, error) {
		return cfgedit.SetProfileKeyText(text, name, key, value)
	})
	if err != nil {
		return err
	}
	reportEdit(w, backup)
	return nil
}

// providerRow is one provider in `ccgp --provider --json`.
type providerRow struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	BaseURL   string   `json:"baseUrl"`
	APIKeyEnv string   `json:"apiKeyEnv"`
	Login     string   `json:"login"`
	Proxy     string   `json:"proxy"`
	ProxyOwn  bool     `json:"proxyOwn"`
	PeakHours string   `json:"peakHours"`
	Profiles  []string `json:"profiles"`
}

type ProviderListData struct {
	Providers []providerRow `json:"providers"`
}

func ProviderRows(cfg *config.Config, store creds.Store, pauses *peakcfg.Pauses, now time.Time) []providerRow {
	var rows []providerRow
	for _, name := range common.SortedKeys(cfg.Providers) {
		pr := cfg.Providers[name]
		row := providerRow{Name: name, Type: pr.Type, BaseURL: pr.EffectiveBaseURL(), APIKeyEnv: pr.APIKeyEnv,
			Login: common.LoginState(store, pr), Profiles: common.ProfilesOf(cfg, name)}
		if !config.IsOwnType(pr.Type) {
			row.Proxy, row.ProxyOwn = cfg.ProviderProxy(name).String(), pr.Proxy != ""
			row.PeakHours = tune.PeakSummary(pr, name, now, pauses)
		}
		rows = append(rows, row)
	}
	return rows
}

func ProviderCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := common.CredsStore()
	if err != nil {
		return err
	}
	return Provider(os.Stdout, cfg, store, peakcfg.NewPauses(store.Dir), time.Now(), args)
}

// provider implements `ccgp --provider`: the list, one provider in detail, and
// set / unset of base_url and api_key_env.
func Provider(w io.Writer, cfg *config.Config, store creds.Store, pauses *peakcfg.Pauses, now time.Time, args []string) error {
	if cfg.IsEnvMode() {
		return errors.New("ccgp --provider needs config.ktav (env-only mode has a single implicit provider)")
	}
	if len(args) > 0 && (args[0] == "set" || args[0] == "unset") {
		return ProviderSet(w, cfg, args[1:], args[0] == "unset")
	}
	jsonOut, name := false, ""
	for _, a := range args {
		switch {
		case a == "--json":
			jsonOut = true
		case name == "" && !strings.HasPrefix(a, "-"):
			name = a
		default:
			return errors.New(providerUsage)
		}
	}
	if name != "" {
		if _, ok := cfg.Providers[name]; !ok {
			return fmt.Errorf("unknown provider %q (configured: %s)", name, common.Names(cfg.Providers))
		}
	}
	rows := ProviderRows(cfg, store, pauses, now)
	if name != "" {
		kept := rows[:0]
		for _, r := range rows {
			if r.Name == name {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if jsonOut {
		return common.WriteJSON(w, ProviderListData{Providers: rows})
	}
	if name != "" {
		_, _ = fmt.Fprintln(w, tune.ProviderText(cfg, store, pauses, now, name))
		return nil
	}
	_, _ = fmt.Fprintf(w, "%-14s %-17s %-12s %s\n", "PROVIDER", "TYPE", "PEAK", "PROFILES")
	for _, r := range rows {
		peak := "-"
		if r.PeakHours != "" && r.PeakHours != "none" {
			peak = "yes"
		}
		_, _ = fmt.Fprintf(w, "%-14s %-17s %-12s %s\n", r.Name, r.Type, peak, common.Dash(strings.Join(r.Profiles, ", ")))
	}
	return nil
}

// providerSet implements `ccgp --provider set|unset <name> <key> [value]`.
func ProviderSet(w io.Writer, cfg *config.Config, args []string, unset bool) error {
	want := 3
	if unset {
		want = 2
	}
	if len(args) != want {
		return errors.New(providerUsage)
	}
	name, key := args[0], args[1]
	pr, ok := cfg.Providers[name]
	if !ok {
		return fmt.Errorf("unknown provider %q (configured: %s)", name, common.Names(cfg.Providers))
	}
	if pr.Type == config.ProviderTypeClaude {
		return fmt.Errorf("provider %q is a Claude profile: it does not go through ccgp, so it has no settings of ccgp's", name)
	}
	if !slices.Contains(cfgedit.ProviderKeys(), key) {
		return fmt.Errorf("unknown provider key %q (%s)", key, strings.Join(cfgedit.ProviderKeys(), ", "))
	}
	if key == "api_key_env" && pr.Type == config.ProviderTypeCodex {
		return fmt.Errorf("provider %q signs in with OAuth: it takes no api_key_env", name)
	}
	value := ""
	if !unset {
		value = strings.TrimSpace(args[2])
	}
	backup, err := cfgedit.EditFile(cfg.ConfigFile, func(text string) (string, error) {
		return cfgedit.SetProviderKeyText(text, name, key, value)
	})
	if err != nil {
		return err
	}
	reportEdit(w, backup)
	return nil
}

// profileVerbs and providerVerbs list the entry points of --profile and
// --provider beyond the command itself.
func ProfileVerbs() []string {
	v := []string{"list", "add --like", "add --provider", "add --type", "rm"}
	for _, k := range cfgedit.ProfileKeys() {
		v = append(v, "set "+k)
		if k != "provider" {
			v = append(v, "unset "+k)
		}
	}
	return v
}

func ProviderVerbs() []string {
	var v []string
	for _, k := range cfgedit.ProviderKeys() {
		v = append(v, "set "+k, "unset "+k)
	}
	return v
}

// models implements `ccgp --models [apply] [-v] [profile]`.
func Models(cfg *config.Config, args []string) error {
	apply, verbose, jsonOut := false, false, false
flags:
	for len(args) > 0 {
		switch args[0] {
		case "apply":
			apply = true
		case "-v", "--verbose":
			verbose = true
		case "--json":
			jsonOut = true
		default:
			break flags
		}
		args = args[1:]
	}
	if len(args) > 1 {
		return errors.New("usage: ccgp --models [apply] [-v] [profile]")
	}
	if cfg.IsEnvMode() {
		return errors.New("models needs config.ktav (env-only mode has no managed profiles)")
	}
	if apply && jsonOut {
		return errors.New("--json only works with the show form: ccgp --models [--json] [-v] [profile]")
	}

	profiles, err := ManagedProfiles(cfg, args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for _, pf := range profiles {
		if cfg.Providers[pf.Provider].Type == config.ProviderTypeCodex {
			clientVersion(ctx, cfg, apply, os.Stderr)
			break
		}
	}
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

	if !jsonOut {
		for _, name := range common.SortedKeys(byProvider) {
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
	}
	var mj ModelsData
	for _, pf := range profiles {
		if jsonOut {
			if pc := byProvider[pf.Provider]; pc != nil {
				mj.Profiles = append(mj.Profiles, CatalogModelsProfile(pf.Name, pf.Provider, pc.sel))
			} else {
				p, _ := preset.ForBaseURL(cfg.Providers[pf.Provider].BaseURL)
				mj.Profiles = append(mj.Profiles, PresetModelsProfile(pf.Name, pf.Provider, p))
			}
			continue
		}
		fmt.Println()
		var rules []config.ModelRule
		if pc := byProvider[pf.Provider]; pc != nil {
			printSelection(pf.Name, pc.sel)
			rules = clientver.ManagedFrom(pc.sel).Rules
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
	if jsonOut {
		return common.WriteJSON(os.Stdout, mj)
	}
	if !apply {
		fmt.Println("\nDry run. Write this into the config with: ccgp --models apply")
	}
	return nil
}

// modelTier is one Claude tier -> backend model mapping.
type modelTier struct {
	Tier          string `json:"tier"`
	Model         string `json:"model"`
	ContextWindow int64  `json:"contextWindow"`
}

type ModelsProfile struct {
	Profile  string      `json:"profile"`
	Provider string      `json:"provider"`
	Preset   string      `json:"preset,omitempty"`
	Tiers    []modelTier `json:"tiers"`
	Notes    []string    `json:"notes,omitempty"`
}

type ModelsData struct {
	Profiles []ModelsProfile `json:"profiles"`
}

// catalogModelsProfile converts a live codex catalog selection into JSON form.
func CatalogModelsProfile(profile, provider string, sel catalog.Selection) ModelsProfile {
	return ModelsProfile{
		Profile:  profile,
		Provider: provider,
		Tiers: []modelTier{
			{Tier: "fable", Model: sel.Fable, ContextWindow: sel.FableWindow},
			{Tier: "opus", Model: sel.Opus, ContextWindow: sel.OpusWindow},
			{Tier: "sonnet", Model: sel.Sonnet, ContextWindow: sel.SonnetWindow},
			{Tier: "haiku", Model: sel.Haiku, ContextWindow: sel.HaikuWindow},
			// Custom model names route to the Sonnet model.
			{Tier: "other", Model: sel.Sonnet, ContextWindow: sel.SonnetWindow},
		},
		Notes: sel.Notes,
	}
}

// presetModelsProfile converts a built-in preset into JSON form.
func PresetModelsProfile(profile, provider string, p preset.Preset) ModelsProfile {
	tiers := make([]modelTier, 0, len(p.Rules))
	for _, r := range p.Rules {
		tiers = append(tiers, modelTier{Tier: r.Match, Model: r.Model, ContextWindow: r.ContextWindow})
	}
	return ModelsProfile{Profile: profile, Provider: provider, Preset: p.Name, Tiers: tiers}
}

// writeManaged writes rules into the profile's managed block, adding the block
// first when the profile has none.
func writeManaged(cfg *config.Config, profile string, rules []config.ModelRule) error {
	added, err := cfgedit.EnsureManaged(cfg.ConfigFile, profile)
	if err != nil {
		return err
	}
	if added {
		fmt.Printf("added a managed block to profile %q (its previous default_model and models were replaced)\n", profile)
	}
	return cfgedit.ApplyManaged(cfg.ConfigFile, profile, cfgedit.ManagedModels{Rules: rules})
}

// printRules shows a preset's mapping.
func printRules(profile string, p preset.Preset) {
	fmt.Printf("profile %q mapping (preset %s: %s):\n", profile, p.Name, p.Source)
	for _, r := range p.Rules {
		fmt.Printf("  %-12s -> %-16s %d\n", r.Match, r.Model, r.ContextWindow)
	}
}

type providerCatalog struct {
	models []catalog.Model
	sel    catalog.Selection
}

// managedProfiles returns the profiles whose mapping ccgp can generate: codex
// profiles (live catalog) and anthropic_compat profiles with a built-in preset.
// With only set, just that profile.
func ManagedProfiles(cfg *config.Config, only []string) ([]*config.Profile, error) {
	var out []*config.Profile
	for _, name := range common.SortedKeys(cfg.Profiles) {
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
			return nil, fmt.Errorf("no generated profile named %q (configured: %s; mappings can be generated for codex providers and anthropic_compat providers with a preset)", only[0], common.Names(cfg.Profiles))
		}
		return nil, errors.New("no profile uses a codex provider or a provider with a built-in preset")
	}
	return out, nil
}

func loadCatalog(ctx context.Context, cfg *config.Config, provider string) (*providerCatalog, error) {
	ctx = proxydial.WithChoice(ctx, cfg.ProviderProxyChoice(provider))
	store, err := common.CredsStore()
	if err != nil {
		return nil, err
	}
	sess := &oauth.Session{Store: oauth.Store{Path: store.OAuthPath(provider)}, Client: &oauth.Client{}}
	tok, err := sess.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w (run: ccgp --login %s)", err, provider)
	}
	ctx = codex.WithClientVersion(ctx, cfg.CodexClientVersion())
	list, err := catalog.FetchCatalog(ctx, cfg.Providers[provider].BaseURL, tok)
	if err != nil {
		return nil, err
	}
	sel, err := catalog.Pick(list)
	if err != nil {
		return nil, fmt.Errorf("%w (catalog: %s)", err, slugs(list))
	}
	return &providerCatalog{models: list, sel: sel}, nil
}

func slugs(list []catalog.Model) string {
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

func printSelection(profile string, sel catalog.Selection) {
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

// clientVersion brings the Codex client version up to the newest Codex CLI
// release before a catalog read: the backend lists a model generation only to
// a client at least as new as the one that brought it. write raises
// codex.client_version in config.ktav (seeding it first when absent); without
// write the newer version is only used for this read. Best effort: a failed
// lookup leaves the configured version.
func clientVersion(ctx context.Context, cfg *config.Config, write bool, out io.Writer) {
	if write {
		if seeded, err := cfgedit.SeedCodexClientVersion(cfg); err != nil {
			_, _ = fmt.Fprintf(out, "ccgp: could not write codex.client_version: %v\n", err)
		} else if seeded {
			_, _ = fmt.Fprintf(out, "codex.client_version %s written to %s\n", cfg.CodexClientVersion(), cfg.ConfigFile)
		}
	}
	current := cfg.CodexClientVersion()
	target, err := clientver.Default.Target(proxydial.WithChoice(ctx, cfg.ProxyChoice("")), current, "")
	if err != nil {
		_, _ = fmt.Fprintf(out, "ccgp: could not check for a newer Codex CLI (%v); using codex.client_version %s\n", err, current)
		return
	}
	if target == "" {
		return
	}
	if !write {
		_, _ = fmt.Fprintf(out, "Codex CLI %s is out (codex.client_version is %s): the catalog below is read as %s; `ccgp --models apply` raises it\n", target, current, target)
		cfg.CodexClientVersionSet = target
		return
	}
	if _, _, err := cfgedit.RaiseCodexClientVersion(cfg.ConfigFile, target); err != nil {
		_, _ = fmt.Fprintf(out, "ccgp: could not raise codex.client_version to %s: %v\n", target, err)
		return
	}
	cfg.CodexClientVersionSet = target
	_, _ = fmt.Fprintf(out, "codex.client_version raised %s -> %s (newest Codex CLI release)\n", current, target)
}

// autoApplyModels refreshes the managed mapping of every codex profile on a
// provider after a fresh login. Best effort: problems are reported, not fatal.
func autoApplyModels(cfg *config.Config, provider string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clientVersion(ctx, cfg, true, os.Stderr)
	pc, err := loadCatalog(ctx, cfg, provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccgp: could not read the model catalog (%v); run `ccgp --models apply` later\n", err)
		return
	}
	data, err := os.ReadFile(cfg.ConfigFile)
	if err != nil {
		return
	}
	for _, name := range common.SortedKeys(cfg.Profiles) {
		pf := cfg.Profiles[name]
		if pf.Provider != provider || !cfgedit.HasManaged(string(data), name) {
			continue
		}
		if err := cfgedit.ApplyManaged(cfg.ConfigFile, name, clientver.ManagedFrom(pc.sel)); err != nil {
			fmt.Fprintf(os.Stderr, "ccgp: update mapping of profile %q: %v\n", name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "Model mapping for profile %q: Fable=%s Opus=%s Sonnet=%s Haiku=%s\n",
			name, pc.sel.Fable, pc.sel.Opus, pc.sel.Sonnet, pc.sel.Haiku)
	}
}

// printRaw lists a catalog entry's short scalar fields (long prompt texts and
// nested structures are skipped), to see which size-related fields a catalog has.
func printRaw(m catalog.Model) {
	keys := make([]string, 0, len(m.Raw))
	for k := range m.Raw {
		keys = append(keys, k)
	}
	common.SortStrings(keys)
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
	for _, name := range common.SortedKeys(cfg.Profiles) {
		pf := cfg.Profiles[name]
		if pf.Provider != provider || !cfgedit.HasManaged(string(data), name) {
			continue
		}
		if err := cfgedit.ApplyManaged(cfg.ConfigFile, name, cfgedit.ManagedModels{Rules: p.Rules}); err != nil {
			fmt.Fprintf(os.Stderr, "ccgp: update mapping of profile %q: %v\n", name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "Model mapping for profile %q: preset %s (Fable=%s, other tiers=%s)\n", name, p.Name, p.Rules[0].Model, p.Rules[1].Model)
	}
}

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
func WhichRoute(cfg *config.Config, profile, model string) (routeInfo, error) {
	pf, ok := cfg.Profiles[profile]
	if !ok {
		return routeInfo{}, fmt.Errorf("unknown profile %q (configured: %s)", profile, common.Names(cfg.Profiles))
	}
	if common.IsClaudeProfile(cfg, pf) {
		return routeInfo{}, fmt.Errorf("profile %q is a Claude profile: it does not go through ccgp, so it has no model mapping", profile)
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

func WhichCmd(args []string) error {
	profile, model, jsonOut, err := parseWhichArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ri, err := WhichRoute(cfg, profile, model)
	if err != nil {
		return err
	}
	if jsonOut {
		return common.WriteJSON(os.Stdout, ri)
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
