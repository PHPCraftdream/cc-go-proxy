package config

import (
	_ "embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/claude-code-proxy/proxy/internal/securefile"
)

//go:embed config.example.ktav
var defaultTemplate []byte

// DefaultTemplate returns the config written on first run.
func DefaultTemplate() []byte { return append([]byte(nil), defaultTemplate...) }

// EnsureFile creates the default config when none exists. It does nothing when
// CCGP_CONFIG or PROXY_TOKEN selects another mode. Existing files are never touched.
func EnsureFile() (path string, created bool, err error) {
	if os.Getenv(ConfigFileEnv) != "" || os.Getenv("PROXY_TOKEN") != "" {
		return "", false, nil
	}
	path, err = DefaultConfigPath()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return path, false, nil
	}
	if err != nil {
		return path, false, err
	}
	if _, err := f.Write(defaultTemplate); err != nil {
		_ = f.Close()
		return path, false, err
	}
	return path, true, f.Close()
}

// ResetFile replaces the config with the current default template, keeping the
// old file as <name>.bak-<timestamp>. It returns the config path and the backup
// path ("" when there was no config).
func ResetFile() (path, backup string, err error) {
	path, _, err = configFilePath()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, "", err
	}
	if old, rerr := os.ReadFile(path); rerr == nil {
		backup = path + ".bak-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, old, 0o600); err != nil {
			return path, "", err
		}
	} else if !errors.Is(rerr, fs.ErrNotExist) {
		return path, "", rerr
	}
	return path, backup, securefile.WriteFile(path, defaultTemplate)
}
