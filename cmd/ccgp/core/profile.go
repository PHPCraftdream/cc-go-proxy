package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/preset"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"golang.org/x/term"
)

// defaultCatchallWindow is the context window stated for a catch-all model
// rule created with --model but without --window.
const DefaultCatchallWindow = 272_000

// profileAddOpts is the parsed `ccgp --profile add` command line.
type ProfileAddOpts struct {
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

// profileCmd implements `ccgp --profile <add|list|rm>`.
func ProfileCmd(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ccgp --profile add <name> (--like <profile> | --provider <provider> | --type <codex|codex_api|anthropic_compat|claude|claude_api>) | ccgp --profile list | ccgp --profile rm <name> [--yes] [--purge] | ccgp --profile set <name> <key> <value> | ccgp --profile unset <name> <key>")
	}
	if cfg.IsEnvMode() {
		return errors.New("ccgp --profile needs config.ktav (env-only mode has a single implicit profile)")
	}
	store, err := common.CredsStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		opts, err := parseProfileAdd(args[1:])
		if err != nil {
			return err
		}
		return ProfileAdd(os.Stdout, opts, cfg.ConfigFile, cfg, store)
	case "list":
		base, err := common.ClaudeHomeBase()
		if err != nil {
			return err
		}
		jsonOut := false
		for _, a := range args[1:] {
			if a == "--json" {
				jsonOut = true
			}
		}
		rows := BuildProfileRows(cfg, store, base)
		if jsonOut {
			return common.WriteJSON(os.Stdout, ProfileListData{Profiles: rows})
		}
		return ProfileList(os.Stdout, rows)
	case "rm":
		opts, err := parseProfileRm(args[1:])
		if err != nil {
			return err
		}
		base, err := common.ClaudeHomeBase()
		if err != nil {
			return err
		}
		return ProfileRemove(os.Stdout, opts, cfg.ConfigFile, cfg, store, base, os.Stdin)
	case "set", "unset":
		return ProfileSet(os.Stdout, cfg, args[1:], args[0] == "unset")
	}
	return fmt.Errorf("unknown profile subcommand %q (want add, list, rm, set or unset)", args[0])
}

// parseProfileAdd parses flags; both --flag value and --flag=value work.
func parseProfileAdd(args []string) (ProfileAddOpts, error) {
	var o ProfileAddOpts
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
func NameTaken(cfg *config.Config, name string) bool {
	_, profile := cfg.Profiles[name]
	_, provider := cfg.Providers[name]
	return profile || provider
}

// profileAdd is the testable core of `ccgp --profile add`. The store parameter
// keeps the credential store at hand for future uses; credentials themselves
// are created by the first `ccgp <name>` sign-in, not here.
func ProfileAdd(w io.Writer, opts ProfileAddOpts, cfgPath string, cfg *config.Config, store creds.Store) error {
	_ = store
	chosen := 0
	for _, v := range []string{opts.Like, opts.Provider, opts.Type} {
		if v != "" {
			chosen++
		}
	}
	if chosen != 1 {
		return errors.New("usage: ccgp --profile add <name> --like <profile> | --provider <provider> | --type <codex|codex_api|anthropic_compat|claude|claude_api> [--base-url URL] [--model M [--window N]] (exactly one of --like, --provider, --type)")
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	srcText := string(data)

	var prov *cfgedit.ProviderSpec // nil: the profile shares an existing provider
	var rules []config.ModelRule
	managed := false
	providerName, providerType, providerURL := "", "", ""

	switch {
	case opts.Like != "":
		src, ok := cfg.Profiles[opts.Like]
		if !ok {
			return fmt.Errorf("unknown profile %q (configured: %s)", opts.Like, common.Names(cfg.Profiles))
		}
		srcProv := cfg.Providers[src.Provider]
		// The copy gets its own credentials (no api_key_env) and the source's name.
		prov = &cfgedit.ProviderSpec{
			Name:      opts.Like,
			Type:      srcProv.Type,
			BaseURL:   srcProv.BaseURL,
			EffortMap: srcProv.EffortMap,
			// Peak hours and the proxy belong to the service, not to the login.
			Peak:   srcProv.Peak,
			PeakTZ: srcProv.PeakTZ,
			Proxy:  srcProv.Proxy,
		}
		providerName, providerType, providerURL = opts.Like, srcProv.Type, srcProv.BaseURL
		rules = src.Models
		managed = cfgedit.HasManaged(srcText, opts.Like)

	case opts.Provider != "":
		pr, ok := cfg.Providers[opts.Provider]
		if !ok {
			return fmt.Errorf("unknown provider %q (configured: %s)", opts.Provider, common.Names(cfg.Providers))
		}
		if config.IsOwnType(pr.Type) {
			return fmt.Errorf("provider %q is a Claude profile: every Claude account and every API key is its own profile (ccgp --profile add <name> --type %s)", opts.Provider, pr.Type)
		}
		providerName, providerType, providerURL = opts.Provider, pr.Type, pr.BaseURL
		if !opts.HasModel {
			// Copy the mapping of the provider's first profile (by name).
			for _, n := range common.SortedKeys(cfg.Profiles) {
				if cfg.Profiles[n].Provider != opts.Provider {
					continue
				}
				rules = cfg.Profiles[n].Models
				managed = cfgedit.HasManaged(srcText, n)
				break
			}
		}

	case opts.Type != "":
		switch opts.Type {
		case config.ProviderTypeCodex, config.ProviderTypeCodexAPI, config.ProviderTypeAnthropicCompat:
		case config.ProviderTypeClaude:
			if opts.BaseURL != "" || opts.HasModel || opts.HasWindow {
				return fmt.Errorf("--type %s goes straight to Claude: --base-url, --model and --window do not apply", opts.Type)
			}
		case config.ProviderTypeClaudeAPI:
			if opts.HasModel || opts.HasWindow {
				return fmt.Errorf("--type %s goes straight to Claude: --model and --window do not apply", opts.Type)
			}
		default:
			return fmt.Errorf("--type must be %q, %q, %q, %q or %q", config.ProviderTypeCodex, config.ProviderTypeCodexAPI, config.ProviderTypeAnthropicCompat, config.ProviderTypeClaude, config.ProviderTypeClaudeAPI)
		}
		providerName, providerType, providerURL = opts.Name, opts.Type, opts.BaseURL
		prov = &cfgedit.ProviderSpec{Name: opts.Name, Type: opts.Type, BaseURL: opts.BaseURL}
		if p, ok := preset.ForBaseURL(opts.BaseURL); ok && !config.IsOwnType(opts.Type) {
			prov.EffortMap = p.EffortMap
		}
	}

	// Ready for `ccgp --models apply`: codex providers have a live catalog,
	// other providers a built-in preset for their URL. Without --model no
	// mapping is invented.
	_, hasPreset := preset.ForBaseURL(providerURL)
	if opts.HasModel {
		window := opts.Window
		if !opts.HasWindow {
			window = DefaultCatchallWindow
		}
		rules = []config.ModelRule{{Match: "*", Model: opts.Model, ContextWindow: window}}
	}
	readyForApply := !opts.HasModel && len(rules) == 0 && !config.IsOwnType(providerType) &&
		(providerType == config.ProviderTypeCodex || hasPreset)

	name := opts.Name
	if name == "" && opts.Like != "" {
		name = cfgedit.NextFreeName(opts.Like, func(n string) bool { return NameTaken(cfg, n) })
	}
	if name == "" {
		return errors.New("profile add: a profile name is required")
	}
	if err := cfgedit.ValidateProfileName(name); err != nil {
		return err
	}
	if NameTaken(cfg, name) {
		return fmt.Errorf("name %q is already in use by a profile or provider", name)
	}
	if prov != nil {
		// A new provider is named like its profile, and the profile must use it,
		// not the source's provider: that is what makes it a separate login.
		prov.Name = name
		providerName = name
	}

	backup, err := cfgedit.EditFile(cfgPath, func(text string) (string, error) {
		out, err := cfgedit.AddProfileText(text, prov, cfgedit.ProfileSpec{Name: name, Provider: providerName, Rules: rules, Managed: managed})
		if err != nil || !readyForApply {
			return out, err
		}
		// An empty managed block, so `ccgp --models apply <name>` can fill it;
		// inside the same validated, backed-up edit.
		return cfgedit.AddManaged(out, name)
	})
	if err != nil {
		return err
	}

	if providerType == config.ProviderTypeClaudeAPI {
		server := providerURL
		if server == "" {
			server = config.OfficialAnthropicURL + " (the official one)"
		}
		_, _ = fmt.Fprintf(w, "created Claude API profile %q: the real Claude Code with an API key of its own, server %s, nothing goes through ccgp\n", name, server)
		if backup != "" {
			_, _ = fmt.Fprintf(w, "backup: %s\n", backup)
		}
		_, _ = fmt.Fprintf(w, "run: ccgp %s   (asks for the API key on first start; or set it now: ccgp --login %s; only settings, CLAUDE.md, skills and agents are copied from your own Claude Code)\n", name, name)
		return nil
	}
	if providerType == config.ProviderTypeClaude {
		_, _ = fmt.Fprintf(w, "created Claude profile %q: the real Claude Code with its own login, nothing goes through ccgp\n", name)
		if backup != "" {
			_, _ = fmt.Fprintf(w, "backup: %s\n", backup)
		}
		_, _ = fmt.Fprintf(w, "run: ccgp %s   (sign in inside Claude Code with /login; only settings, CLAUDE.md, skills and agents are copied from your own Claude Code)\n", name)
		return nil
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
		_, _ = fmt.Fprintf(w, "rule: * -> %s, context_window %d (the default; set --window to change it)\n", opts.Model, DefaultCatchallWindow)
	}
	_, _ = fmt.Fprintf(w, "run: ccgp %s   (signs in on first start)\n", name)
	switch {
	case readyForApply:
		_, _ = fmt.Fprintf(w, "then: ccgp --models apply %s\n", name)
	case !opts.HasModel && len(rules) == 0:
		_, _ = fmt.Fprintf(w, "no model rules were copied; edit %s to add a models list\n", cfgPath)
	}
	return nil
}

// profileRow is one entry of `ccgp --profile list`.
type ProfileRow struct {
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Type      string `json:"type"`
	Login     string `json:"login"`
	MainModel string `json:"mainModel"`
	ClaudeDir bool   `json:"claudeDir"`
}

type ProfileListData struct {
	Profiles []ProfileRow `json:"profiles"`
}

// buildProfileRows assembles the list rows; the only I/O is the Claude dir check.
func BuildProfileRows(cfg *config.Config, store creds.Store, claudeBase string) []ProfileRow {
	var rows []ProfileRow
	for _, name := range common.SortedKeys(cfg.Profiles) {
		pf := cfg.Profiles[name]
		pr := cfg.Providers[pf.Provider]
		mainModel := "-"
		if m, ok := (&config.Config{ModelRules: pf.Models, DefaultModel: pf.DefaultModel}).ResolveModel("sonnet"); ok && !config.IsOwnType(pr.Type) {
			mainModel = m
		}
		claudeDir := false
		if p, err := common.ClaudeProfileDir(claudeBase, name); err == nil {
			if _, err := os.Stat(p); err == nil {
				claudeDir = true
			}
		}
		login := common.LoginState(store, pr)
		if pr.Type == config.ProviderTypeClaude {
			login = common.ClaudeLoginState(claudeBase, name)
		}
		rows = append(rows, ProfileRow{Name: name, Provider: pf.Provider, Type: pr.Type, Login: login, MainModel: mainModel, ClaudeDir: claudeDir})
	}
	return rows
}

// profileList prints the human-readable profile table.
func ProfileList(w io.Writer, rows []ProfileRow) error {
	_, _ = fmt.Fprintf(w, "%-16s %-16s %-17s %-38s %-16s %s\n",
		"PROFILE", "PROVIDER", "TYPE", "LOGIN", "MAIN MODEL", "CLAUDE DIR")
	for _, r := range rows {
		dir := "no"
		if r.ClaudeDir {
			dir = "yes"
		}
		_, _ = fmt.Fprintf(w, "%-16s %-16s %-17s %-38s %-16s %s\n",
			r.Name, r.Provider, r.Type, r.Login, r.MainModel, dir)
	}
	return nil
}

// profileRmOpts is the parsed `ccgp --profile rm` command line.
type ProfileRmOpts struct {
	Name  string
	Yes   bool
	Purge bool
}

func parseProfileRm(args []string) (ProfileRmOpts, error) {
	var o ProfileRmOpts
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
		return o, errors.New("usage: ccgp --profile rm <name> [--yes] [--purge]")
	}
	return o, nil
}

// profileRemove is the testable core of `ccgp --profile rm`.
func ProfileRemove(w io.Writer, opts ProfileRmOpts, cfgPath string, cfg *config.Config, store creds.Store, claudeBase string, stdin io.Reader) error {
	pf, ok := cfg.Profiles[opts.Name]
	if !ok {
		return fmt.Errorf("unknown profile %q (configured: %s)", opts.Name, common.Names(cfg.Profiles))
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
	for _, n := range common.SortedKeys(cfg.Profiles) {
		if cfg.Profiles[n].Provider == provider {
			refs++
		}
	}
	backup, err := cfgedit.EditFile(cfgPath, func(text string) (string, error) {
		return cfgedit.RemoveProfileText(text, opts.Name, true)
	})
	if err != nil {
		return err
	}
	if err := store.DeleteProfileToken(opts.Name); err != nil {
		return fmt.Errorf("drop the URL token of %q: %w", opts.Name, err)
	}
	providerRemoved := refs == 1
	own := cfg.Providers[provider].Type == config.ProviderTypeClaude // the login lives in the Claude dir
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

	dir, dirErr := common.ClaudeProfileDir(claudeBase, opts.Name)
	if opts.Purge {
		if dirErr != nil {
			return dirErr
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "deleted the Claude dir %s\n", dir)
		if providerRemoved && !own {
			if err := store.DeleteProviderCreds(provider); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "deleted the credentials of provider %q in %s\n", provider, store.Dir)
		}
		return nil
	}
	if dirErr == nil {
		if _, err := os.Stat(dir); err == nil {
			if own {
				_, _ = fmt.Fprintf(w, "kept the Claude dir %s with its sign-in (delete it with --purge)\n", dir)
			} else {
				_, _ = fmt.Fprintf(w, "kept the Claude dir %s (delete it with --purge)\n", dir)
			}
		}
	}
	if providerRemoved && !own {
		// The provider is gone from the config, so nothing uses its login now.
		_, _ = fmt.Fprintf(w, "left behind: the credentials of provider %q in %s (delete them with --purge)\n", provider, store.Dir)
	}
	return nil
}

// login authenticates a provider. force=false skips when credentials exist.
func Login(cfg *config.Config, provider string, force, device bool) error {
	if cfg.IsEnvMode() {
		return errors.New("login needs config.ktav; env-only mode uses OPENAI_API_KEY")
	}
	pr, ok := cfg.Providers[provider]
	if !ok {
		return fmt.Errorf("unknown provider %q (configured: %s)", provider, common.Names(cfg.Providers))
	}
	store, err := common.CredsStore()
	if err != nil {
		return err
	}
	switch pr.Type {
	case config.ProviderTypeClaude:
		return fmt.Errorf("profile %q is a Claude profile: sign in inside Claude Code (ccgp %s, then /login)", pr.Name, pr.Name)
	case config.ProviderTypeCodex:
		fresh, err := loginCodex(store, pr.Name, force, device, cfg.ProviderProxyChoice(pr.Name))
		if err == nil && fresh {
			autoApplyModels(cfg, pr.Name)
		}
		return err
	case config.ProviderTypeCodexAPI, config.ProviderTypeClaudeAPI:
		_, err := loginAPIKey(store, pr, force)
		return err
	case config.ProviderTypeAnthropicCompat:
		fresh, err := loginAPIKey(store, pr, force)
		if err == nil && fresh {
			autoApplyPreset(cfg, pr.Name)
		}
		return err
	}
	return fmt.Errorf("provider %q: unsupported type %q", pr.Name, pr.Type)
}

// loginCodex signs in with ChatGPT OAuth: browser callback flow by default,
// device-code flow with device=true. The browser is never opened automatically.
func loginCodex(store creds.Store, provider string, force, device bool, via proxydial.Choice) (fresh bool, err error) {
	ts := oauth.Store{Path: store.OAuthPath(provider)}
	if !force {
		if _, lerr := ts.Load(); lerr == nil {
			return false, nil
		} else if !errors.Is(lerr, oauth.ErrNotLoggedIn) {
			return false, lerr
		}
	}
	ctx, stop := signal.NotifyContext(proxydial.WithChoice(context.Background(), via), os.Interrupt)
	defer stop()

	var tok *oauth.Tokens
	if device {
		tok, err = deviceLogin(ctx, provider)
	} else {
		tok, err = browserLogin(ctx, provider)
	}
	if err != nil {
		return false, err
	}
	if err := ts.Save(tok); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stderr, "Logged in (provider %q).\n", provider)
	return true, nil
}

func browserLogin(ctx context.Context, provider string) (*oauth.Tokens, error) {
	tok, err := (&oauth.Client{}).BrowserLogin(ctx, func(authURL string) {
		fmt.Fprintf(os.Stderr, "\nSign in to ChatGPT for provider %q.\n\n", provider)
		fmt.Fprintf(os.Stderr, "Open this link in a browser on this computer:\n\n  %s\n\n", authURL)
		fmt.Fprintf(os.Stderr, "After you sign in, the browser returns to localhost:%s and ccgp continues.\n", oauth.CallbackPort)
		fmt.Fprintf(os.Stderr, "No browser on this computer? Use: ccgp --login %s --device\n\nWaiting (10 minutes)...\n", provider)
	})
	if err != nil {
		return nil, err
	}
	return tok, nil
}

func deviceLogin(ctx context.Context, provider string) (*oauth.Tokens, error) {
	c := &oauth.Client{}
	dc, err := c.RequestDeviceCode(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "\nSign in to ChatGPT for provider %q (device code):\n\n", provider)
	fmt.Fprintf(os.Stderr, "  1. Open in a browser:  %s\n", dc.VerificationURL)
	fmt.Fprintf(os.Stderr, "  2. Enter the code:     %s   (expires in 15 minutes)\n\n", dc.UserCode)
	fmt.Fprintln(os.Stderr, "Device-code sign-in must be enabled in ChatGPT Security Settings.")
	fmt.Fprintln(os.Stderr, "Only continue if you started this login yourself. Waiting...")
	return c.CompleteDeviceLogin(ctx, dc)
}

func loginAPIKey(store creds.Store, pr *config.Provider, force bool) (fresh bool, err error) {
	if pr.APIKeyEnv != "" {
		if os.Getenv(pr.APIKeyEnv) == "" {
			return false, fmt.Errorf("provider %q reads its key from $%s, which is empty", pr.Name, pr.APIKeyEnv)
		}
		return false, nil
	}
	if !force {
		if k, err := store.APIKey(pr.Name); err != nil || k != "" {
			return false, err
		}
		if common.IsLocal(pr.BaseURL) && pr.Type != config.ProviderTypeClaudeAPI {
			return false, nil // local backends (Ollama) need no key
		}
	}
	fmt.Fprintf(os.Stderr, "API key for provider %q (%s): ", pr.Name, pr.EffectiveBaseURL())
	key, err := readSecret()
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return false, err
	}
	if key == "" {
		return false, errors.New("empty API key")
	}
	if err := store.SaveAPIKey(pr.Name, key); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stderr, "API key saved (provider %q).\n", pr.Name)
	return true, nil
}

// readSecret reads a line without echo when stdin is a terminal.
func readSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func LogoutCmd(args []string) error {
	yes, rest := false, []string{}
	for _, a := range args {
		if a == "--yes" {
			yes = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		return errors.New("usage: ccgp --logout <provider> [--yes]")
	}
	if !yes && !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("refusing to log out without a terminal; pass --yes to confirm")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := common.CredsStore()
	if err != nil {
		return err
	}
	return Logout(os.Stdout, os.Stdin, store, cfg, rest[0], yes)
}

// logout is the testable core of `ccgp --logout`. It removes the provider's
// stored credentials only; config.ktav is never written. Any stored file
// counts, whatever the provider's current type or api_key_env: a corrupt or
// stale file must not be left behind.
func Logout(w io.Writer, in io.Reader, store creds.Store, cfg *config.Config, provider string, yes bool) error {
	if cfg.IsEnvMode() {
		return errors.New("ccgp --logout needs config.ktav (env-only mode has no stored logins)")
	}
	pr, ok := cfg.Providers[provider]
	if !ok {
		return fmt.Errorf("unknown provider %q (configured: %s)", provider, common.Names(cfg.Providers))
	}
	if !store.HasProviderCreds(provider) {
		if pr.APIKeyEnv != "" {
			_, _ = fmt.Fprintf(w, "provider %q reads its key from $%s, there is no stored login (the environment variable is not touched)\n", provider, pr.APIKeyEnv)
		} else {
			_, _ = fmt.Fprintf(w, "provider %q has no stored login\n", provider)
		}
		return nil
	}
	if !yes {
		_, _ = fmt.Fprintf(w, "remove the login of provider %q? [y/N] ", provider)
		line, _ := bufio.NewReader(in).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			_, _ = fmt.Fprintln(w, "aborted")
			return nil
		}
	}
	if err := store.DeleteProviderCreds(provider); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "logged out of %s\n", provider)
	_, _ = fmt.Fprintf(w, "login again with: ccgp --login %s\n", provider)
	return nil
}
