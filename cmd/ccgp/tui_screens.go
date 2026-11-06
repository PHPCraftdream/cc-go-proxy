package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

// The screens of `ccgp --interactive`. Each acts through the code the commands
// use (config.EditFile and its editors, profileAdd, profileRemove, peak), so a
// change made here is the change the command makes.

func push(s func() screen) func(a *app) tea.Cmd {
	return func(a *app) tea.Cmd {
		a.push(s())
		return nil
	}
}

func mainMenu() *menu {
	return &menu{
		head: func(*app) string { return "ccgp" },
		info: func(a *app) string { return styDim.Render(a.cfg.ConfigFile) },
		items: func(a *app) []menuItem {
			return []menuItem{
				{id: "profiles", label: fmt.Sprintf("Profiles (%d)", len(a.cfg.Profiles)), run: push(func() screen { return profilesMenu() })},
				{id: "providers", label: fmt.Sprintf("Providers (%d)", len(a.cfg.Providers)), run: push(func() screen { return providersMenu() })},
				{id: "peak", label: "Peak hours", run: push(func() screen { return peakListMenu() })},
				{id: "config", label: "Config and connections", run: push(func() screen { return configMenu() })},
				{id: "models", label: "Models", run: push(func() screen { return modelsMenu() })},
				{id: "sessions", label: "Sessions", run: push(func() screen { return sessionsMenu() })},
				{id: "server", label: "Server", run: push(func() screen { return serverMenu() })},
				{id: navQuit, label: "Quit", run: func(a *app) tea.Cmd {
					a.quit = true
					return tea.Quit
				}},
			}
		},
	}
}

// ---- profiles ----

func profilesMenu() *menu {
	return &menu{
		head: func(*app) string { return "Profiles" },
		items: func(a *app) []menuItem {
			var items []menuItem
			for _, r := range buildProfileRows(a.cfg, a.store, a.claudeBase) {
				name := r.Name
				items = append(items, menuItem{
					id: "profile", label: fmt.Sprintf("%-14s %-14s %-17s %s", r.Name, r.Provider, r.Type, r.Login),
					run: push(func() screen { return profileMenu(name) }),
				})
			}
			return append(items, menuItem{id: "add", label: "+ add a profile", hot: "a", run: push(func() screen { return addProfileMenu() })})
		},
	}
}

func profileInfo(a *app, name string) string {
	pf, ok := a.cfg.Profiles[name]
	if !ok {
		return "this profile is gone"
	}
	pr := a.cfg.Providers[pf.Provider]
	var b strings.Builder
	fmt.Fprintf(&b, "provider  %s (%s)\nlogin     %s", pf.Provider, pr.Type, loginState(a.store, pr))
	if config.IsOwnType(pr.Type) {
		if pr.Type == config.ProviderTypeClaudeAPI {
			fmt.Fprintf(&b, "\nserver    %s", pr.EffectiveBaseURL())
		}
		b.WriteString("\nthe real Claude Code: nothing goes through ccgp")
		return b.String()
	}
	choice := a.cfg.ProxyChoice(name).String()
	proxy := "global (" + choice + ")"
	switch a.cfg.ProxyLevel(name) {
	case config.ProxyLevelProfile:
		proxy = choice + " (its own)"
	case config.ProxyLevelProvider:
		proxy = choice + " (of provider " + pf.Provider + ")"
	}
	fmt.Fprintf(&b, "\nproxy     %s\nmodels    %d rule(s)", proxy, len(pf.Models))
	for i, r := range pf.Models {
		if i == 6 {
			b.WriteString("\n  ...")
			break
		}
		if r.ContextWindow > 0 {
			fmt.Fprintf(&b, "\n  %s -> %s (%d)", r.Match, r.Model, r.ContextWindow)
		} else {
			fmt.Fprintf(&b, "\n  %s -> %s", r.Match, r.Model)
		}
	}
	return b.String()
}

// editKeyInput asks for a new value of one setting, the current one in the field.
func editKeyInput(head, key, hint, current string, set func(text, value string) (string, error)) *input {
	return newInput(head, key, hint, current, func(a *app, v string) (screen, error) {
		return nil, a.edit(func(text string) (string, error) { return set(text, v) })
	})
}

func profileMenu(name string) *menu {
	return &menu{
		head: func(*app) string { return "Profile " + name },
		info: func(a *app) string { return profileInfo(a, name) },
		items: func(a *app) []menuItem {
			pf, ok := a.cfg.Profiles[name]
			if !ok {
				return nil
			}
			head := "Profile " + name
			setKey := func(key, current, hint string) menuItem {
				return menuItem{
					id: key, label: fmt.Sprintf("%-16s %s", key, dash(current)),
					run: push(func() screen {
						return editKeyInput(head, key, hint+"; empty removes the setting", current, func(text, v string) (string, error) {
							return config.SetProfileKeyText(text, name, key, v)
						})
					}),
				}
			}
			remove := menuItem{id: "rm", label: "remove the profile", hot: "r", run: func(a *app) tea.Cmd {
				a.push(removeMenu(name, len(a.stack)-1))
				return nil
			}}
			switch pr := a.cfg.Providers[pf.Provider]; pr.Type {
			case config.ProviderTypeClaude:
				return []menuItem{remove}
			case config.ProviderTypeClaudeAPI:
				// The key and the server belong to the provider, which is this profile's own.
				return []menuItem{
					{id: "base_url", label: fmt.Sprintf("%-16s %s", "base_url", dash(pr.BaseURL)), run: push(func() screen {
						return editKeyInput(head, "base_url", "the server; empty: the official one ("+config.OfficialAnthropicURL+")", pr.BaseURL, func(text, v string) (string, error) {
							return config.SetProviderKeyText(text, pr.Name, "base_url", v)
						})
					})},
					{id: "login", label: "log in: save the API key", run: func(a *app) tea.Cmd {
						return a.run("Log in to "+pr.Name, []string{"--login", pr.Name}, true)
					}},
					remove,
				}
			}
			proxy := menuItem{
				id: "proxy", label: fmt.Sprintf("%-16s %s", "proxy", dash(pf.Proxy)),
				run: push(func() screen {
					return newInput(head, "proxy", "a proxy URL (socks5://…, http://…), none (no proxy at all), or empty (the provider's proxy, else the global one)", pf.Proxy,
						func(a *app, v string) (screen, error) {
							p := ""
							if v != "" {
								var err error
								if p, err = proxyValue(a.cfg, v, name); err != nil {
									return nil, err
								}
							}
							return nil, a.edit(func(text string) (string, error) { return config.SetProfileProxyText(text, name, p) })
						})
				}),
			}
			return []menuItem{
				setKey("default_model", pf.DefaultModel, "the backend model that every Claude model name goes to"),
				setKey("context_window", zeroDash(pf.ContextWindow), "tokens; Claude Code compacts before this"),
				setKey("compact_percent", zeroDash(pf.CompactPercent), "0-100: where Claude Code compacts"),
				proxy,
				{id: "move", label: "move to another provider…", run: push(func() screen { return moveMenu(name, pf.Provider) })},
				remove,
			}
		},
	}
}

// moveMenu lists the providers a profile can be moved to.
func moveMenu(name, current string) *menu {
	return &menu{
		head: func(*app) string { return "Move profile " + name + " to" },
		items: func(a *app) []menuItem {
			var items []menuItem
			for _, n := range providerNames(a.cfg) {
				if n == current {
					continue
				}
				to := n
				items = append(items, menuItem{
					id: "provider", label: fmt.Sprintf("%-14s %s", to, a.cfg.Providers[to].Type),
					run: func(a *app) tea.Cmd {
						err := a.edit(func(text string) (string, error) { return config.SetProfileKeyText(text, name, "provider", to) })
						if err != nil {
							a.errMsg = err.Error()
							return nil
						}
						a.pop()
						return nil
					},
				})
			}
			return items
		},
	}
}

// removeMenu asks how to remove a profile; depth is the number of screens
// below the profile's own, where the list goes on afterwards.
func removeMenu(name string, depth int) *menu {
	remove := func(purge bool) func(a *app) tea.Cmd {
		return func(a *app) tea.Cmd {
			var buf bytes.Buffer
			err := profileRemove(&buf, profileRmOpts{Name: name, Yes: true, Purge: purge}, a.cfg.ConfigFile, a.cfg, a.store, a.claudeBase, nil)
			if err != nil {
				a.errMsg = err.Error()
				return nil
			}
			if err := a.reload(); err != nil {
				a.errMsg = err.Error()
			}
			a.msg = strings.TrimSpace(buf.String())
			a.popTo(depth)
			return nil
		}
	}
	return &menu{
		head: func(*app) string { return "Remove profile " + name },
		items: func(a *app) []menuItem {
			return []menuItem{
				{id: "rm_keep", label: "remove it; keep its Claude dir and logins", run: remove(false)},
				{id: "rm_purge", label: "remove it and delete its Claude dir and the logins nothing else uses (--purge)", run: remove(true)},
				{id: navCancel, label: "cancel", run: func(a *app) tea.Cmd {
					a.pop()
					return nil
				}},
			}
		},
	}
}

// ---- adding a profile ----

// wizard carries the answers of the steps that add a profile; depth is the
// number of screens below the first of them.
type wizard struct {
	opts   profileAddOpts
	base   string // what the suggested name grows from
	claude bool   // a profile of the real Claude Code has no model rules
	depth  int
}

func (w *wizard) finish(a *app) error {
	var buf bytes.Buffer
	if err := profileAdd(&buf, w.opts, a.cfg.ConfigFile, a.cfg, a.store); err != nil {
		return err
	}
	if err := a.reload(); err != nil {
		return err
	}
	a.msg = strings.TrimSpace(buf.String())
	a.popTo(w.depth)
	return nil
}

func (w *wizard) nameInput(a *app) *input {
	suggested := config.NextFreeName(w.base, func(n string) bool { return nameTaken(a.cfg, n) })
	return newInput("Add a profile", "profile name", "letters, digits, - and _; a free name is suggested", suggested,
		func(a *app, v string) (screen, error) {
			if err := config.ValidateProfileName(v); err != nil {
				return nil, err
			}
			if nameTaken(a.cfg, v) {
				return nil, fmt.Errorf("name %q is already in use by a profile or provider", v)
			}
			w.opts.Name = v
			if w.claude {
				return nil, w.finish(a)
			}
			return w.modelInput(), nil
		})
}

func (w *wizard) modelInput() *input {
	return newInput("Add a profile", "catch-all model", "a backend model name for every Claude model (optional; empty: no new rule)", "",
		func(a *app, v string) (screen, error) {
			if v == "" {
				return nil, w.finish(a)
			}
			w.opts.Model, w.opts.HasModel = v, true
			return w.windowInput(), nil
		})
}

func (w *wizard) windowInput() *input {
	return newInput("Add a profile", "context window", "tokens of that model", strconv.Itoa(defaultCatchallWindow),
		func(a *app, v string) (screen, error) {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("the context window must be a positive number, got %q", v)
			}
			w.opts.Window, w.opts.HasWindow = n, true
			return nil, w.finish(a)
		})
}

func addProfileMenu() *menu {
	return &menu{
		head: func(*app) string { return "Add a profile" },
		items: func(a *app) []menuItem {
			return []menuItem{
				{id: "add_like", label: "another account of the kind a profile has: its own login, its settings copied (--like)", run: func(a *app) tea.Cmd {
					w := &wizard{depth: len(a.stack) - 1}
					a.push(chooseNameMenu("Copy which profile?", profileNames, func(a *app, n string) screen {
						w.opts.Like, w.base, w.claude = n, n, isClaudeProfile(a.cfg, a.cfg.Profiles[n])
						return w.nameInput(a)
					}))
					return nil
				}},
				{id: "add_provider", label: "another profile on a login that exists: shares its provider (--provider)", run: func(a *app) tea.Cmd {
					w := &wizard{depth: len(a.stack) - 1}
					a.push(chooseNameMenu("On which provider?", providerNames, func(a *app, n string) screen {
						w.opts.Provider, w.base = n, n
						return w.nameInput(a)
					}))
					return nil
				}},
				{id: "add_type", label: "a new backend: a new provider and its first profile (--type)", run: func(a *app) tea.Cmd {
					a.push(backendKindMenu(&wizard{depth: len(a.stack) - 1}))
					return nil
				}},
			}
		},
	}
}

// chooseNameMenu lists names (profiles, providers) and goes on to the screen
// that next returns for the one picked.
func chooseNameMenu(head string, names func(*config.Config) []string, next func(a *app, name string) screen) *menu {
	return &menu{
		head: func(*app) string { return head },
		items: func(a *app) []menuItem {
			var items []menuItem
			for _, n := range names(a.cfg) {
				name := n
				items = append(items, menuItem{id: "name", label: name, run: func(a *app) tea.Cmd {
					a.push(next(a, name))
					return nil
				}})
			}
			return items
		},
	}
}

// backendKindMenu picks the type of a new provider.
func backendKindMenu(w *wizard) *menu {
	kind := func(label, typ string) menuItem {
		return menuItem{id: "kind_" + typ, label: label, run: func(a *app) tea.Cmd {
			w.opts.Type, w.base, w.claude = typ, backendNames[typ], config.IsOwnType(typ)
			switch typ {
			case config.ProviderTypeAnthropicCompat:
				a.push(w.baseURLInput(true))
			case config.ProviderTypeCodexAPI, config.ProviderTypeClaudeAPI:
				a.push(w.baseURLInput(false))
			default:
				a.push(w.nameInput(a))
			}
			return nil
		}}
	}
	return &menu{
		head: func(*app) string { return "The kind of backend" },
		items: func(*app) []menuItem {
			return []menuItem{
				kind("codex: your ChatGPT subscription (OAuth)", config.ProviderTypeCodex),
				kind("codex_api: an OpenAI-compatible API with a key", config.ProviderTypeCodexAPI),
				kind("anthropic_compat: an Anthropic-style endpoint (z.ai and others)", config.ProviderTypeAnthropicCompat),
				kind("claude: the real Claude Code with an account of its own", config.ProviderTypeClaude),
				kind("claude_api: the real Claude Code on the Claude Platform, with an API key of its own", config.ProviderTypeClaudeAPI),
			}
		},
	}
}

func (w *wizard) baseURLInput(required bool) *input {
	hint := "e.g. https://api.z.ai/api/anthropic"
	switch {
	case w.opts.Type == config.ProviderTypeClaudeAPI:
		hint = "optional; empty: the official server, " + config.OfficialAnthropicURL
	case !required:
		hint = "optional; empty: https://api.openai.com/v1"
	}
	return newInput("Add a backend", "base URL", hint, "", func(a *app, v string) (screen, error) {
		if v == "" && required {
			return nil, fmt.Errorf("a base URL is required for %s", w.opts.Type)
		}
		w.opts.BaseURL = v
		return w.nameInput(a), nil
	})
}

// ---- providers ----

func providersMenu() *menu {
	return &menu{
		head: func(*app) string { return "Providers" },
		items: func(a *app) []menuItem {
			var items []menuItem
			for _, n := range sortedKeys(a.cfg.Providers) {
				name, pr := n, a.cfg.Providers[n]
				label := fmt.Sprintf("%-14s %-17s %s", name, pr.Type, strings.Join(profilesOf(a.cfg, name), ", "))
				if len(pr.Peak) > 0 {
					label += "  [peak hours]"
				}
				items = append(items, menuItem{id: "provider", label: label, run: push(func() screen { return providerMenu(name) })})
			}
			return append(items, menuItem{id: "add", label: "+ add a provider (with its first profile)", hot: "a", run: func(a *app) tea.Cmd {
				a.push(backendKindMenu(&wizard{depth: len(a.stack)}))
				return nil
			}})
		},
	}
}

func providerInfo(a *app, name string) string {
	return providerText(a.cfg, a.store, a.pauses, a.now(), name)
}

func providerMenu(name string) *menu {
	return &menu{
		head: func(*app) string { return "Provider " + name },
		info: func(a *app) string { return providerInfo(a, name) },
		items: func(a *app) []menuItem {
			pr, ok := a.cfg.Providers[name]
			if !ok || pr.Type == config.ProviderTypeClaude {
				return nil
			}
			head := "Provider " + name
			setKey := func(key, current, hint string) menuItem {
				return menuItem{
					id: key, label: fmt.Sprintf("%-14s %s", key, dash(current)),
					run: push(func() screen {
						return editKeyInput(head, key, hint+"; empty removes the setting", current, func(text, v string) (string, error) {
							return config.SetProviderKeyText(text, name, key, v)
						})
					}),
				}
			}
			items := []menuItem{setKey("base_url", pr.BaseURL, "the endpoint")}
			if pr.Type != config.ProviderTypeCodex {
				items = append(items, setKey("api_key_env", pr.APIKeyEnv, "the environment variable that holds the API key"))
			}
			if pr.Type == config.ProviderTypeClaudeAPI {
				return append(items, loginItems(name, pr)...) // no proxy, no peak hours
			}
			proxy := menuItem{
				id: "proxy", label: fmt.Sprintf("%-14s %s", "proxy", dash(pr.Proxy)),
				run: push(func() screen {
					return newInput(head, "proxy", "a proxy URL (socks5://…, http://…), none (no proxy at all), or empty (the global proxy); a profile's own proxy wins", pr.Proxy,
						func(a *app, v string) (screen, error) {
							p := ""
							if v != "" {
								var err error
								if p, err = providerProxyValue(a.cfg, v, name); err != nil {
									return nil, err
								}
							}
							return nil, a.edit(func(text string) (string, error) { return config.SetProviderProxyText(text, name, p) })
						})
				}),
			}
			items = append(items, proxy)
			items = append(items, menuItem{id: "peak", label: "peak hours…", hot: "p", run: push(func() screen { return peakMenu(name) })})
			return append(items, loginItems(name, pr)...)
		},
	}
}

// ---- peak hours ----

// peakListMenu lists the providers, to pick the one whose peak hours to edit.
func peakListMenu() *menu {
	return &menu{
		head: func(*app) string { return "Peak hours: which provider?" },
		items: func(a *app) []menuItem {
			var items []menuItem
			for _, n := range providerNames(a.cfg) {
				name := n
				items = append(items, menuItem{
					id: "provider", label: fmt.Sprintf("%-14s %s", name, peakSummary(a.cfg.Providers[name], name, a.now(), a.pauses)),
					run: push(func() screen { return peakMenu(name) }),
				})
			}
			return items
		},
	}
}

// peakDo runs `ccgp --peak <provider> …` and takes its first line as the answer.
func (a *app) peakDo(args ...string) error {
	var buf bytes.Buffer
	if err := peak(&buf, a.now(), a.pauses, args); err != nil {
		return err
	}
	if err := a.reload(); err != nil {
		return err
	}
	first, _, _ := strings.Cut(buf.String(), "\n")
	if strings.HasPrefix(first, "config.ktav updated") {
		first = "config.ktav updated, backup kept"
	}
	a.msg = first
	return nil
}

func peakPanel(a *app, provider string) string {
	if a.cfg.Providers[provider] == nil {
		return "this provider is gone"
	}
	var buf bytes.Buffer
	if err := showPeak(&buf, a.cfg, a.now(), a.pauses, provider, false); err != nil {
		return err.Error()
	}
	return strings.TrimRight(buf.String(), "\n")
}

func peakMenu(provider string) *menu {
	head := "Peak hours of " + provider
	words := func(id, prompt, hint, initial string, run func(a *app, fields []string) error) menuItem {
		return menuItem{id: id, label: prompt, run: push(func() screen {
			return newInput(head, prompt, hint, initial, func(a *app, v string) (screen, error) {
				return nil, run(a, strings.Fields(v))
			})
		})}
	}
	need := func(fields []string) error {
		if len(fields) == 0 {
			return fmt.Errorf("enter at least one window, like mon-fri 14:00-18:00")
		}
		return nil
	}
	const windowHint = "[days] HH:MM-HH:MM, days like mon-fri or sat,sun (empty: every day); several windows separated by spaces"
	return &menu{
		head: func(*app) string { return head },
		info: func(a *app) string { return peakPanel(a, provider) },
		items: func(a *app) []menuItem {
			pr := a.cfg.Providers[provider]
			if pr == nil {
				return nil
			}
			var current []string
			for _, w := range pr.Peak {
				current = append(current, w.String())
			}
			tz := pr.PeakTZ
			if tz == "" {
				tz = "local"
			}
			return []menuItem{
				words("add", "add windows", windowHint, "", func(a *app, f []string) error {
					if err := need(f); err != nil {
						return err
					}
					return a.peakDo(append([]string{provider, "add"}, f...)...)
				}),
				words("set", "replace all windows", windowHint, strings.Join(current, " "), func(a *app, f []string) error {
					if err := need(f); err != nil {
						return err
					}
					return a.peakDo(append([]string{provider, "set"}, f...)...)
				}),
				{id: "rm", label: "remove a window…", run: func(a *app) tea.Cmd {
					if len(pr.Peak) == 0 {
						a.errMsg = "there are no windows to remove"
						return nil
					}
					a.push(removeWindowMenu(provider))
					return nil
				}},
				words("tz", "time zone", "a name (Asia/Shanghai), an offset (+08:00), UTC, or local (this machine's)", tz, func(a *app, f []string) error {
					if len(f) != 1 {
						return fmt.Errorf("enter one zone")
					}
					return a.peakDo(provider, "tz", f[0])
				}),
				words("pause", "pause", "how long requests go through: 90m, 2h, 1d (empty: until the running peak ends)", "", func(a *app, f []string) error {
					return a.peakDo(append([]string{provider, "pause"}, f...)...)
				}),
				{id: "resume", label: "resume (end the pause)", run: func(a *app) tea.Cmd {
					if err := a.peakDo(provider, "resume"); err != nil {
						a.errMsg = err.Error()
					}
					return nil
				}},
				{id: "clear", label: "clear all peak hours", run: func(a *app) tea.Cmd {
					a.push(&confirmScreen{head: head, question: "Remove all peak hours of " + provider + "?", yes: func(a *app) error {
						return a.peakDo(provider, "clear")
					}})
					return nil
				}},
			}
		},
	}
}

// removeWindowMenu lists the windows of a provider to remove one.
func removeWindowMenu(provider string) *menu {
	return &menu{
		head: func(*app) string { return "Remove which window of " + provider + "?" },
		items: func(a *app) []menuItem {
			var items []menuItem
			if pr := a.cfg.Providers[provider]; pr != nil {
				for i, w := range pr.Peak {
					n := strconv.Itoa(i + 1)
					items = append(items, menuItem{id: "window", label: w.String(), run: func(a *app) tea.Cmd {
						if err := a.peakDo(provider, "rm", n); err != nil {
							a.errMsg = err.Error()
							return nil
						}
						a.pop()
						return nil
					}})
				}
			}
			return items
		},
	}
}
