package config

import (
	_ "embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
