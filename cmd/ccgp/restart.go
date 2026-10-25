package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/daemon"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server"
)

// serverStale reports whether a running server was started from other code
// than this binary: a different version, or the executable file on disk has
// been replaced since the server started (same path, other mod time).
// Missing/unknown fields mean an old server: not stale.
func serverStale(h *daemon.Health, ownVersion, ownExe string, ownExeMod int64) (stale bool, why string) {
	if h.Version != ownVersion {
		return true, fmt.Sprintf("server version %s, this binary is %s", h.Version, ownVersion)
	}
	if h.Exe != "" && h.Exe == ownExe && h.ExeMod != 0 && ownExeMod != 0 && h.ExeMod != ownExeMod {
		return true, "the executable file changed since the server started"
	}
	return false, ""
}

// ownExeInfo returns this binary's path and mod time (0 when unknown).
func ownExeInfo() (string, int64) {
	exe, err := os.Executable()
	if err != nil {
		return "", 0
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return exe, 0
	}
	return exe, fi.ModTime().UnixNano()
}

// serverChecks bundles the process operations of ensureServer so the stale
// detection can be tested without real servers.
type serverChecks struct {
	check      func(base string) (*daemon.Health, error)
	stop       func(base string, timeout time.Duration) error
	start      func(exe string, args []string, logPath, base string, timeout time.Duration) (*daemon.Health, error)
	stderr     io.Writer
	ownVersion string
	ownExe     string
	ownExeMod  int64
}

func defaultServerChecks() *serverChecks {
	exe, mod := ownExeInfo()
	return &serverChecks{
		check:      daemon.Check,
		stop:       daemon.Stop,
		start:      daemon.Start,
		stderr:     os.Stderr,
		ownVersion: server.ProxyVersion,
		ownExe:     exe,
		ownExeMod:  mod,
	}
}

// ensureServer returns the running server's health, starting it detached if
// needed. A stale server (older binary) is restarted when idle; when requests
// are in flight it is kept and reported, because stopping it would cut them.
func ensureServer(cfg *config.Config, serveArgs []string) (*daemon.Health, error) {
	return ensureServerWith(cfg, serveArgs, defaultServerChecks())
}

func ensureServerWith(cfg *config.Config, serveArgs []string, sc *serverChecks) (*daemon.Health, error) {
	base := daemon.BaseURL(cfg.Host, cfg.Port)
	var why string
	if h, err := sc.check(base); err == nil {
		var stale bool
		stale, why = serverStale(h, sc.ownVersion, sc.ownExe, sc.ownExeMod)
		if !stale {
			return h, nil
		}
		if h.Active == 0 {
			if err := sc.stop(base, 5*time.Second); err != nil {
				return nil, err
			}
		} else {
			_, _ = fmt.Fprintf(sc.stderr,
				"ccgp: the running server is older than this binary (%s), %d request(s) in flight; restart it with: ccgp --restart\n",
				why, h.Active)
			return h, nil
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logPath, err := daemon.LogPath()
	if err != nil {
		return nil, err
	}
	args := daemonServeArgs(serveArgs)
	h, err := sc.start(exe, args, logPath, base, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if why != "" {
		_, _ = fmt.Fprintf(sc.stderr, "ccgp: restarted the server (%s)\n", why)
	}
	return h, nil
}

// restartOps bundles the process operations of restartCmd so it can be
// tested without real servers.
type restartOps struct {
	stop    func(base string, timeout time.Duration) error
	start   func(exe string, args []string, logPath, base string, timeout time.Duration) (*daemon.Health, error)
	stdout  io.Writer
	exe     func() (string, error)
	logPath func() (string, error)
}

func defaultRestartOps() *restartOps {
	return &restartOps{
		stop:    daemon.Stop,
		start:   daemon.Start,
		stdout:  os.Stdout,
		exe:     os.Executable,
		logPath: daemon.LogPath,
	}
}

// restartServer stops the server at base when it runs and starts a new one,
// regardless of staleness or in-flight requests.
func restartServer(cfg *config.Config, ops *restartOps) (*daemon.Health, error) {
	base := daemon.BaseURL(cfg.Host, cfg.Port)
	if err := ops.stop(base, 5*time.Second); err != nil && !errors.Is(err, daemon.ErrNotRunning) {
		return nil, err
	}
	exe, err := ops.exe()
	if err != nil {
		return nil, err
	}
	logPath, err := ops.logPath()
	if err != nil {
		return nil, err
	}
	h, err := ops.start(exe, daemonServeArgs(nil), logPath, base, 10*time.Second)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(ops.stdout, "ccgp restarted (pid %d) at %s\n", h.PID, base)
	return h, nil
}

// --restart stops the running server (if any) and starts a new one.
func restartCmd(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: ccgp --restart")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	_, err = restartServer(cfg, defaultRestartOps())
	return err
}
