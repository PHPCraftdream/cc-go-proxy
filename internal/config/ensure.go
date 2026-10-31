package config

import (
	"bytes"
	_ "embed"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
	"github.com/PHPCraftdream/cc-go-proxy/internal/securefile"
)

//go:embed config.example.ktav
var defaultTemplate []byte

// DefaultTemplate returns the config written on first run.
func DefaultTemplate() []byte { return append([]byte(nil), defaultTemplate...) }

// newConfig is the config written for a new setup. A setup in its own home
// directory gets a free port: a server is found by its port, so two homes on
// the default one would share a server (and its tokens).
func newConfig() []byte {
	t := DefaultTemplate()
	if home.Root() == "" {
		return t
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return t
	}
	defer func() { _ = l.Close() }()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		return t
	}
	return bytes.Replace(t, []byte("listen.port: "+DefaultPort), []byte("listen.port: "+port), 1)
}

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
	if _, err := f.Write(newConfig()); err != nil {
		_ = f.Close()
		return path, false, err
	}
	return path, true, f.Close()
}

// backupFile copies path to <path>.bak-<timestamp> (0600) and returns the
// backup path. A missing original is not an error: backup stays "". The
// timestamp has one-second resolution, so a taken name gets a -N suffix:
// two edits in the same second must not overwrite the first backup.
func backupFile(path string) (string, error) {
	old, rerr := os.ReadFile(path)
	if errors.Is(rerr, fs.ErrNotExist) {
		return "", nil
	}
	if rerr != nil {
		return "", rerr
	}
	stem := path + ".bak-" + time.Now().Format("20060102-150405")
	backup := stem
	for i := 1; ; i++ {
		f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			backup = stem + "-" + strconv.Itoa(i)
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(old); err != nil {
			_ = f.Close()
			return "", err
		}
		return backup, f.Close()
	}
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
	if backup, err = backupFile(path); err != nil {
		return path, "", err
	}
	return path, backup, securefile.WriteFile(path, newConfig())
}
