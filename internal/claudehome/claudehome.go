// Package claudehome prepares an isolated Claude Code config directory for a
// ccgp profile. The directory never holds an official (claude.ai / Anthropic
// API) credential, so a gateway session can never pick one up, while the
// user's shared instructions and tooling are mirrored from the real one.
// PrepareOwn serves profiles of the real Claude Code: the same mirror, but no
// account state is seeded and the profile's own login is kept.
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

// mirroredDirs are copied (one-way) from the source config dir on every launch.
var mirroredDirs = []string{"agents", "commands", "skills", "output-styles", "plugins"}

// SourceDir returns the Claude Code config dir the user normally runs with:
// $CLAUDE_CONFIG_DIR when set, else ~/.claude. stateFile is its .claude.json.
func SourceDir(env func(string) string, home string) (dir, stateFile string) {
	if d := env("CLAUDE_CONFIG_DIR"); d != "" {
		return d, filepath.Join(d, ".claude.json")
	}
	return filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")
}

// Prepare makes dst an isolated config dir mirrored from src. srcState is the
// source's .claude.json: it seeds the destination's, and a later launch adds the
// preferenceKeys that the destination's lacks. It returns an error if an official
// credential file cannot be removed from dst.
func Prepare(src, srcState, dst string) error {
	if same(src, dst) {
		return errors.New("claudehome: destination is the user's own Claude Code config dir")
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}

	// A login made inside the isolated session must not persist.
	if err := os.Remove(filepath.Join(dst, credentialsFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("claudehome: remove credentials from %s: %w", dst, err)
	}

	if err := mirrorShared(src, dst); err != nil {
		return err
	}
	state := filepath.Join(dst, ".claude.json")
	if err := seedState(srcState, state); err != nil {
		return err
	}
	addPreferences(srcState, state)
	return nil
}

// PrepareOwn makes dst the config dir of a profile that runs the real Claude
// Code with its own login. Only the shared tooling and settings are mirrored
// from src (see mirrorShared): no .claude.json, session or account data is
// carried over, and a login made in dst is kept.
func PrepareOwn(src, dst string) error {
	if same(src, dst) {
		return errors.New("claudehome: destination is the user's own Claude Code config dir")
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	return mirrorShared(src, dst)
}

// mirrorShared copies the instructions, settings (without credential entries),
// agents, commands, skills, output styles and plugins from src to dst.
func mirrorShared(src, dst string) error {
	for _, name := range mirroredFiles {
		if err := mirrorFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	if err := mirrorSettings(filepath.Join(src, "settings.json"), filepath.Join(dst, "settings.json")); err != nil {
		return err
	}
	man := loadManifest(dst)
	for _, name := range mirroredDirs {
		if err := mirrorTree(src, dst, name, man); err != nil {
			return fmt.Errorf("claudehome: %w", err)
		}
	}
	if err := man.save(dst); err != nil {
		return err
	}
	if err := rewritePluginPaths(src, dst); err != nil {
		return fmt.Errorf("claudehome: %w", err)
	}
	return nil
}

func same(a, b string) bool {
	if sameFile(a, b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil && sameFile(resolvedA, resolvedB) {
		return true
	}
	ca, err1 := filepath.Abs(a)
	cb, err2 := filepath.Abs(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}

func sameFile(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
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

// stateKeys are the .claude.json entries carried over once, at creation.
var stateKeys = []string{"hasCompletedOnboarding", "theme", "mcpServers", "editorMode", "verbose"}

// preferenceKeys are the options that live only in .claude.json (the "Global
// config" keys of Claude Code's settings reference) and shape how it behaves at
// the keyboard and in the terminal, such as leftArrowOpensAgents. They follow the
// user into every profile: at creation, and later into a profile that lacks
// them. Account and project data are never among them, and neither is
// claudeInChromeDefaultEnabled: it would switch on a browser link in the profile.
var preferenceKeys = []string{
	"autoConnectIde", "autoInstallIdeExtension", "copyFullResponse", "copyOnSelect", "defaultToAgentsView",
	"diffTool", "externalEditorContext", "leftArrowOpensAgents", "prStatusFooterEnabled",
}

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
			keys := append(append([]string(nil), stateKeys...), preferenceKeys...)
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

// addPreferences gives an existing dst the preferenceKeys of src that it lacks: a
// profile made before a preference was carried over gets it on its next launch.
// A key dst has is the profile's own (set in a session, it stays). It is
// best-effort: a file that cannot be read, parsed or replaced is left as it is, a
// launch never fails for it. Claude Code rewrites this file too, so it is written
// only when something is added.
func addPreferences(src, dst string) {
	srcData, err := os.ReadFile(src)
	if err != nil {
		return
	}
	dstData, err := os.ReadFile(dst)
	if err != nil {
		return
	}
	var from, to map[string]json.RawMessage
	if json.Unmarshal(srcData, &from) != nil || json.Unmarshal(dstData, &to) != nil || to == nil {
		return
	}
	changed := false
	for _, k := range preferenceKeys {
		if v, ok := from[k]; ok {
			if _, has := to[k]; !has {
				to[k], changed = v, true
			}
		}
	}
	if !changed {
		return
	}
	if out, err := json.MarshalIndent(to, "", "  "); err == nil {
		_ = writeIfChanged(dst, out)
	}
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
