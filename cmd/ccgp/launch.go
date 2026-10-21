package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/claude-code-proxy/proxy/internal/claudehome"
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
	configDir, err := prepareClaudeHome(profile)
	if err != nil {
		return err
	}
	cmd := exec.Command(claude, claudeArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = compactEnv(
		claudeEnv(os.Environ(), daemon.BaseURL(cfg.Host, cfg.Port)+"/call/"+token, configDir),
		cfg.Profiles[profile], mainModel(claudeArgs, os.Getenv, configDir))

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

// compactEnv tells Claude Code the real context size of the backend model that
// serves the session's main Claude model (mainModel), so it compacts before
// that limit instead of at its own model window. Claude Code takes one window
// per process; models on other rules (e.g. a smaller Haiku) are not covered.
// Variables the user already set win.
func compactEnv(env []string, pf *config.Profile, mainModel string) []string {
	window := pf.ContextWindowFor(mainModel)
	if pf == nil || window <= 0 {
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
		env = append(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW="+strconv.FormatInt(window, 10))
	}
	if pf.CompactPercent > 0 && !has("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE") {
		env = append(env, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE="+strconv.FormatInt(pf.CompactPercent, 10))
	}
	return env
}

// claudeEnv points Claude Code at the proxy and drops conflicting Anthropic creds.
//
// No official credential may reach the session: every endpoint/credential
// variable of the parent is dropped and CLAUDE_CONFIG_DIR points at the
// profile's isolated directory (see claudehome).
func claudeEnv(env []string, baseURL, configDir string) []string {
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if claudehome.IsAuthEnv(k) || strings.EqualFold(k, "CLAUDE_CONFIG_DIR") {
			continue
		}
		out = append(out, kv)
	}
	// Auth is the URL token; Claude Code still requires a credential to be set.
	out = append(out, "ANTHROPIC_BASE_URL="+baseURL, "ANTHROPIC_AUTH_TOKEN=ccgp", "CLAUDE_CONFIG_DIR="+configDir)
	return withDefaults(out, gatewayDefaults)
}

// gatewayDefaults are Claude Code's documented opt-outs for features that
// cannot work behind a non-Anthropic gateway. They only silence startup notices.
var gatewayDefaults = [][2]string{
	// claude.ai connectors need a claude.ai login, which the gateway token replaces.
	{"ENABLE_CLAUDEAI_MCP_SERVERS", "false"},
	// Server-side auto-mode classifier checks need Anthropic's API; use the local classifier.
	{"CLAUDE_CODE_AUTO_MODE_SERVER", "0"},
}

// withDefaults appends name=value for each pair the environment does not set.
func withDefaults(env []string, defaults [][2]string) []string {
	for _, d := range defaults {
		set := false
		for _, kv := range env {
			if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, d[0]) {
				set = true
				break
			}
		}
		if !set {
			env = append(env, d[0]+"="+d[1])
		}
	}
	return env
}

// claudeHomeBase holds the isolated Claude Code config dirs, one per profile.
func claudeHomeBase() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cc-go-proxy", "claude"), nil
}

// prepareClaudeHome creates the profile's isolated Claude Code config dir
// (no official credentials) mirrored from the user's own one, and returns it.
func prepareClaudeHome(profile string) (string, error) {
	base, err := claudeHomeBase()
	if err != nil {
		return "", err
	}
	src, srcState, err := officialClaudeDir(base)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(base, profile)
	if err := claudehome.Prepare(src, srcState, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// officialClaudeDir returns the user's own Claude Code config dir and its
// state file. base is the root of the isolated profile dirs.
func officialClaudeDir(base string) (dir, state string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	env := func(k string) string {
		v := os.Getenv(k)
		// A nested launch inherits an isolated dir: use the real one.
		if k == "CLAUDE_CONFIG_DIR" && strings.HasPrefix(strings.ToLower(filepath.Clean(v)), strings.ToLower(base)) {
			return ""
		}
		return v
	}
	dir, state = claudehome.SourceDir(env, home)
	return dir, state, nil
}

// mainModel returns the Claude model name the session's main conversation will
// use, in Claude Code's own precedence: --model, ANTHROPIC_MODEL, the "model"
// of the (mirrored) settings.json, else the default alias "sonnet".
func mainModel(args []string, getenv func(string) string, configDir string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--model="); ok && v != "" {
			return v
		}
		if a == "--model" && i+1 < len(args) {
			return args[i+1]
		}
	}
	if v := getenv("ANTHROPIC_MODEL"); v != "" {
		return v
	}
	if b, err := os.ReadFile(filepath.Join(configDir, "settings.json")); err == nil {
		var s struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(b, &s) == nil && s.Model != "" {
			return s.Model
		}
	}
	return "sonnet"
}
