package main

import (
	"errors"
	"os"
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

const interactiveUsage = "usage: ccgp --interactive (or -i)"

// interactiveCmd runs `ccgp --interactive`: a full-screen Bubble Tea interface
// to look at, change and add profiles and providers (tui.go, tui_screens.go).
// Nothing starts it but the flag.
func interactiveCmd(args []string) error {
	if len(args) != 0 {
		return errors.New(interactiveUsage)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("ccgp --interactive needs a terminal: it draws menus and reads keys")
	}
	store, err := credsStore()
	if err != nil {
		return err
	}
	base, err := claudeHomeBase()
	if err != nil {
		return err
	}
	a, err := newApp(store, base, time.Now)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(a, tea.WithAltScreen()).Run()
	return err
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

// backendNames are the default profile names for a new backend of each type.
var backendNames = map[string]string{
	config.ProviderTypeCodex: "codex", config.ProviderTypeCodexAPI: "codex_api",
	config.ProviderTypeAnthropicCompat: "custom", config.ProviderTypeClaude: "work",
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func zeroDash(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}
