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
	"io/fs"
	"os"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/daemon"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server"
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

// status prints server state, file locations and per-provider login state.
func status() error {
	host, port := listenAddr()
	base := daemon.BaseURL(host, port)
	logPath, _ := daemon.LogPath()

	h, err := daemon.Check(base)
	if err != nil {
		fmt.Printf("server:  not running at %s\n", base)
	} else {
		fmt.Printf("server:  %s running (pid %d) at %s\n", h.Version, h.PID, base)
	}
	fmt.Printf("log:     %s\n", logPath)

	cfg, cfgErr := config.Load()
	switch {
	case cfgErr != nil && missingDefaultConfig():
		p, _ := config.DefaultConfigPath()
		fmt.Printf("config:  %s (not created yet; created on first run)\n", p)
	case cfgErr != nil:
		fmt.Printf("config:  error: %v\n", cfgErr)
	case cfg.IsEnvMode():
		fmt.Println("config:  env-only mode (no config.ktav)")
	default:
		fmt.Printf("config:  %s\n", cfg.ConfigFile)
		store, err := credsStore()
		if err != nil {
			return err
		}
		fmt.Printf("creds:   %s\n", store.Dir)
		for _, name := range sortedKeys(cfg.Providers) {
			pr := cfg.Providers[name]
			fmt.Printf("  provider %-12s %-7s %s\n", name, pr.Type, loginState(store, pr))
		}
		for _, name := range sortedKeys(cfg.Profiles) {
			fmt.Printf("  profile  %-12s -> %s\n", name, cfg.Profiles[name].Provider)
		}
	}
	if err != nil {
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
	if h, err := daemon.Check(daemon.BaseURL(cfg.Host, cfg.Port)); err == nil {
		return fmt.Errorf("already running (pid %d)", h.PID)
	}
	return server.Start(cfg)
}

// ensureServer returns the running server's health, starting it detached if needed.
func ensureServer(cfg *config.Config, serveArgs []string) (*daemon.Health, error) {
	base := daemon.BaseURL(cfg.Host, cfg.Port)
	if h, err := daemon.Check(base); err == nil {
		return h, nil
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
	return daemon.Start(exe, args, logPath, base, 10*time.Second)
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
