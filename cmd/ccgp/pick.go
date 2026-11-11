package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

// pickArg in place of a profile opens a list to choose one from:
// ccgp . [claude args...]. ccgpy without a profile passes it.
const pickArg = "."

// errNotPicked ends a list closed without a choice.
var errNotPicked = errors.New("no profile chosen")

// launchPicked asks on the terminal which profile to run, then runs it with
// claudeArgs. The choice claude is your own Claude Code, run as plain claude.
func launchPicked(claudeArgs []string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("ccgp . needs a terminal to choose a profile; name one instead: ccgp <profile>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := credsStore()
	if err != nil {
		return err
	}
	base, err := claudeHomeBase()
	if err != nil {
		return err
	}
	picked := ""
	a := &app{cfg: cfg, store: store, claudeBase: base, now: time.Now, run: runSelf, stack: []screen{pickMenu(&picked)}}
	if _, err := tea.NewProgram(a, tea.WithAltScreen()).Run(); err != nil {
		return err
	}
	return runPicked(picked, claudeArgs)
}

// runPicked runs what the list returned: nothing, plain claude or a profile.
func runPicked(picked string, claudeArgs []string) error {
	switch picked {
	case "":
		return errNotPicked
	case claudeEndpointLabel:
		return launchPlainClaude(claudeArgs)
	}
	return launch(picked, claudeArgs)
}

// pickMenu lists the profiles and your own Claude Code; choosing one sets
// *picked and quits.
func pickMenu(picked *string) *menu {
	choose := func(name string) func(a *app) tea.Cmd {
		return func(a *app) tea.Cmd {
			*picked = name
			a.quit = true
			return tea.Quit
		}
	}
	return &menu{
		head: func(*app) string { return "Run Claude Code with which profile?" },
		items: func(a *app) []menuItem {
			var items []menuItem
			for _, r := range buildProfileRows(a.cfg, a.store, a.claudeBase) {
				items = append(items, menuItem{
					id: "profile", label: fmt.Sprintf("%-14s %-14s %-17s %s", r.Name, r.Provider, r.Type, r.Login),
					run: choose(r.Name),
				})
			}
			return append(items, menuItem{
				id: claudeEndpointLabel, label: fmt.Sprintf("%-14s %s", claudeEndpointLabel, "your own Claude Code, as plain claude (no ccgp)"),
				hot: "c", run: choose(claudeEndpointLabel),
			})
		},
	}
}

// launchPlainClaude runs claude with the arguments and the environment as they
// are: what typing claude here does.
func launchPlainClaude(claudeArgs []string) error {
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude not found in PATH")
	}
	return runClaude(claude, claudeArgs, os.Environ())
}
