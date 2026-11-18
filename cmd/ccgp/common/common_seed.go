package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/cc-go-proxy/internal/claudehome"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	_ "golang.org/x/crypto/x509roots/fallback"
)

// writeJSON prints one pretty-printed, newline-terminated JSON document.
func WriteJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func SortStrings(s []string) { sort.Strings(s) }

// claudeHomeBase holds the isolated Claude Code config dirs, one per profile.
func ClaudeHomeBase() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "claude"), nil
}

// officialClaudeDir returns the user's own Claude Code config dir and its
// state file. base is the root of the isolated profile dirs.
func OfficialClaudeDir(base string) (dir, state string, err error) {
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

func CredsStore() (creds.Store, error) {
	dir, err := creds.DefaultDir()
	return creds.Store{Dir: dir}, err
}

func IsLocal(u string) bool {
	return (&config.Config{OpenAIBaseURL: u}).IsLocalhost()
}

func SortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	SortStrings(out)
	return out
}

func Names[T any](m map[string]T) string {
	if len(m) == 0 {
		return "none"
	}
	return strings.Join(SortedKeys(m), ", ")
}

// claudeLoginState tells whether the Claude dir of a Claude profile under base
// holds a login made inside Claude Code (the file is never read).
func ClaudeLoginState(base, name string) string {
	if claudehome.HasCredentials(filepath.Join(base, name)) {
		return "signed in inside Claude Code"
	}
	return "sign in inside Claude Code (/login)"
}

// loginState describes stored credentials without exposing them.
func LoginState(store creds.Store, pr *config.Provider) string {
	switch pr.Type {
	case config.ProviderTypeClaude:
		base, err := ClaudeHomeBase()
		if err != nil {
			return "error: " + err.Error()
		}
		return ClaudeLoginState(base, pr.Name)
	case config.ProviderTypeCodex:
		t, err := oauth.Store{Path: store.OAuthPath(pr.Name)}.Load()
		switch {
		case errors.Is(err, oauth.ErrNotLoggedIn):
			return "not logged in (ccgp --login " + pr.Name + ")"
		case err != nil:
			return "error: " + err.Error()
		}
		return "logged in, tokens refreshed " + t.LastRefresh.Local().Format("2006-01-02 15:04")
	case config.ProviderTypeCodexAPI, config.ProviderTypeAnthropicCompat, config.ProviderTypeClaudeAPI:
		if pr.APIKeyEnv != "" {
			if os.Getenv(pr.APIKeyEnv) == "" {
				return "$" + pr.APIKeyEnv + " is empty"
			}
			return "key from $" + pr.APIKeyEnv
		}
		k, err := store.APIKey(pr.Name)
		switch {
		case err != nil:
			return "error: " + err.Error()
		case k != "":
			return "API key saved"
		case IsLocal(pr.BaseURL) && pr.Type != config.ProviderTypeClaudeAPI:
			return "no key needed (local)"
		}
		return "no API key (ccgp --login " + pr.Name + ")"
	}
	return "unknown type"
}

// exitError propagates a child exit code without printing.
type ExitError int

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// listenAddr resolves host/port without requiring a fully valid config.
func ListenAddr() (string, string) {
	if cfg, err := config.Load(); err == nil {
		return cfg.Host, cfg.Port
	}
	host, port := os.Getenv("HOST"), os.Getenv("PORT")
	if host == "" {
		host = config.DefaultHost
	}
	if port == "" {
		port = config.DefaultPort
	}
	return host, port
}

// claudeProfileDir is the isolated Claude Code dir of a profile under base.
// It is only safe to delete when the name is valid and the path is a direct
// child of base: the user's own Claude Code dir must never be touched.
func ClaudeProfileDir(base, name string) (string, error) {
	if err := cfgedit.ValidateProfileName(name); err != nil {
		return "", err
	}
	base = filepath.Clean(base)
	p := filepath.Join(base, name)
	if filepath.Dir(p) != base {
		return "", fmt.Errorf("%q is not a profile directory under %s", name, base)
	}
	return p, nil
}

// isClaudeProfile reports whether pf runs the real Claude Code (provider type
// claude or claude_api) and so does not go through ccgp.
func IsClaudeProfile(cfg *config.Config, pf *config.Profile) bool {
	pr := cfg.Providers[pf.Provider]
	return pr != nil && config.IsOwnType(pr.Type)
}

// profileNames lists the profiles of cfg.
func ProfileNames(cfg *config.Config) []string { return SortedKeys(cfg.Profiles) }

// providerNames lists the providers that go through ccgp (a Claude one is not
// a backend a profile can be moved to).
func ProviderNames(cfg *config.Config) []string {
	var out []string
	for _, n := range SortedKeys(cfg.Providers) {
		if !config.IsOwnType(cfg.Providers[n].Type) {
			out = append(out, n)
		}
	}
	return out
}

// profilesOf lists the profiles that use a provider.
func ProfilesOf(cfg *config.Config, provider string) []string {
	var out []string
	for _, n := range SortedKeys(cfg.Profiles) {
		if cfg.Profiles[n].Provider == provider {
			out = append(out, n)
		}
	}
	return out
}

// backendNames are the default profile names for a new backend of each type.
var BackendNames = map[string]string{
	config.ProviderTypeCodex: "codex", config.ProviderTypeCodexAPI: "codex_api",
	config.ProviderTypeAnthropicCompat: "custom", config.ProviderTypeClaude: "work", config.ProviderTypeClaudeAPI: "api",
}

func Dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ZeroDash(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}
