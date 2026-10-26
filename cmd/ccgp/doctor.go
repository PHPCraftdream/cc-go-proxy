package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/codex"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/creds"
	"github.com/PHPCraftdream/cc-go-proxy/internal/oauth"
	"github.com/PHPCraftdream/cc-go-proxy/internal/proxydial"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server"
)

// doctorRefreshMaxAge mirrors oauth refreshMaxAge; do not import the
// unexported one.
const doctorRefreshMaxAge = 8 * 24 * time.Hour

// doctorCmd implements `ccgp --doctor`.
func doctorCmd(args []string) error {
	return runDoctor(args, os.Stdout, defaultDoctorEnv())
}

// check is one doctor diagnostic result.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	OK     bool    `json:"ok"`
	Checks []check `json:"checks"`
}

// doctorEnv bundles the machine state of the doctor checks so tests can fake
// it without touching the real system.
type doctorEnv struct {
	lookPath     func(string) (string, error)
	loadConfig   func() (*config.Config, error)
	defaultMiss  func() bool
	angelCheck   func(string) (*angel.Health, error)
	base         string
	ownVersion   string
	ownExe       string
	ownExeMod    int64
	store        creds.Store
	loginStateFn func(creds.Store, *config.Provider) string
	refreshAge   func(creds.Store, *config.Provider) (time.Time, bool)
	probe        func(rawURL string, via proxydial.Choice) (int, error)
	stat         func(string) (fs.FileInfo, error)
	windows      bool
	logPath      func() (string, error)
	logWritable  func(dir string) error
	now          func() time.Time
}

func defaultDoctorEnv() *doctorEnv {
	store, _ := credsStore()
	exe, mod := ownExeInfo()
	host, port := listenAddr()
	return &doctorEnv{
		lookPath:     exec.LookPath,
		loadConfig:   config.Load,
		defaultMiss:  missingDefaultConfig,
		angelCheck:   angel.Check,
		base:         angel.BaseURL(host, port),
		ownVersion:   server.ProxyVersion,
		ownExe:       exe,
		ownExeMod:    mod,
		store:        store,
		loginStateFn: loginState,
		refreshAge:   oauthRefreshAge,
		probe:        probeURL,
		stat:         os.Stat,
		windows:      runtime.GOOS == "windows",
		logPath:      angel.LogPath,
		logWritable:  defaultLogWritable,
		now:          time.Now,
	}
}

// oauthRefreshAge reports a Codex provider's last token refresh, for the
// staleness warning. ok=false when unknown.
func oauthRefreshAge(store creds.Store, pr *config.Provider) (time.Time, bool) {
	t, err := oauth.Store{Path: store.OAuthPath(pr.Name)}.Load()
	if err != nil || pr.Type != config.ProviderTypeCodex {
		return time.Time{}, false
	}
	return t.LastRefresh, !t.LastRefresh.IsZero()
}

// runDoctor runs the diagnostics and prints the report as text or JSON.
func runDoctor(args []string, w io.Writer, env *doctorEnv) error {
	offline, jsonOut := false, false
	for _, a := range args {
		switch a {
		case "--offline":
			offline = true
		case "--json":
			jsonOut = true
		default:
			return errors.New("usage: ccgp --doctor [--offline] [--json]")
		}
	}
	cfg, _ := env.loadConfig()
	checks := doctorRun(env, cfg, offline)
	anyFail := false
	for _, c := range checks {
		if c.Status == "fail" {
			anyFail = true
		}
	}
	if jsonOut {
		if err := writeJSON(w, doctorReport{OK: !anyFail, Checks: checks}); err != nil {
			return err
		}
	} else {
		for _, c := range checks {
			body := c.Name
			if d := strings.TrimSpace(c.Detail); d != "" {
				body += ": " + d
			}
			tag := map[string]string{"ok": "[ok]", "warn": "[warn]", "fail": "[FAIL]"}[c.Status]
			if tag == "" {
				tag = "[" + c.Status + "]"
			}
			_, _ = fmt.Fprintf(w, "%-7s%s\n", tag, body)
		}
	}
	if anyFail {
		return exitError(1)
	}
	return nil
}

// doctorRun assembles the ordered checks. cfg may be nil when loading failed.
func doctorRun(env *doctorEnv, cfg *config.Config, offline bool) []check {
	var checks []check
	checks = append(checks, checkClaude(env))
	checks = append(checks, checkConfig(env))
	checks = append(checks, checkServer(env))
	if cfg != nil && !cfg.IsEnvMode() {
		checks = append(checks, checkLogins(env, cfg)...)
		if !offline {
			checks = append(checks, checkBackends(env, cfg)...)
		}
	}
	checks = append(checks, checkCredsDir(env))
	checks = append(checks, checkLogFile(env))
	return checks
}

func checkClaude(env *doctorEnv) check {
	if _, err := env.lookPath("claude"); err != nil {
		return check{Name: "claude", Status: "fail", Detail: "not found in PATH; install Claude Code (https://claude.com/claude-code) and ensure it is on PATH"}
	}
	return check{Name: "claude", Status: "ok", Detail: "found in PATH"}
}

func checkConfig(env *doctorEnv) check {
	// When the default config file is missing, Load returns an env-only config
	// or an env validation error, so the missing check must run before Load's
	// outcome is judged. missingDefaultConfig is false when CCGP_CONFIG is set,
	// so an explicit env override is respected.
	if env.defaultMiss() {
		return check{Name: "config", Status: "warn", Detail: "not created yet, created on first run"}
	}
	cfg, err := env.loadConfig()
	if err != nil {
		return check{Name: "config", Status: "fail", Detail: err.Error()}
	}
	if cfg.IsEnvMode() {
		return check{Name: "config", Status: "ok", Detail: "env-only mode (no config.ktav)"}
	}
	return check{Name: "config", Status: "ok", Detail: cfg.ConfigFile}
}

func checkServer(env *doctorEnv) check {
	h, err := env.angelCheck(env.base)
	if err != nil {
		return check{Name: "server", Status: "ok", Detail: "not running (starts on demand)"}
	}
	if stale, why := serverStale(h, env.ownVersion, env.ownExe, env.ownExeMod); stale {
		return check{Name: "server", Status: "warn", Detail: fmt.Sprintf("older than this binary (%s); run: ccgp --restart", why)}
	}
	return check{Name: "server", Status: "ok", Detail: fmt.Sprintf("%s pid %d", h.Version, h.PID)}
}

func checkLogins(env *doctorEnv, cfg *config.Config) []check {
	var checks []check
	for _, name := range sortedKeys(cfg.Providers) {
		pr := cfg.Providers[name]
		s := env.loginStateFn(env.store, pr)
		c := check{Name: "login " + name, Status: "ok", Detail: s}
		switch {
		case loginIsWarn(s):
			c.Status = "warn"
		case pr.Type == config.ProviderTypeCodex:
			if env.refreshAge == nil {
				break
			}
			if last, ok := env.refreshAge(env.store, pr); ok && env.now().Sub(last) > doctorRefreshMaxAge {
				c.Status = "warn"
				c.Detail = fmt.Sprintf("%s; tokens %d days old, refresh with: ccgp --login %s", s, int(env.now().Sub(last).Hours()/24), pr.Name)
			}
		}
		checks = append(checks, c)
	}
	return checks
}

// loginIsWarn reports whether a loginState string describes missing or broken
// credentials.
func loginIsWarn(s string) bool {
	for _, p := range []string{"not logged in", "no API key", "error:"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return strings.HasSuffix(s, " is empty")
}

func checkBackends(env *doctorEnv, cfg *config.Config) []check {
	var checks []check
	for _, name := range sortedKeys(cfg.Providers) {
		pr := cfg.Providers[name]
		u := pr.BaseURL
		if u == "" && pr.Type == config.ProviderTypeCodex {
			u = codex.DefaultBaseURL
		}
		if u == "" {
			continue
		}
		c := check{Name: "backend " + name, Status: "ok"}
		if code, err := env.probe(u, cfg.ProviderProxyChoice(name)); err != nil {
			// warn, not fail: the user may simply be offline.
			c.Status = "warn"
			c.Detail = fmt.Sprintf("unreachable: %v", err)
		} else {
			c.Detail = fmt.Sprintf("reachable (HTTP %d)", code)
		}
		checks = append(checks, c)
	}
	return checks
}

func checkCredsDir(env *doctorEnv) check {
	dir := env.store.Dir
	if dir == "" {
		return check{Name: "creds dir", Status: "warn", Detail: "cannot determine the creds directory"}
	}
	fi, err := env.stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return check{Name: "creds dir", Status: "ok", Detail: "not created yet"}
	case err != nil:
		return check{Name: "creds dir", Status: "warn", Detail: err.Error()}
	case env.windows:
		return check{Name: "creds dir", Status: "ok", Detail: "owner-only ACLs are set by ccgp (not checked)"}
	case fi.Mode().Perm()&0o077 != 0:
		return check{Name: "creds dir", Status: "warn", Detail: fmt.Sprintf("permissions %04o, expected 0700; fix: chmod 700 %s", fi.Mode().Perm(), dir)}
	}
	return check{Name: "creds dir", Status: "ok", Detail: dir}
}

func checkLogFile(env *doctorEnv) check {
	p, err := env.logPath()
	if err != nil {
		return check{Name: "log file", Status: "warn", Detail: err.Error()}
	}
	dir := filepath.Dir(p)
	if err := env.logWritable(dir); err != nil {
		return check{Name: "log file", Status: "warn", Detail: fmt.Sprintf("%s is not writable: %v", dir, err)}
	}
	return check{Name: "log file", Status: "ok", Detail: p}
}

// defaultLogWritable checks that dir can hold new files.
func defaultLogWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(name)
}

// probeURL sends a credential-free GET and returns the status code.
func probeURL(url string, via proxydial.Choice) (int, error) {
	req, err := http.NewRequestWithContext(proxydial.WithChoice(context.Background(), via), http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := proxydial.HTTPClient(5 * time.Second).Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}
