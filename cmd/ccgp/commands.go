package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/daemon"
)

// command is one CLI command, named with a leading "--". The table below is
// the single source for dispatch and the help text.
type command struct {
	name         string // "--status"
	args         string // synopsis after the name
	desc         string // one-line description
	ensureConfig bool   // create the default config first
	run          func(args []string) error
}

// commands is ordered; later tasks append entries.
func commands() []command {
	return []command{
		{name: "--serve", args: "[-d] [-s]", desc: "Run server in foreground (-d debug, -s request log)", ensureConfig: true, run: serveCmd},
		{name: "--start", args: "", desc: "Start background server", ensureConfig: true, run: startCmd},
		{name: "--stop", args: "", desc: "Stop background server", run: stopCmd},
		{name: "--status", args: "[--json]", desc: "Show background server status", run: statusCmd},
		{name: "--login", args: "<provider> [--device]", desc: "Authenticate (codex: browser OAuth, --device: device code; codex_api: API key)", ensureConfig: true, run: loginCmd},
		{name: "--models", args: "[apply] [--json] [-v] [profile]", desc: "Show (or write into config) the newest Codex models per Claude tier", ensureConfig: true, run: modelsCmd},
		{name: "--send", args: "<from> <to> [count]", desc: "Copy the latest sessions (default 10) of the current directory between \"claude\" and a profile", ensureConfig: true, run: sessionsSendCmd},
		{name: "--import", args: "<profile> [count]", desc: "Same as: ccgp --send claude <profile>", ensureConfig: true, run: sessionsImportCmd},
		{name: "--export", args: "<profile> [count]", desc: "Same as: ccgp --send <profile> claude", ensureConfig: true, run: sessionsExportCmd},
		{name: "--profile", args: "add|list [--json]|rm", desc: "Manage profiles (and their providers) in config.ktav", ensureConfig: true, run: profileDispatchCmd},
		{name: "--config", args: "reset", desc: "Back up the config and regenerate the default one", run: configCmd},
		{name: "--version", args: "", desc: "Show version", run: versionCmd},
		{name: "--logs", args: "[-f] [-n N]", desc: "Show the background server log (-f follow, -n N lines, default 50)", run: logsCmd},
		{name: "--version", args: "", desc: "Show version", run: versionCmd},
		{name: "--help", args: "", desc: "Show this help", run: helpCmd},
	}
}

func findCommand(name string) (command, bool) {
	for _, c := range commands() {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// invocation is a parsed command line: either a command (cmd != nil) or a
// profile run (profile != ""). cmd == nil with an empty profile means help
// (no args, -h or --help); the caller prints it.
type invocation struct {
	cmd     *command
	profile string
	args    []string
}

// parseInvocation classifies args without side effects. A first argument
// starting with "-" must name a command; a bare word is always a profile and
// the rest of the arguments go to claude verbatim.
func parseInvocation(args []string) (invocation, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return invocation{}, nil
	}
	if args[0] == "" {
		return invocation{}, errors.New("empty first argument: give a profile or a --command, see: ccgp --help")
	}
	if strings.HasPrefix(args[0], "-") {
		c, ok := findCommand(args[0])
		if !ok {
			return invocation{}, fmt.Errorf("unknown command %q, see: ccgp --help", args[0])
		}
		return invocation{cmd: &c, args: args[1:]}, nil
	}
	return invocation{profile: args[0], args: args[1:]}, nil
}

func serveCmd(args []string) error {
	return serve(args)
}

func startCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h, err := ensureServer(cfg, args)
	if err != nil {
		return err
	}
	fmt.Printf("ccgp running (pid %d) at %s\n", h.PID, daemon.BaseURL(cfg.Host, cfg.Port))
	return nil
}

func stopCmd(args []string) error {
	host, port := listenAddr()
	if err := daemon.Stop(daemon.BaseURL(host, port), 5*time.Second); err != nil {
		return err
	}
	fmt.Println("ccgp stopped")
	return nil
}

func statusCmd(args []string) error {
	return status(args)
}

func loginCmd(args []string) error {
	device, rest := false, []string{}
	for _, a := range args {
		if a == "--device" {
			device = true
		} else {
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		return errors.New("usage: ccgp --login <provider> [--device]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return login(cfg, rest[0], true, device)
}

func modelsCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return models(cfg, args)
}

func sessionsSendCmd(args []string) error   { return sessionsCmd("send", args) }
func sessionsImportCmd(args []string) error { return sessionsCmd("import", args) }
func sessionsExportCmd(args []string) error { return sessionsCmd("export", args) }

func profileDispatchCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return profileCmd(cfg, args)
}

func versionCmd(args []string) error {
	fmt.Println(version)
	return nil
}

func helpCmd(args []string) error {
	printHelp(os.Stdout)
	return nil
}

// daemonServeArgs builds the argv of the detached background server.
func daemonServeArgs(serveArgs []string) []string {
	return append([]string{"--serve", "-s"}, serveArgs...)
}

// printHelp writes the help text, with the command lines from the table.
func printHelp(w io.Writer) {
	fmt.Fprint(w, `ccgp - Claude Code bridge to OpenAI-compatible APIs and ChatGPT Codex

Usage:
  ccgp <profile> [claude args...]  Ensure server + credentials, then run claude
                                   (a word without dashes is always a profile; commands start with --)
`)
	for _, c := range commands() {
		head := strings.TrimSpace(c.name + " " + c.args)
		fmt.Fprintf(w, "  ccgp %-34s %s\n", head, c.desc)
	}
	fmt.Fprint(w, `
Profiles:
  ccgp --profile add [name] --like <profile>      New profile with its OWN login (e.g. a second Codex account: ccgp codex2)
  ccgp --profile add <name> --provider <p>        New profile on an existing login [--model M [--window N]]
  ccgp --profile add <name> --type <t> ...        New backend: codex | codex_api | anthropic_compat [--base-url URL] [--model M]
  ccgp --profile list                             Show profiles, providers, login state and Claude dirs
  ccgp --profile rm <name> [--yes] [--purge]      Remove a profile (its provider too when unused; --purge deletes data)

Config (Ktav):
  Windows: %APPDATA%\cc-go-proxy\config.ktav   Linux: ~/.config/cc-go-proxy/config.ktav
  Override: CCGP_CONFIG. Created automatically on first run.
  Credentials are stored separately in <config dir>/cc-go-proxy/creds (owner-only).

Env-only mode (no config.ktav):
  OPENAI_API_KEY, PROXY_TOKEN (min 32 chars), OPENAI_BASE_URL, HOST, PORT
  Profile name: default

Example:
  ccgp codex -p "hello"
`)
}
