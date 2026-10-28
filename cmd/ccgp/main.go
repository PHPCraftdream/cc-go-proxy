// Command ccgp is a headless Claude Code -> OpenAI/Codex bridge.
//
//	ccgp <profile> [claude args...]   start server if needed, ensure creds, run claude
//	ccgp --serve [-d] [-s]            run server in foreground
//	ccgp --start|--stop|--status      manage background server
//	ccgp --login <provider> [--device] (re)authenticate a provider
//	ccgp --send <from> <to> [n]       copy the n latest sessions of the current directory between "claude" and profiles
//	ccgp --import|--export <profile> [n]  aliases of send: claude -> profile / profile -> claude
//	ccgp --profile add|list|rm        manage profiles (and their providers) in config.ktav
//
// Commands are written with a leading "--"; a bare word is always a profile.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server"

	// Root certificates for HTTPS when the system has none (busybox, scratch and
	// other minimal containers): used only as a fallback, never over the system store.
	_ "golang.org/x/crypto/x509roots/fallback"
)

var version = "ccgp v" + server.ProxyVersion

func main() {
	if err := run(os.Args[1:]); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(int(ee))
		}
		fmt.Fprintf(os.Stderr, "ccgp: %v\n", err)
		os.Exit(1)
	}
}

// exitError propagates a child exit code without printing.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func run(args []string) error {
	inv, err := parseInvocation(args)
	if err != nil {
		return err
	}
	if inv.cmd == nil {
		if inv.profile == "" {
			printHelp(os.Stdout)
			return nil
		}
		if err := ensureConfig(); err != nil {
			return err
		}
		return launch(inv.profile, inv.args)
	}
	// Per-command help runs before ensureConfig, so --help always works.
	if handleHelp(inv, os.Stdout) {
		return nil
	}
	if inv.cmd.ensureConfig {
		if err := ensureConfig(); err != nil {
			return err
		}
	}
	return inv.cmd.run(inv.args)
}

// ensureConfig creates the default config on first run.
func ensureConfig() error {
	p, created, err := config.EnsureFile()
	if err != nil {
		return fmt.Errorf("create default config: %w", err)
	}
	if created {
		fmt.Fprintf(os.Stderr, "ccgp: created default config %s (edit providers, profiles and model names)\n", p)
	}
	return nil
}

func missingDefaultConfig() bool {
	if os.Getenv(config.ConfigFileEnv) != "" {
		return false
	}
	p, err := config.DefaultConfigPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return errors.Is(err, fs.ErrNotExist)
}

// statusData is the machine- and human-rendered state of `ccgp --status`.
type statusServer struct {
	Running bool   `json:"running"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
	URL     string `json:"url"`
}

type statusConfig struct {
	Mode  string `json:"mode"` // "file", "env", "missing" or "error"
	Path  string `json:"path"`
	Error string `json:"error,omitempty"`
}

type statusProvider struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Login string `json:"login"`
}

type statusProfile struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

type statusData struct {
	Server    statusServer     `json:"server"`
	LogFile   string           `json:"logFile"`
	Config    statusConfig     `json:"config"`
	CredsDir  string           `json:"credsDir"`
	Providers []statusProvider `json:"providers"`
	Profiles  []statusProfile  `json:"profiles"`
}

// buildStatus assembles the status payload; it performs no I/O.
func buildStatus(base, logPath string, h *angel.Health, healthErr error, cfg *config.Config, cfgErr error, credsDir string, credsErr error, providers []statusProvider, profiles []statusProfile) statusData {
	s := statusData{
		LogFile:   logPath,
		CredsDir:  credsDir,
		Providers: providers,
		Profiles:  profiles,
	}
	if healthErr != nil {
		s.Server = statusServer{URL: base}
	} else {
		s.Server = statusServer{Running: true, Version: h.Version, PID: h.PID, URL: base}
	}
	switch {
	case cfgErr != nil && missingDefaultConfig():
		p, _ := config.DefaultConfigPath()
		s.Config = statusConfig{Mode: "missing", Path: p}
	case cfgErr != nil:
		s.Config = statusConfig{Mode: "error", Error: cfgErr.Error()}
	case cfg.IsEnvMode():
		s.Config = statusConfig{Mode: "env"}
	default:
		s.Config = statusConfig{Mode: "file", Path: cfg.ConfigFile}
	}
	return s
}

// renderStatusText prints the human-readable form of a status payload.
func renderStatusText(w io.Writer, s statusData) error {
	if s.Server.Running {
		_, _ = fmt.Fprintf(w, "server:  %s running (pid %d) at %s\n", s.Server.Version, s.Server.PID, s.Server.URL)
	} else {
		_, _ = fmt.Fprintf(w, "server:  not running at %s\n", s.Server.URL)
	}
	_, _ = fmt.Fprintf(w, "log:     %s\n", s.LogFile)
	switch s.Config.Mode {
	case "missing":
		_, _ = fmt.Fprintf(w, "config:  %s (not created yet; created on first run)\n", s.Config.Path)
	case "error":
		_, _ = fmt.Fprintf(w, "config:  error: %s\n", s.Config.Error)
	case "env":
		_, _ = fmt.Fprintln(w, "config:  env-only mode (no config.ktav)")
	default:
		_, _ = fmt.Fprintf(w, "config:  %s\n", s.Config.Path)
		if s.CredsDir != "" {
			_, _ = fmt.Fprintf(w, "creds:   %s\n", s.CredsDir)
			for _, p := range s.Providers {
				_, _ = fmt.Fprintf(w, "  provider %-12s %-7s %s\n", p.Name, p.Type, p.Login)
			}
			for _, p := range s.Profiles {
				_, _ = fmt.Fprintf(w, "  profile  %-12s -> %s\n", p.Name, p.Provider)
			}
		}
	}
	return nil
}

// status prints server state, file locations and per-provider login state.
func status(args []string) error {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			return fmt.Errorf("--status: unknown flag %q", a)
		}
	}
	host, port := listenAddr()
	base := angel.BaseURL(host, port)
	logPath, _ := angel.LogPath()
	h, healthErr := angel.Check(base)
	cfg, cfgErr := config.Load()
	credsDir := ""
	var credsErr error
	var providers []statusProvider
	var profiles []statusProfile
	if cfgErr == nil && !cfg.IsEnvMode() {
		store, err := credsStore()
		if err != nil {
			credsErr = err
		} else {
			credsDir = store.Dir
			for _, name := range sortedKeys(cfg.Providers) {
				pr := cfg.Providers[name]
				providers = append(providers, statusProvider{Name: name, Type: pr.Type, Login: loginState(store, pr)})
			}
			for _, name := range sortedKeys(cfg.Profiles) {
				profiles = append(profiles, statusProfile{Name: name, Provider: cfg.Profiles[name].Provider})
			}
		}
	}
	st := buildStatus(base, logPath, h, healthErr, cfg, cfgErr, credsDir, credsErr, providers, profiles)
	out := io.Writer(os.Stdout)
	var err error
	if jsonOut {
		err = writeJSON(out, st)
	} else {
		err = renderStatusText(out, st)
	}
	if err != nil {
		return err
	}
	if credsErr != nil {
		return credsErr
	}
	if healthErr != nil {
		return exitError(3)
	}
	return nil
}

// --serve runs the server in the foreground.
func serve(args []string) error {
	debug, simple := false, false
	for _, a := range args {
		switch a {
		case "-d", "--debug":
			debug = true
		case "-s", "--simple":
			simple = true
		default:
			return fmt.Errorf("--serve: unknown flag %q", a)
		}
	}
	cfg, err := config.LoadWithDebug(debug)
	if err != nil {
		return err
	}
	cfg.SimpleLog = simple
	if cfg.ConfigFile != "" {
		fmt.Printf("config: %s\n", cfg.ConfigFile)
	}
	if h, err := angel.Check(angel.BaseURL(cfg.Host, cfg.Port)); err == nil {
		return fmt.Errorf("already running (pid %d)", h.PID)
	}
	return server.Start(cfg)
}

// listenAddr resolves host/port without requiring a fully valid config.
func listenAddr() (string, string) {
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

// configCmd implements `ccgp --config reset`.
func configCmd(args []string) error {
	if len(args) != 1 || args[0] != "reset" {
		return errors.New("usage: ccgp --config reset")
	}
	path, backup, err := config.ResetFile()
	if err != nil {
		return err
	}
	if backup != "" {
		fmt.Printf("old config saved as %s\n", backup)
	}
	fmt.Printf("default config written to %s\n", path)
	fmt.Println("next: ccgp --models apply   (refresh the generated model mappings)")
	return nil
}
