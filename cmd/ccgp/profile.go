package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/preset"
)

// defaultCatchallWindow is the context window stated for a catch-all model
// rule created with --model but without --window.
const defaultCatchallWindow = 272_000

// profileAddOpts is the parsed `ccgp profile add` command line.
type profileAddOpts struct {
	Name      string
	Like      string // --like <profile>: copy an existing profile + its provider
	Provider  string // --provider <p>: new profile on an existing provider
	Type      string // --type: brand-new provider + profile
	BaseURL   string // --base-url (with --type)
	Model     string // --model: one catch-all rule
	Window    int64  // --window
	HasModel  bool
	HasWindow bool
}

// profileCmd implements `ccgp profile <add|list|rm>`.
func profileCmd(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ccgp profile add <name> (--like <profile> | --provider <provider> | --type <codex|codex_api|anthropic_compat>) | ccgp profile list | ccgp profile rm <name> [--yes] [--purge]")
	}
	if cfg.IsEnvMode() {
		return errors.New("ccgp profile needs config.ktav (env-only mode has a single implicit profile)")
	}
	store, err := credsStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		opts, err := parseProfileAdd(args[1:])
		if err != nil {
			return err
		}
		return profileAdd(os.Stdout, opts, cfg.ConfigFile, cfg, store)
	case "list":
		base, err := claudeHomeBase()
		if err != nil {
			return err
		}
		return profileList(os.Stdout, cfg, store, base)
	case "rm":
		opts, err := parseProfileRm(args[1:])
		if err != nil {
			return err
		}
		base, err := claudeHomeBase()
		if err != nil {
			return err
		}
		return profileRemove(os.Stdout, opts, cfg.ConfigFile, cfg, store, base, os.Stdin)
	}
	return fmt.Errorf("unknown profile subcommand %q (want add, list or rm)", args[0])
}

// parseProfileAdd parses flags; both --flag value and --flag=value work.
func parseProfileAdd(args []string) (profileAddOpts, error) {
	var o profileAddOpts
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		need := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", arg)
			}
			i++
			return args[i], nil
		}
		if strings.HasPrefix(name, "--") {
			v, err := need()
			if err != nil {
				return o, err
			}
			switch name {
			case "--like":
				o.Like = v
			case "--provider":
				o.Provider = v
			case "--type":
				o.Type = v
			case "--base-url":
				o.BaseURL = v
			case "--model":
				if v == "" {
					return o, errors.New("--model needs a model name")
				}
				o.Model, o.HasModel = v, true
			case "--window":
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil || n < 1 {
					return o, fmt.Errorf("--window must be a positive number, got %q", v)
				}
				o.Window, o.HasWindow = n, true
			default:
				return o, fmt.Errorf("profile add: unknown flag %q", arg)
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return o, fmt.Errorf("profile add: unknown flag %q", arg)
		}
		if o.Name != "" {
			return o, fmt.Errorf("profile add: unexpected argument %q (only one name)", arg)
		}
		o.Name = arg
	}
	return o, nil
}

// nameTaken reports whether a profile or provider already uses name.
func nameTaken(cfg *config.Config, name string) bool {
	_, profile := cfg.Profiles[name]
	_, provider := cfg.Providers[name]
	return profile || provider
}

// profileAdd is the testable core of `ccgp profile add`. The store parameter
// keeps the credential store at hand for future uses; credentials themselves
// are created by the first `ccgp <name>` sign-in, not here.
func profileAdd(w io.Writer, opts profileAddOpts, cfgPath string, cfg *config.Config, store creds.Store) error {
	_ = store
	chosen := 0
	for _, v := range []string{opts.Like, opts.Provider, opts.Type} {
		if v != "" {
			chosen++
		}
	}
	if chosen != 1 {
		return errors.New("usage: ccgp profile add <name> --like <profile> | --provider <provider> | --type <codex|codex_api|anthropic_compat> [--base-url URL] [--model M [--window N]] (exactly one of --like, --provider, --type)")
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	srcText := string(data)

	var prov *config.ProviderSpec // nil: the profile shares an existing provider
	var rules []config.ModelRule
	managed := false
	providerName, providerType, providerURL := "", "", ""

	switch {
	case opts.Like != "":
		src, ok := cfg.Profiles[opts.Like]
		if !ok {
			return fmt.Errorf("unknown profile %q (configured: %s)", opts.Like, names(cfg.Profiles))
		}
		srcProv := cfg.Providers[src.Provider]
		// The copy gets its own credentials (no api_key_env) and the source's name.
		prov = &config.ProviderSpec{
			Name:      opts.Like,
			Type:      srcProv.Type,
			BaseURL:   srcProv.BaseURL,
			EffortMap: srcProv.EffortMap,
		}
		providerName, providerType, providerURL = opts.Like, srcProv.Type, srcProv.BaseURL
		rules = src.Models
		managed = config.HasManaged(srcText, opts.Like)

	case opts.Provider != "":
		pr, ok := cfg.Providers[opts.Provider]
		if !ok {
			return fmt.Errorf("unknown provider %q (configured: %s)", opts.Provider, names(cfg.Providers))
		}
		providerName, providerType, providerURL = opts.Provider, pr.Type, pr.BaseURL
		if !opts.HasModel {
			// Copy the mapping of the provider's first profile (by name).
			for _, n := range sortedKeys(cfg.Profiles) {
				if cfg.Profiles[n].Provider != opts.Provider {
					continue
				}
				rules = cfg.Profiles[n].Models
				managed = config.HasManaged(srcText, n)
				break
			}
		}

	case opts.Type != "":
		switch opts.Type {
		case config.ProviderTypeCodex, config.ProviderTypeCodexAPI, config.ProviderTypeAnthropicCompat:
		default:
			return fmt.Errorf("--type must be %q, %q or %q", config.ProviderTypeCodex, config.ProviderTypeCodexAPI, config.ProviderTypeAnthropicCompat)
		}
		providerName, providerType, providerURL = opts.Name, opts.Type, opts.BaseURL
		prov = &config.ProviderSpec{Name: opts.Name, Type: opts.Type, BaseURL: opts.BaseURL}
		if p, ok := preset.ForBaseURL(opts.BaseURL); ok {
			prov.EffortMap = p.EffortMap
		}
	}

	// Ready for `ccgp models apply`: codex providers have a live catalog,
	// other providers a built-in preset for their URL. Without --model no
	// mapping is invented.
	_, hasPreset := preset.ForBaseURL(providerURL)
	if opts.HasModel {
		window := opts.Window
		if !opts.HasWindow {
			window = defaultCatchallWindow
		}
		rules = []config.ModelRule{{Match: "*", Model: opts.Model, ContextWindow: window}}
	}
	readyForApply := !opts.HasModel && len(rules) == 0 &&
		(providerType == config.ProviderTypeCodex || hasPreset)

	name := opts.Name
	if name == "" && opts.Like != "" {
		name = config.NextFreeName(opts.Like, func(n string) bool { return nameTaken(cfg, n) })
	}
	if name == "" {
		return errors.New("profile add: a profile name is required")
	}
	if err := config.ValidateProfileName(name); err != nil {
		return err
	}
	if nameTaken(cfg, name) {
		return fmt.Errorf("name %q is already in use by a profile or provider", name)
	}
	if prov != nil {
		// A new provider is named like its profile, and the profile must use it,
		// not the source's provider: that is what makes it a separate login.
		prov.Name = name
		providerName = name
	}

	backup, err := config.EditFile(cfgPath, func(text string) (string, error) {
		out, err := config.AddProfileText(text, prov, config.ProfileSpec{Name: name, Provider: providerName, Rules: rules, Managed: managed})
		if err != nil || !readyForApply {
			return out, err
		}
		// An empty managed block, so `ccgp models apply <name>` can fill it;
		// inside the same validated, backed-up edit.
		return config.AddManaged(out, name)
	})
	if err != nil {
		return err
	}

	if prov != nil {
		_, _ = fmt.Fprintf(w, "created profile %q with its own provider %q (type %s)\n", name, name, providerType)
	} else {
		_, _ = fmt.Fprintf(w, "created profile %q sharing provider %q\n", name, providerName)
	}
	if backup != "" {
		_, _ = fmt.Fprintf(w, "backup: %s\n", backup)
	}
	if opts.HasModel && !opts.HasWindow {
		_, _ = fmt.Fprintf(w, "rule: * -> %s, context_window %d (the default; set --window to change it)\n", opts.Model, defaultCatchallWindow)
	}
	_, _ = fmt.Fprintf(w, "run: ccgp %s   (signs in on first start)\n", name)
	switch {
	case readyForApply:
		_, _ = fmt.Fprintf(w, "then: ccgp models apply %s\n", name)
	case !opts.HasModel && len(rules) == 0:
		_, _ = fmt.Fprintf(w, "no model rules were copied; edit %s to add a models list\n", cfgPath)
	}
	return nil
}

// profileList is the testable core of `ccgp profile list`.
func profileList(w io.Writer, cfg *config.Config, store creds.Store, claudeBase string) error {
	_, _ = fmt.Fprintf(w, "%-16s %-16s %-17s %-38s %-16s %s\n",
		"PROFILE", "PROVIDER", "TYPE", "LOGIN", "MAIN MODEL", "CLAUDE DIR")
	for _, name := range sortedKeys(cfg.Profiles) {
		pf := cfg.Profiles[name]
		pr := cfg.Providers[pf.Provider]
		mainModel := "-"
		if m, ok := (&config.Config{ModelRules: pf.Models, DefaultModel: pf.DefaultModel}).ResolveModel("sonnet"); ok {
			mainModel = m
		}
		dir := "no"
		if p, err := claudeProfileDir(claudeBase, name); err == nil {
			if _, err := os.Stat(p); err == nil {
				dir = "yes"
			}
		}
		_, _ = fmt.Fprintf(w, "%-16s %-16s %-17s %-38s %-16s %s\n",
			name, pf.Provider, pr.Type, loginState(store, pr), mainModel, dir)
	}
	return nil
}

// profileRmOpts is the parsed `ccgp profile rm` command line.
type profileRmOpts struct {
	Name  string
	Yes   bool
	Purge bool
}

func parseProfileRm(args []string) (profileRmOpts, error) {
	var o profileRmOpts
	for _, arg := range args {
		switch arg {
		case "--yes":
			o.Yes = true
		case "--purge":
			o.Purge = true
		default:
			if strings.HasPrefix(arg, "-") {
				return o, fmt.Errorf("profile rm: unknown flag %q", arg)
			}
			if o.Name != "" {
				return o, fmt.Errorf("profile rm: unexpected argument %q (only one name)", arg)
			}
			o.Name = arg
		}
	}
	if o.Name == "" {
		return o, errors.New("usage: ccgp profile rm <name> [--yes] [--purge]")
	}
	return o, nil
}

// claudeProfileDir is the isolated Claude Code dir of a profile under base.
// It is only safe to delete when the name is valid and the path is a direct
// child of base: the user's own Claude Code dir must never be touched.
func claudeProfileDir(base, name string) (string, error) {
	if err := config.ValidateProfileName(name); err != nil {
		return "", err
	}
	base = filepath.Clean(base)
	p := filepath.Join(base, name)
	if filepath.Dir(p) != base {
		return "", fmt.Errorf("%q is not a profile directory under %s", name, base)
	}
	return p, nil
}

// profileRemove is the testable core of `ccgp profile rm`.
func profileRemove(w io.Writer, opts profileRmOpts, cfgPath string, cfg *config.Config, store creds.Store, claudeBase string, stdin io.Reader) error {
	pf, ok := cfg.Profiles[opts.Name]
	if !ok {
		return fmt.Errorf("unknown profile %q (configured: %s)", opts.Name, names(cfg.Profiles))
	}
	if !opts.Yes {
		_, _ = fmt.Fprintf(w, "remove profile %q? [y/N] ", opts.Name)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			_, _ = fmt.Fprintln(w, "aborted")
			return nil
		}
	}

	provider := pf.Provider
	refs := 0
	for _, n := range sortedKeys(cfg.Profiles) {
		if cfg.Profiles[n].Provider == provider {
			refs++
		}
	}
	backup, err := config.EditFile(cfgPath, func(text string) (string, error) {
		return config.RemoveProfileText(text, opts.Name, true)
	})
	if err != nil {
		return err
	}
	if err := store.DeleteProfileToken(opts.Name); err != nil {
		return fmt.Errorf("drop the URL token of %q: %w", opts.Name, err)
	}
	providerRemoved := refs == 1
	if providerRemoved {
		// RemoveProfileText already dropped the provider object from the config.
		delete(cfg.Providers, provider)
	}
	delete(cfg.Profiles, opts.Name)

	_, _ = fmt.Fprintf(w, "removed profile %q from %s\n", opts.Name, cfgPath)
	if backup != "" {
		_, _ = fmt.Fprintf(w, "backup: %s\n", backup)
	}
	if providerRemoved {
		_, _ = fmt.Fprintf(w, "provider %q had no other profile and was removed\n", provider)
	} else {
		_, _ = fmt.Fprintf(w, "provider %q still has other profiles and was kept\n", provider)
	}

	dir, dirErr := claudeProfileDir(claudeBase, opts.Name)
	if opts.Purge {
		if dirErr != nil {
			return dirErr
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "deleted the Claude dir %s\n", dir)
		if providerRemoved {
			if err := store.DeleteProviderCreds(provider); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "deleted the credentials of provider %q in %s\n", provider, store.Dir)
		}
		return nil
	}
	if dirErr == nil {
		if _, err := os.Stat(dir); err == nil {
			_, _ = fmt.Fprintf(w, "kept the Claude dir %s (delete it with --purge)\n", dir)
		}
	}
	if providerRemoved {
		// The provider is gone from the config, so nothing uses its login now.
		_, _ = fmt.Fprintf(w, "left behind: the credentials of provider %q in %s (delete them with --purge)\n", provider, store.Dir)
	}
	return nil
}
