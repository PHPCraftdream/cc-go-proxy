// Command ccgp is a headless Claude Code -> OpenAI/Codex bridge.
//
//	ccgp <profile> [claude args...]   start server if needed, ensure creds, run claude
//	ccgp serve [-d] [-s]              run server in foreground
//	ccgp start|stop|status            manage background server
//	ccgp login <provider>             (re)authenticate a provider
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/claude-code-proxy/proxy/internal/config"
	"github.com/claude-code-proxy/proxy/internal/daemon"
	"github.com/claude-code-proxy/proxy/internal/server"
)

const version = "ccgp v" + server.ProxyVersion

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
	if len(args) == 0 {
		printHelp()
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		printHelp()
		return nil
	case "version", "--version":
		fmt.Println(version)
		return nil
	case "serve":
		return serve(args[1:])
	case "start":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		h, err := ensureServer(cfg, args[1:])
		if err != nil {
			return err
		}
		fmt.Printf("ccgp running (pid %d) at %s\n", h.PID, daemon.BaseURL(cfg.Host, cfg.Port))
		return nil
	case "stop":
		host, port := listenAddr()
		if err := daemon.Stop(daemon.BaseURL(host, port), 5*time.Second); err != nil {
			return err
		}
		fmt.Println("ccgp stopped")
		return nil
	case "status":
		host, port := listenAddr()
		base := daemon.BaseURL(host, port)
		h, err := daemon.Check(base)
		if err != nil {
			fmt.Printf("ccgp not running at %s\n", base)
			return exitError(3)
		}
		logPath, _ := daemon.LogPath()
		fmt.Printf("ccgp %s running (pid %d) at %s\nlog: %s\n", h.Version, h.PID, base, logPath)
		return nil
	case "login":
		if len(args) != 2 {
			return errors.New("usage: ccgp login <provider>")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return login(cfg, args[1], true)
	default:
		return launch(args[0], args[1:])
	}
}

// serve runs the server in the foreground.
func serve(args []string) error {
	debug, simple := false, false
	for _, a := range args {
		switch a {
		case "-d", "--debug":
			debug = true
		case "-s", "--simple":
			simple = true
		default:
			return fmt.Errorf("serve: unknown flag %q", a)
		}
	}
	cfg, err := config.LoadWithDebug(debug)
	if err != nil {
		return err
	}
	cfg.SimpleLog = simple
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
	// Simple request log by default in background mode.
	args := append([]string{"serve", "-s"}, serveArgs...)
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

func printHelp() {
	fmt.Println(`ccgp - Claude Code bridge to OpenAI-compatible APIs and ChatGPT Codex

Usage:
  ccgp <profile> [claude args...]  Ensure server + credentials, then run claude
  ccgp serve [-d] [-s]             Run server in foreground (-d debug, -s request log)
  ccgp start                       Start background server
  ccgp stop                        Stop background server
  ccgp status                      Show background server status
  ccgp login <provider>            Authenticate provider (codex: device code, openai: API key)
  ccgp version                     Show version

Config (Ktav):
  Windows: %APPDATA%\cc-go-proxy\config.ktav   Linux: ~/.config/cc-go-proxy/config.ktav
  Override: CCGP_CONFIG. See config.example.ktav.
  Credentials are stored separately in <config dir>/cc-go-proxy/creds (owner-only).

Env-only mode (no config.ktav):
  OPENAI_API_KEY, PROXY_TOKEN (min 32 chars), OPENAI_BASE_URL, HOST, PORT
  Profile name: default

Example:
  ccgp work -p "hello"`)
}
