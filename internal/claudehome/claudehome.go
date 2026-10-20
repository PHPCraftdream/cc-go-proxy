// Package claudehome prepares an isolated Claude Code config directory for a
// ccgp profile. The directory never holds an official (claude.ai / Anthropic
// API) credential, so a gateway session can never pick one up, while the
// user's shared instructions and tooling are mirrored from the real one.
package claudehome

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const credentialsFile = ".credentials.json"

// mirroredFiles are copied from the source config dir on every launch.
var mirroredFiles = []string{"CLAUDE.md", "keybindings.json"}

// linkedDirs are shared with the source config dir via junction/symlink.
var linkedDirs = []string{"agents", "commands", "skills", "output-styles", "plugins"}

// SourceDir returns the Claude Code config dir the user normally runs with:
// $CLAUDE_CONFIG_DIR when set, else ~/.claude. stateFile is its .claude.json.
func SourceDir(env func(string) string, home string) (dir, stateFile string) {
	if d := env("CLAUDE_CONFIG_DIR"); d != "" {
		return d, filepath.Join(d, ".claude.json")
	}
	return filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")
}

// Prepare makes dst an isolated config dir mirrored from src. srcState is the
// source's .claude.json, used once to seed the destination's. It returns an
// error if an official credential file cannot be removed from dst.
func Prepare(src, srcState, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if same(src, dst) {
		return errors.New("claudehome: destination is the user's own Claude Code config dir")
	}

	// A login made inside the isolated session must not persist.
	if err := os.Remove(filepath.Join(dst, credentialsFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("claudehome: remove credentials from %s: %w", dst, err)
	}

	for _, name := range mirroredFiles {
		if err := mirrorFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	if err := mirrorSettings(filepath.Join(src, "settings.json"), filepath.Join(dst, "settings.json")); err != nil {
		return err
	}
	for _, name := range linkedDirs {
		if err := linkDir(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			return fmt.Errorf("claudehome: link %s: %w", name, err)
		}
	}
	return seedState(srcState, filepath.Join(dst, ".claude.json"))
}

func same(a, b string) bool {
	ca, err1 := filepath.Abs(a)
	cb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}

// mirrorFile copies src to dst, or removes dst when src is gone.
func mirrorFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return removeIfExists(dst)
	}
	if err != nil {
		return err
	}
	return writeIfChanged(dst, data)
}

func mirrorSettings(src, dst string) error {
	data, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return removeIfExists(dst)
	}
	if err != nil {
		return err
	}
	clean, err := SanitizeSettings(data)
	if err != nil {
		return fmt.Errorf("claudehome: %s: %w", src, err)
	}
	return writeIfChanged(dst, clean)
}

// authEnvKeys are removed from settings.json's "env": they would re-point the
// session at an official endpoint or credential.
var authEnvKeys = map[string]bool{
	"ANTHROPIC_BASE_URL":            true,
	"ANTHROPIC_API_KEY":             true,
	"ANTHROPIC_AUTH_TOKEN":          true,
	"CLAUDE_CODE_OAUTH_TOKEN":       true,
	"CLAUDE_CODE_USE_BEDROCK":       true,
	"CLAUDE_CODE_USE_VERTEX":        true,
	"CLAUDE_CODE_USE_FOUNDRY":       true,
	"CLAUDE_CODE_USE_ANTHROPIC_AWS": true,
}

// authSettingKeys are top-level settings that select or fetch credentials.
var authSettingKeys = []string{"apiKeyHelper", "forceLoginMethod", "forceLoginOrgUUID", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh"}

// SanitizeSettings removes credential-related entries from a settings.json.
func SanitizeSettings(data []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	for _, k := range authSettingKeys {
		delete(m, k)
	}
	if env, ok := m["env"].(map[string]any); ok {
		for k := range env {
			if authEnvKeys[strings.ToUpper(k)] {
				delete(env, k)
			}
		}
	}
	return json.MarshalIndent(m, "", "  ")
}

// stateKeys are the only .claude.json entries carried over (once, at creation).
var stateKeys = []string{"hasCompletedOnboarding", "theme", "mcpServers", "editorMode", "verbose"}

// seedState creates dst from src's non-account preferences when dst is absent.
// Account data (oauthAccount, userID, project history) is never copied.
func seedState(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	seed := map[string]any{"hasCompletedOnboarding": true}
	if data, err := os.ReadFile(src); err == nil {
		var all map[string]any
		if json.Unmarshal(data, &all) == nil {
			keys := append([]string(nil), stateKeys...)
			sort.Strings(keys)
			for _, k := range keys {
				if v, ok := all[k]; ok {
					seed[k] = v
				}
			}
		}
	}
	data, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

func writeIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// HasCredentials reports whether dir contains an official credential file.
func HasCredentials(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, credentialsFile))
	return err == nil
}

// IsAuthEnv reports whether an environment variable selects an official
// endpoint or credential and must not reach a gateway session.
func IsAuthEnv(name string) bool { return authEnvKeys[strings.ToUpper(name)] }
