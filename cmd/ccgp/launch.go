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

	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/claudehome"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
)

func sortStrings(s []string) { sort.Strings(s) }

// launch: ensure creds + token + server for profile, then run claude with it.
func launch(profile string, claudeArgs []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pf, ok := cfg.Profiles[profile]
	if !ok {
		return fmt.Errorf("unknown profile %q (configured: %s)", profile, names(cfg.Profiles))
	}
	if isClaudeProfile(cfg, pf) {
		return launchOwn(cfg, profile, claudeArgs)
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
	seedProfileSessions(os.Stderr, cfg, profile)
	env := compactEnv(
		claudeEnv(os.Environ(), angel.BaseURL(cfg.Host, cfg.Port)+"/call/"+token, configDir),
		cfg.Profiles[profile], mainModel(claudeArgs, os.Getenv, configDir))
	return runClaude(claude, claudeArgs, env)
}

// runClaude runs claude on the terminal and returns its exit code as exitError.
func runClaude(claude string, args, env []string) error {
	cmd := exec.Command(claude, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env

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

// launchOwn runs the real Claude Code for a profile of type claude or claude_api:
// no server, no proxy, no token. The profile's config dir gets the shared
// settings only (see claudehome.PrepareOwn). A claude profile is signed in by
// Claude Code itself; a claude_api profile gets its API key (and server) from ccgp.
func launchOwn(cfg *config.Config, profile string, claudeArgs []string) error {
	claude, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude not found in PATH")
	}
	pr := cfg.Providers[cfg.Profiles[profile].Provider]
	var extra []string
	if pr.Type == config.ProviderTypeClaudeAPI {
		if extra, err = apiEnv(cfg, pr); err != nil {
			return err
		}
	}
	configDir, err := prepareOwnHome(profile)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(configDir, ".claude.json")); err != nil && pr.Type == config.ProviderTypeClaude {
		fmt.Fprintf(os.Stderr, "ccgp: new Claude profile %q: sign in inside Claude Code (/login); nothing of your own login is copied\n", profile)
	}
	return runClaude(claude, claudeArgs, ownEnv(os.Environ(), configDir, extra...))
}

// apiEnv is the key and server of a claude_api provider for Claude Code's
// environment. The key is asked for when none is saved; the server is set only
// when the provider names one (otherwise Claude Code uses the official one).
func apiEnv(cfg *config.Config, pr *config.Provider) ([]string, error) {
	if err := login(cfg, pr.Name, false, false); err != nil {
		return nil, err
	}
	key := ""
	if pr.APIKeyEnv != "" {
		key = os.Getenv(pr.APIKeyEnv)
	} else {
		store, err := credsStore()
		if err != nil {
			return nil, err
		}
		if key, err = store.APIKey(pr.Name); err != nil {
			return nil, err
		}
	}
	if key == "" {
		return nil, fmt.Errorf("provider %q has no API key (ccgp --login %s)", pr.Name, pr.Name)
	}
	env := []string{"ANTHROPIC_API_KEY=" + key}
	if pr.BaseURL != "" {
		env = append(env, "ANTHROPIC_BASE_URL="+pr.BaseURL)
	}
	return env, nil
}

// ownEnv points Claude Code at the profile's config dir and drops every
// endpoint/credential variable of the parent, so the profile's own account or
// key is the only one it can use; extra (the profile's key and server) is added
// after the drop.
func ownEnv(env []string, configDir string, extra ...string) []string {
	out := make([]string, 0, len(env)+1+len(extra))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if claudehome.IsAuthEnv(k) || strings.EqualFold(k, "CLAUDE_CONFIG_DIR") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, extra...)
	return append(out, "CLAUDE_CONFIG_DIR="+configDir)
}

// The range of CLAUDE_CODE_AUTO_COMPACT_WINDOW that Claude Code takes as is.
// A backend window below the minimum cannot be protected by compaction.
const (
	minCompactWindow = 100_000
	maxCompactWindow = 1_000_000
)

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
		// Claude Code accepts 100k-1M and warns on anything else.
		window = min(max(window, minCompactWindow), maxCompactWindow)
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
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "claude"), nil
}

// prepareClaudeHome creates the profile's isolated Claude Code config dir
// (no official credentials) mirrored from the user's own one, and returns it.
func prepareClaudeHome(profile string) (string, error) {
	base, src, srcState, err := claudeSource()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(base, profile)
	if err := claudehome.Prepare(src, srcState, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// prepareOwnHome creates the config dir of a profile of type claude: the shared
// settings of the user's Claude Code, nothing of its account (claudehome.PrepareOwn).
func prepareOwnHome(profile string) (string, error) {
	base, src, _, err := claudeSource()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(base, profile)
	if err := claudehome.PrepareOwn(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// claudeSource returns the root of the profile dirs and the user's own Claude
// Code dir with its state file.
func claudeSource() (base, src, srcState string, err error) {
	if base, err = claudeHomeBase(); err != nil {
		return "", "", "", err
	}
	if src, srcState, err = officialClaudeDir(base); err != nil {
		return "", "", "", err
	}
	return base, src, srcState, nil
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
