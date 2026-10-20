package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/daemon"
)

func sortStrings(s []string) { sort.Strings(s) }

// launch: ensure creds + token + server for profile, then run claude with it.
func launch(profile string, claudeArgs []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, ok := cfg.Profiles[profile]; !ok {
		return fmt.Errorf("unknown profile %q (configured: %s)", profile, names(cfg.Profiles))
	}

	token := cfg.Token // env-only mode
	if !cfg.IsEnvMode() {
		if err := login(cfg, cfg.Profiles[profile].Provider, false, false); err != nil {
			return err
		}
		store, err := credsStore()
		if err != nil {
			return err
		}
		if token, err = store.EnsureProfileToken(profile); err != nil {
			return err
		}
	}

	if _, err := ensureServer(cfg, nil); err != nil {
		return err
	}

	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude not found in PATH")
	}
	cmd := exec.Command(claude, claudeArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = compactEnv(
		claudeEnv(os.Environ(), daemon.BaseURL(cfg.Host, cfg.Port)+"/call/"+token),
		cfg.Profiles[profile])

	// Ctrl+C goes to claude (same console group); ccgp must not exit first.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitError(ee.ExitCode())
		}
		return err
	}
	return nil
}

// compactEnv tells Claude Code the backend's real context size so it compacts
// before the backend limit instead of at its own (larger) model window.
// Variables the user already set win.
func compactEnv(env []string, pf *config.Profile) []string {
	if pf == nil || pf.ContextWindow <= 0 {
		return env
	}
	has := func(name string) bool {
		for _, kv := range env {
			if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, name) {
				return true
			}
		}
		return false
	}
	if !has("CLAUDE_CODE_AUTO_COMPACT_WINDOW") {
		env = append(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW="+strconv.FormatInt(pf.ContextWindow, 10))
	}
	if pf.CompactPercent > 0 && !has("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE") {
		env = append(env, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE="+strconv.FormatInt(pf.CompactPercent, 10))
	}
	return env
}

// claudeEnv points Claude Code at the proxy and drops conflicting Anthropic creds.
func claudeEnv(env []string, baseURL string) []string {
	drop := map[string]bool{
		"ANTHROPIC_BASE_URL":   true,
		"ANTHROPIC_API_KEY":    true,
		"ANTHROPIC_AUTH_TOKEN": true,
	}
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[strings.ToUpper(k)] {
			out = append(out, kv)
		}
	}
	// Auth is the URL token; Claude Code still requires a credential to be set.
	return append(out, "ANTHROPIC_BASE_URL="+baseURL, "ANTHROPIC_AUTH_TOKEN=ccgp")
}
