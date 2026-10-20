package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
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
		if err := login(cfg, cfg.Profiles[profile].Provider, false); err != nil {
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
	cmd.Env = claudeEnv(os.Environ(), daemon.BaseURL(cfg.Host, cfg.Port)+"/call/"+token)

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
