package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

// More screens of `ccgp --interactive`: the config and its connections, the
// model mappings, the sessions and the server. Each does what the matching
// command does: in this process where the command is an edit of config.ktav, by
// running the command (app.run) where it is not.

// Ids of the items that only move around the interface; the parity test lets
// them stand without a command.
const (
	navQuit   = "nav:quit"
	navCancel = "nav:cancel"
)

// action is an item that runs `ccgp args…` and shows what it printed.
func action(id, label, title string, args ...string) menuItem {
	return menuItem{id: id, label: label, run: func(a *app) tea.Cmd { return a.run(title, args, false) }}
}

// confirmed is an item that asks first, then runs `ccgp args…`.
func confirmed(id, label, question, title string, terminal bool, args ...string) menuItem {
	return menuItem{id: id, label: label, run: func(a *app) tea.Cmd {
		a.push(&confirmScreen{head: title, question: question, then: func(a *app) tea.Cmd { return a.run(title, args, terminal) }})
		return nil
	}}
}

// commandInput asks for a line and then runs the command that args builds from it.
func commandInput(head, label, hint, initial, title string, args func(v string) ([]string, error)) *input {
	var in *input
	in = newInput(head, label, hint, initial, func(a *app, v string) (screen, error) {
		list, err := args(v)
		if err != nil {
			return nil, err
		}
		in.pending = a.run(title, list, false)
		return nil, nil
	})
	return in
}

// ---- config and connections ----

// connectionsDo runs `ccgp --connections …` in this process and takes the
// config.ktav line of its answer as the message.
func (a *app) connectionsDo(args ...string) error {
	var buf bytes.Buffer
	if err := connections(&buf, args); err != nil {
		return err
	}
	if err := a.reload(); err != nil {
		return err
	}
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, "config.ktav") {
			a.msg = l
			if strings.HasPrefix(l, "config.ktav updated") {
				a.msg = "config.ktav updated, backup kept"
			}
		}
	}
	return nil
}

func configMenu() *menu {
	const head = "Config and connections"
	return &menu{
		head: func(*app) string { return head },
		info: func(a *app) string {
			var buf bytes.Buffer
			if err := connections(&buf, nil); err != nil {
				return err.Error()
			}
			return strings.TrimRight(buf.String(), "\n")
		},
		items: func(a *app) []menuItem {
			field := func(id, hint, initial string, args func(v string) []string) menuItem {
				return menuItem{id: id, label: fmt.Sprintf("%-8s %s", id, dash(initial)), run: push(func() screen {
					return newInput(head, id, hint, initial, func(a *app, v string) (screen, error) {
						return nil, a.connectionsDo(args(v)...)
					})
				})}
			}
			count := func(key string) func(string) []string {
				return func(v string) []string {
					if v == "" {
						v = "0"
					}
					return []string{key, v}
				}
			}
			return []menuItem{
				field("active", "connections per backend that take new requests (0 or empty: the default, 7)", zeroDash(a.cfg.ConnectionsActive), count("active")),
				field("total", "active plus the ones still finishing older requests (0 or empty: the default, 10)", zeroDash(a.cfg.ConnectionsTotal), count("total")),
				field("proxy", "the global proxy: socks5://host:port, http://host:port, https://… ; empty removes it. A provider's or a profile's own proxy replaces it", a.cfg.ConnectionsProxy, func(v string) []string {
					if v == "" {
						v = "off"
					}
					return []string{"proxy", v}
				}),
				confirmed("reset", "reset config.ktav to the default…", "Write the default config? The old one is kept as a backup.", "Reset the config", false, "--config", "reset"),
			}
		},
	}
}

// ---- models ----

func modelsMenu() *menu {
	return &menu{
		head: func(*app) string { return "Models" },
		info: func(*app) string {
			return styDim.Render("where Claude's models go in each profile, and the newest models of the backend")
		},
		items: func(a *app) []menuItem {
			items := []menuItem{
				action("show", "show the mapping of every profile", "Models", "--models"),
				confirmed("apply", "write the newest models into every profile…", "Apply the newest models to every profile?", "Models apply", false, "--models", "apply"),
			}
			for _, n := range profileNames(a.cfg) {
				name := n
				if isClaudeProfile(a.cfg, a.cfg.Profiles[name]) {
					continue
				}
				items = append(items, menuItem{id: "profile", label: name, run: push(func() screen { return modelsProfileMenu(name) })})
			}
			return items
		},
	}
}

func modelsProfileMenu(name string) *menu {
	head := "Models of " + name
	return &menu{
		head: func(*app) string { return head },
		info: func(a *app) string { return profileInfo(a, name) },
		items: func(a *app) []menuItem {
			return []menuItem{
				action("show", "show the mapping", head, "--models", name),
				confirmed("apply", "write the newest models into the profile…", "Apply the newest models to "+name+"?", head, false, "--models", "apply", name),
				{id: "which", label: "where does a model name go?", run: push(func() screen {
					return commandInput(head, "model", "a Claude model name, e.g. claude-opus-4-1", "", "Where a model goes", func(v string) ([]string, error) {
						if v == "" {
							return nil, errors.New("enter a model name")
						}
						return []string{"--which", name, v}, nil
					})
				})},
			}
		},
	}
}

// ---- sessions ----

// countInput asks how many sessions, and runs the command with it; flag (when
// not empty) goes after the count.
func countInput(title, flag string, args ...string) *input {
	return commandInput(title, "how many", "the latest N sessions of this directory (empty: 10)", "", title, func(v string) ([]string, error) {
		list := append([]string{}, args...)
		if v != "" {
			if n, err := strconv.Atoi(v); err != nil || n < 1 {
				return nil, fmt.Errorf("a positive number, got %q", v)
			}
			list = append(list, v)
		}
		list = append(list, strings.Fields(flag)...)
		return list, nil
	})
}

// sessionModeMenu asks what to do with a session the destination already has
// and that went on differently, then how many sessions.
func sessionModeMenu(title string, args ...string) *menu {
	item := func(id, label, flag string) menuItem {
		return menuItem{id: id, label: label, run: push(func() screen { return countInput(title, flag, args...) })}
	}
	return &menu{
		head: func(*app) string { return title },
		info: func(*app) string {
			return styDim.Render("a session that went on differently on both sides: what to do with it?")
		},
		items: func(*app) []menuItem {
			return []menuItem{
				item("copy", "copy; leave a diverged session as it is", ""),
				item("fork", "copy; keep a diverged session as a new session (--fork)", "--fork"),
				item("force", "copy; replace a diverged one, the old file is kept (--force)", "--force"),
				item("forkforce", "copy; keep a diverged one as a new session, written afresh (--fork --force)", "--fork --force"),
			}
		},
	}
}

func sessionsMenu() *menu {
	return &menu{
		head: func(*app) string { return "Sessions" },
		info: func(*app) string {
			return styDim.Render("copy the latest sessions of the current directory between your Claude Code and a profile")
		},
		items: func(a *app) []menuItem {
			return []menuItem{
				{id: "import", label: "import: from your Claude Code into a profile", run: func(a *app) tea.Cmd {
					a.push(chooseNameMenu("Import into which profile?", profileNames, func(a *app, n string) screen {
						return sessionModeMenu("Import into "+n, "--import", n)
					}))
					return nil
				}},
				{id: "export", label: "export: from a profile into your Claude Code", run: func(a *app) tea.Cmd {
					a.push(chooseNameMenu("Export which profile?", profileNames, func(a *app, n string) screen {
						return sessionModeMenu("Export "+n, "--export", n)
					}))
					return nil
				}},
				{id: "send", label: "send: from one profile to another", run: func(a *app) tea.Cmd {
					a.push(chooseNameMenu("From which profile?", profileNames, func(a *app, from string) screen {
						return chooseNameMenu("To which profile?", profileNames, func(a *app, to string) screen {
							return sessionModeMenu("Send "+from+" to "+to, "--send", from, to)
						})
					}))
					return nil
				}},
			}
		},
	}
}

// ---- server ----

func serverMenu() *menu {
	return &menu{
		head: func(*app) string { return "Server" },
		info: func(*app) string {
			return styDim.Render("the background server, its log and the checks; each item runs the command of the same name")
		},
		items: func(a *app) []menuItem {
			return []menuItem{
				action("status", "status", "Server status", "--status"),
				action("start", "start", "Start the server", "--start"),
				action("stop", "stop", "Stop the server", "--stop"),
				action("restart", "restart", "Restart the server", "--restart"),
				action("logs", "log (the last lines)", "Server log", "--logs"),
				action("doctor", "check everything (doctor)", "Doctor", "--doctor"),
				action("version", "version", "Version", "--version"),
				action("check_update", "look for a newer version", "Update check", "--update", "--check"),
				confirmed("update", "update ccgp to the latest release…", "Replace ccgp and ccgpy with the latest release?", "Update", true, "--update"),
			}
		},
	}
}

// ---- logins, in a provider's menu ----

// loginItems are the items of a provider's menu that sign in and out.
func loginItems(name string, pr *config.Provider) []menuItem {
	items := []menuItem{{id: "login", label: "log in", run: func(a *app) tea.Cmd {
		return a.run("Log in to "+name, []string{"--login", name}, true)
	}}}
	if pr.Type == config.ProviderTypeCodex {
		items = append(items, menuItem{id: "login_device", label: "log in with a device code", run: func(a *app) tea.Cmd {
			return a.run("Log in to "+name, []string{"--login", name, "--device"}, true)
		}})
	}
	return append(items, confirmed("logout", "log out…", "Forget the login of "+name+"? The config stays.", "Log out of "+name, false, "--logout", name, "--yes"))
}
