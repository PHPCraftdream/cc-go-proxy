package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
)

const interactiveUsage = "usage: ccgp --interactive (or -i)"

// interactiveCmd runs `ccgp --interactive`: menus to look at, change and add
// profiles and providers. Nothing starts it but the flag.
func interactiveCmd(args []string) error {
	if len(args) != 0 {
		return errors.New(interactiveUsage)
	}
	store, err := credsStore()
	if err != nil {
		return err
	}
	base, err := claudeHomeBase()
	if err != nil {
		return err
	}
	u := &ui{
		in: bufio.NewReader(os.Stdin), out: os.Stdout, now: time.Now,
		store: store, claudeBase: base, pauses: config.NewPeakPauses(store.Dir),
	}
	return u.run()
}

// ui drives the menus over any reader and writer, one numbered choice or one
// line per question; the end of the input quits. Every change goes through the
// same editors as the commands (config.EditFile: validated, backed up).
type ui struct {
	in         *bufio.Reader
	out        io.Writer
	now        func() time.Time
	store      creds.Store
	claudeBase string
	pauses     *config.PeakPauses
}

type choice struct{ key, label string }

func (u *ui) printf(format string, a ...any) { _, _ = fmt.Fprintf(u.out, format, a...) }

// readLine asks and returns the answer without its line end; io.EOF when the
// input is over.
func (u *ui) readLine(prompt string) (string, error) {
	u.printf("%s", prompt)
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return "", io.EOF
	}
	return strings.TrimSpace(line), nil
}

// pick shows the choices and returns the key of the one typed; an empty line
// is "0". A word that is no choice is asked again.
func (u *ui) pick(title string, items []choice) (string, error) {
	if title != "" {
		u.printf("\n%s\n", title)
	}
	for _, it := range items {
		u.printf("  %s) %s\n", it.key, it.label)
	}
	for {
		line, err := u.readLine("> ")
		if err != nil {
			return "", err
		}
		if line == "" {
			line = "0"
		}
		for _, it := range items {
			if strings.EqualFold(it.key, line) {
				return it.key, nil
			}
		}
		u.printf("not a choice: %q\n", line)
	}
}

// ask returns the line typed, or def for an empty one.
func (u *ui) ask(label, def string) (string, error) {
	if def != "" {
		label += " [" + def + "]"
	}
	line, err := u.readLine(label + ": ")
	if err != nil {
		return "", err
	}
	if line == "" {
		return def, nil
	}
	return line, nil
}

func (u *ui) confirm(question string) (bool, error) {
	line, err := u.readLine(question + " [y/N] ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(line) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// fail reports a failed action and goes on; only the end of the input stops.
func (u *ui) fail(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	u.printf("error: %v\n", err)
	return nil
}

// load reads the config afresh: every screen shows what the file holds now.
func (u *ui) load() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.IsEnvMode() {
		return nil, errors.New("ccgp --interactive needs config.ktav (env-only mode has a single implicit profile)")
	}
	return cfg, nil
}

// apply runs an edit of config.ktav and says what happened.
func (u *ui) apply(cfg *config.Config, edit func(text string) (string, error)) error {
	backup, err := config.EditFile(cfg.ConfigFile, edit)
	if err != nil {
		return err
	}
	if backup == "" {
		u.printf("config.ktav already had this\n")
		return nil
	}
	u.printf("config.ktav updated (old one saved as %s)\n", backup)
	return nil
}

// editKey asks for a new value (Enter keeps, - removes) and applies it.
func (u *ui) editKey(cfg *config.Config, key, current string, set func(text, value string) (string, error)) error {
	line, err := u.ask(fmt.Sprintf("new %s (now %s; Enter keeps, - removes)", key, dash(current)), "")
	if err != nil {
		return err
	}
	switch line {
	case "":
		u.printf("unchanged\n")
		return nil
	case "-":
		line = ""
	}
	return u.apply(cfg, func(text string) (string, error) { return set(text, line) })
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (u *ui) run() error {
	err := u.mainMenu()
	if errors.Is(err, io.EOF) {
		u.printf("\n")
		return nil
	}
	return err
}

func (u *ui) mainMenu() error {
	for {
		cfg, err := u.load()
		if err != nil {
			return err
		}
		key, err := u.pick("ccgp: "+cfg.ConfigFile, []choice{{"1", "profiles"}, {"2", "providers"}, {"0", "quit"}})
		if err != nil {
			return err
		}
		switch key {
		case "1":
			err = u.profiles()
		case "2":
			err = u.providers()
		default:
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// profileNames lists the profiles of cfg.
func profileNames(cfg *config.Config) []string { return sortedKeys(cfg.Profiles) }

// providerNames lists the providers that go through ccgp (a Claude one is not
// a backend a profile can be moved to).
func providerNames(cfg *config.Config) []string {
	var out []string
	for _, n := range sortedKeys(cfg.Providers) {
		if cfg.Providers[n].Type != config.ProviderTypeClaude {
			out = append(out, n)
		}
	}
	return out
}

// profilesOf lists the profiles that use a provider.
func profilesOf(cfg *config.Config, provider string) []string {
	var out []string
	for _, n := range sortedKeys(cfg.Profiles) {
		if cfg.Profiles[n].Provider == provider {
			out = append(out, n)
		}
	}
	return out
}

// chooseOne lets the user pick one of names; "" when they cancel.
func (u *ui) chooseOne(title string, names []string) (string, error) {
	if len(names) == 0 {
		u.printf("nothing to choose from\n")
		return "", nil
	}
	items := make([]choice, 0, len(names)+1)
	for i, n := range names {
		items = append(items, choice{strconv.Itoa(i + 1), n})
	}
	items = append(items, choice{"0", "cancel"})
	key, err := u.pick(title, items)
	if err != nil || key == "0" {
		return "", err
	}
	n, _ := strconv.Atoi(key)
	return names[n-1], nil
}

// ---- profiles ----

func (u *ui) profiles() error {
	for {
		cfg, err := u.load()
		if err != nil {
			return err
		}
		rows := buildProfileRows(cfg, u.store, u.claudeBase)
		items := make([]choice, 0, len(rows)+2)
		for i, r := range rows {
			items = append(items, choice{strconv.Itoa(i + 1), fmt.Sprintf("%-14s %-14s %-17s %s", r.Name, r.Provider, r.Type, r.Login)})
		}
		items = append(items, choice{"a", "add a profile"}, choice{"0", "back"})
		key, err := u.pick("profiles", items)
		if err != nil {
			return err
		}
		switch key {
		case "0":
			return nil
		case "a":
			err = u.fail(u.addProfile(cfg))
		default:
			n, _ := strconv.Atoi(key)
			err = u.profile(rows[n-1].Name)
		}
		if err != nil {
			return err
		}
	}
}

func (u *ui) showProfile(cfg *config.Config, name string, pf *config.Profile, pr *config.Provider) {
	u.printf("\nprofile %s\n", name)
	u.printf("  provider:        %s (%s)\n", pf.Provider, pr.Type)
	u.printf("  login:           %s\n", loginState(u.store, pr))
	if pr.Type == config.ProviderTypeClaude {
		u.printf("  the real Claude Code: nothing goes through ccgp\n")
		return
	}
	u.printf("  default_model:   %s\n", dash(pf.DefaultModel))
	u.printf("  context_window:  %s\n", dash(zeroDash(pf.ContextWindow)))
	u.printf("  compact_percent: %s\n", dash(zeroDash(pf.CompactPercent)))
	if pf.Proxy == "" {
		u.printf("  proxy:           global (%s)\n", cfg.ProxyChoice(name))
	} else {
		u.printf("  proxy:           %s (its own)\n", cfg.ProxyChoice(name))
	}
	u.printf("  models:          %d rule(s)\n", len(pf.Models))
	for i, r := range pf.Models {
		if i == 6 {
			u.printf("    ...\n")
			break
		}
		if r.ContextWindow > 0 {
			u.printf("    %s -> %s (%d)\n", r.Match, r.Model, r.ContextWindow)
		} else {
			u.printf("    %s -> %s\n", r.Match, r.Model)
		}
	}
}

func zeroDash(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func (u *ui) profile(name string) error {
	for {
		cfg, err := u.load()
		if err != nil {
			return err
		}
		pf, ok := cfg.Profiles[name]
		if !ok {
			return nil
		}
		pr := cfg.Providers[pf.Provider]
		u.showProfile(cfg, name, pf, pr)
		items := []choice{
			{"1", "set default_model"}, {"2", "set context_window"}, {"3", "set compact_percent"},
			{"4", "set proxy"}, {"5", "move to another provider"}, {"r", "remove the profile"}, {"0", "back"},
		}
		if pr.Type == config.ProviderTypeClaude {
			items = []choice{{"r", "remove the profile"}, {"0", "back"}}
		}
		key, err := u.pick("", items)
		if err != nil {
			return err
		}
		switch key {
		case "0":
			return nil
		case "1":
			err = u.fail(u.editKey(cfg, "default_model", pf.DefaultModel, func(text, v string) (string, error) {
				return config.SetProfileKeyText(text, name, "default_model", v)
			}))
		case "2":
			err = u.fail(u.editKey(cfg, "context_window", zeroDash(pf.ContextWindow), func(text, v string) (string, error) {
				return config.SetProfileKeyText(text, name, "context_window", v)
			}))
		case "3":
			err = u.fail(u.editKey(cfg, "compact_percent", zeroDash(pf.CompactPercent), func(text, v string) (string, error) {
				return config.SetProfileKeyText(text, name, "compact_percent", v)
			}))
		case "4":
			err = u.fail(u.editProxy(cfg, name))
		case "5":
			err = u.fail(u.moveProfile(cfg, name, pf.Provider))
		case "r":
			var removed bool
			removed, err = u.removeProfile(cfg, name)
			if err == nil && removed {
				return nil
			}
		}
		if err != nil {
			return err
		}
	}
}

func (u *ui) editProxy(cfg *config.Config, name string) error {
	line, err := u.ask("proxy URL (socks5://, http://), none, or inherit (Enter keeps)", "")
	if err != nil {
		return err
	}
	if line == "" {
		u.printf("unchanged\n")
		return nil
	}
	proxy, err := proxyValue(cfg, line, name)
	if err != nil {
		return err
	}
	return u.apply(cfg, func(text string) (string, error) { return config.SetProfileProxyText(text, name, proxy) })
}

func (u *ui) moveProfile(cfg *config.Config, name, current string) error {
	var others []string
	for _, n := range providerNames(cfg) {
		if n != current {
			others = append(others, n)
		}
	}
	to, err := u.chooseOne("move "+name+" to which provider?", others)
	if err != nil || to == "" {
		return err
	}
	return u.apply(cfg, func(text string) (string, error) { return config.SetProfileKeyText(text, name, "provider", to) })
}

// removeProfile asks twice (the profile, then what else to delete) and removes
// it as `ccgp --profile rm` does. removed tells whether it is gone.
func (u *ui) removeProfile(cfg *config.Config, name string) (removed bool, err error) {
	yes, err := u.confirm("remove profile " + name + "?")
	if err != nil || !yes {
		if err == nil {
			u.printf("aborted\n")
		}
		return false, err
	}
	purge, err := u.confirm("also delete its Claude dir and the logins nothing else uses?")
	if err != nil {
		return false, err
	}
	err = profileRemove(u.out, profileRmOpts{Name: name, Yes: true, Purge: purge}, cfg.ConfigFile, cfg, u.store, u.claudeBase, nil)
	if err != nil {
		return false, u.fail(err)
	}
	return true, nil
}

// addProfile adds a profile in one of the three ways of `ccgp --profile add`.
func (u *ui) addProfile(cfg *config.Config) error {
	key, err := u.pick("add a profile", []choice{
		{"1", "another account of the kind a profile has (own login, copies its settings)"},
		{"2", "another profile on a login that exists (shares its provider)"},
		{"3", "a new backend (a new provider and its first profile)"},
		{"0", "cancel"},
	})
	if err != nil || key == "0" {
		return err
	}
	var opts profileAddOpts
	base, claude := "", false
	switch key {
	case "1":
		src, err := u.chooseOne("copy which profile?", profileNames(cfg))
		if err != nil || src == "" {
			return err
		}
		opts.Like, base = src, src
		claude = isClaudeProfile(cfg, cfg.Profiles[src])
	case "2":
		prov, err := u.chooseOne("on which provider?", providerNames(cfg))
		if err != nil || prov == "" {
			return err
		}
		opts.Provider, base = prov, prov
	default:
		return u.addBackend(cfg)
	}
	return u.finishAdd(cfg, opts, base, claude)
}

// backendNames are the default profile names for a new backend of each type.
var backendNames = map[string]string{
	config.ProviderTypeCodex: "codex", config.ProviderTypeCodexAPI: "codex_api",
	config.ProviderTypeAnthropicCompat: "custom", config.ProviderTypeClaude: "work",
}

// addBackend asks for the type of a new provider, then adds it with a profile.
func (u *ui) addBackend(cfg *config.Config) error {
	key, err := u.pick("the kind of backend", []choice{
		{"1", "codex: your ChatGPT subscription (OAuth)"},
		{"2", "codex_api: an OpenAI-compatible API with a key"},
		{"3", "anthropic_compat: an Anthropic-style endpoint (z.ai and others)"},
		{"4", "claude: the real Claude Code with an account of its own"},
		{"0", "cancel"},
	})
	if err != nil || key == "0" {
		return err
	}
	typ := map[string]string{"1": config.ProviderTypeCodex, "2": config.ProviderTypeCodexAPI,
		"3": config.ProviderTypeAnthropicCompat, "4": config.ProviderTypeClaude}[key]
	opts := profileAddOpts{Type: typ}
	switch typ {
	case config.ProviderTypeAnthropicCompat:
		if opts.BaseURL, err = u.ask("base URL of the endpoint, e.g. https://api.z.ai/api/anthropic", ""); err != nil {
			return err
		}
		if opts.BaseURL == "" {
			u.printf("a base URL is required for %s\n", typ)
			return nil
		}
	case config.ProviderTypeCodexAPI:
		if opts.BaseURL, err = u.ask("base URL (Enter: https://api.openai.com/v1)", ""); err != nil {
			return err
		}
	}
	return u.finishAdd(cfg, opts, backendNames[typ], typ == config.ProviderTypeClaude)
}

// finishAdd asks for the name and, unless the profile is a Claude one, a
// catch-all model rule; then adds the profile.
func (u *ui) finishAdd(cfg *config.Config, opts profileAddOpts, base string, claude bool) error {
	name, err := u.ask("profile name", config.NextFreeName(base, func(n string) bool { return nameTaken(cfg, n) }))
	if err != nil {
		return err
	}
	opts.Name = name
	if !claude {
		model, err := u.ask("catch-all model name (Enter: no new rule)", "")
		if err != nil {
			return err
		}
		if model != "" {
			window, err := u.ask("its context window in tokens", strconv.Itoa(defaultCatchallWindow))
			if err != nil {
				return err
			}
			n, perr := strconv.ParseInt(window, 10, 64)
			if perr != nil || n < 1 {
				return fmt.Errorf("the context window must be a positive number, got %q", window)
			}
			opts.Model, opts.HasModel, opts.Window, opts.HasWindow = model, true, n, true
		}
	}
	return profileAdd(u.out, opts, cfg.ConfigFile, cfg, u.store)
}

// ---- providers ----

func (u *ui) providers() error {
	for {
		cfg, err := u.load()
		if err != nil {
			return err
		}
		names := sortedKeys(cfg.Providers)
		items := make([]choice, 0, len(names)+2)
		for i, n := range names {
			pr := cfg.Providers[n]
			label := fmt.Sprintf("%-14s %-17s %s", n, pr.Type, strings.Join(profilesOf(cfg, n), ", "))
			if len(pr.Peak) > 0 {
				label += "  [peak hours]"
			}
			items = append(items, choice{strconv.Itoa(i + 1), label})
		}
		items = append(items, choice{"a", "add a provider (with its first profile)"}, choice{"0", "back"})
		key, err := u.pick("providers", items)
		if err != nil {
			return err
		}
		switch key {
		case "0":
			return nil
		case "a":
			err = u.fail(u.addBackend(cfg))
		default:
			n, _ := strconv.Atoi(key)
			err = u.provider(names[n-1])
		}
		if err != nil {
			return err
		}
	}
}

func (u *ui) showProvider(cfg *config.Config, name string, pr *config.Provider) {
	u.printf("\nprovider %s\n", name)
	u.printf("  type:        %s\n", pr.Type)
	if pr.Type != config.ProviderTypeClaude {
		u.printf("  base_url:    %s\n", dash(pr.BaseURL))
		u.printf("  api_key_env: %s\n", dash(pr.APIKeyEnv))
	}
	u.printf("  login:       %s\n", loginState(u.store, pr))
	if len(pr.EffortMap) > 0 {
		keys := make([]string, 0, len(pr.EffortMap))
		for k := range pr.EffortMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+pr.EffortMap[k])
		}
		u.printf("  effort_map:  %s\n", strings.Join(parts, " "))
	}
	if pr.Type != config.ProviderTypeClaude {
		u.printf("  peak hours:  %s\n", u.peakLine(name, pr))
	}
	u.printf("  profiles:    %s\n", dash(strings.Join(profilesOf(cfg, name), ", ")))
}

// peakLine sums up a provider's peak hours and whether it is peak now.
func (u *ui) peakLine(name string, pr *config.Provider) string {
	s, err := pr.PeakSchedule()
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if s == nil {
		return "none"
	}
	windows := make([]string, 0, len(s.Windows))
	for _, w := range s.Windows {
		windows = append(windows, w.String())
	}
	now := u.now()
	st := s.At(now)
	state := "open now"
	if _, paused := u.pauses.Until(name, now); st.In && paused {
		state = "peak now, paused"
	} else if st.In {
		state = "PEAK NOW"
	}
	return fmt.Sprintf("%s (%s time); %s", strings.Join(windows, ", "), s.ZoneName(), state)
}

func (u *ui) provider(name string) error {
	for {
		cfg, err := u.load()
		if err != nil {
			return err
		}
		pr, ok := cfg.Providers[name]
		if !ok {
			return nil
		}
		u.showProvider(cfg, name, pr)
		var items []choice
		switch pr.Type {
		case config.ProviderTypeClaude:
		case config.ProviderTypeCodex:
			items = append(items, choice{"1", "set base_url"})
		default:
			items = append(items, choice{"1", "set base_url"}, choice{"2", "set api_key_env"})
		}
		if pr.Type != config.ProviderTypeClaude {
			items = append(items, choice{"3", "peak hours"})
		}
		items = append(items, choice{"0", "back"})
		key, err := u.pick("", items)
		if err != nil {
			return err
		}
		switch key {
		case "0":
			return nil
		case "1":
			err = u.fail(u.editKey(cfg, "base_url", pr.BaseURL, func(text, v string) (string, error) {
				return config.SetProviderKeyText(text, name, "base_url", v)
			}))
		case "2":
			err = u.fail(u.editKey(cfg, "api_key_env", pr.APIKeyEnv, func(text, v string) (string, error) {
				return config.SetProviderKeyText(text, name, "api_key_env", v)
			}))
		case "3":
			err = u.peakMenu(name)
		}
		if err != nil {
			return err
		}
	}
}

// peakMenu is `ccgp --peak <provider> ...` as questions.
func (u *ui) peakMenu(provider string) error {
	show := true
	for {
		if show {
			u.printf("\n")
			if err := u.fail(peak(u.out, u.now(), u.pauses, []string{provider})); err != nil {
				return err
			}
		}
		key, err := u.pick("peak hours of "+provider, []choice{
			{"1", "set the windows (replaces them all)"}, {"2", "add windows"}, {"3", "remove windows by number"},
			{"4", "set the time zone"}, {"5", "clear the peak hours"},
			{"6", "pause: let requests through for a while"}, {"7", "resume: end the pause"}, {"0", "back"},
		})
		if err != nil {
			return err
		}
		var verb, prompt string
		required := true
		switch key {
		case "0":
			return nil
		case "1":
			verb, prompt = "set", "windows, e.g. mon-fri 14:00-18:00 (a window is [days] HH:MM-HH:MM)"
		case "2":
			verb, prompt = "add", "windows to add, e.g. sat,sun 10:00-12:00"
		case "3":
			verb, prompt = "rm", "numbers of the windows to remove, e.g. 2"
		case "4":
			verb, prompt = "tz", "time zone: a name (Asia/Shanghai), an offset (+08:00), UTC, or local"
		case "5":
			verb = "clear"
		case "6":
			verb, prompt, required = "pause", "length (90m, 2h, 1d; Enter: until the running peak ends)", false
		case "7":
			verb = "resume"
		}
		var words []string
		if prompt != "" {
			line, err := u.ask(prompt, "")
			if err != nil {
				return err
			}
			if line == "" && required {
				u.printf("nothing entered\n")
				show = false
				continue
			}
			words = strings.Fields(line)
		}
		err = peak(u.out, u.now(), u.pauses, append([]string{provider, verb}, words...))
		if err != nil {
			if err := u.fail(err); err != nil {
				return err
			}
		}
		// set, add, rm, tz and clear print the new state themselves.
		show = err != nil || verb == "pause" || verb == "resume"
	}
}
