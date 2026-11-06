package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
)

// command is one CLI command, named with a leading "--". The table below is
// the single source for dispatch and the help text.
type command struct {
	name         string   // "--status"
	args         string   // synopsis after the name
	desc         string   // one-line description
	ensureConfig bool     // create the default config first
	hidden       bool     // helper command, skipped by printHelp
	verbs        []string // entry points beyond the command itself ("set default_model"); the parity test and the completion read them
	run          func(args []string) error
}

// commands is ordered; later tasks append entries.
func commands() []command {
	return []command{
		{name: "--serve", args: "[-d] [-s]", desc: "Run server in foreground (-d debug, -s request log)", ensureConfig: true, run: serveCmd},
		{name: "--start", args: "", desc: "Start background server", ensureConfig: true, run: startCmd},
		{name: "--stop", args: "", desc: "Stop background server", run: stopCmd},
		{name: "--status", args: "[--json]", desc: "Show background server status", run: statusCmd},
		{name: "--login", args: "<provider> [--device]", desc: "Authenticate (codex: browser OAuth, --device: device code; codex_api: API key)", ensureConfig: true, run: loginCmd, verbs: []string{"--device"}},
		{name: "--models", args: "[apply] [--json] [-v] [profile]", desc: "Show (or write into config) the newest Codex models per Claude tier", ensureConfig: true, run: modelsCmd, verbs: []string{"apply"}},
		{name: "--send", args: "<from> <to> [count] [--fork|--force]", desc: "Copy the latest sessions (default 10) of the current directory between \"claude\" and a profile (--fork: keep a diverged one as a new session, --force: replace it)", ensureConfig: true, run: sessionsSendCmd, verbs: sessionVerbs},
		{name: "--import", args: "<profile> [count] [--fork|--force]", desc: "Same as: ccgp --send claude <profile>", ensureConfig: true, run: sessionsImportCmd, verbs: sessionVerbs},
		{name: "--export", args: "<profile> [count] [--fork|--force]", desc: "Same as: ccgp --send <profile> claude", ensureConfig: true, run: sessionsExportCmd, verbs: sessionVerbs},
		{name: "--profile", args: "add|list [--json]|rm|set|unset", desc: "Manage profiles (and their providers) in config.ktav", ensureConfig: true, run: profileDispatchCmd, verbs: profileVerbs()},
		{name: "--provider", args: "[NAME] [--json] | set|unset NAME KEY [VALUE]", desc: "Show the providers, or set base_url / api_key_env of one", ensureConfig: true, run: providerCmd, verbs: providerVerbs()},
		{name: "--interactive", args: "", desc: "Full-screen interface that does everything the commands do (also: -i)", ensureConfig: true, run: interactiveCmd},
		{name: "-i", args: "", desc: "Same as: ccgp --interactive", ensureConfig: true, run: interactiveCmd},
		{name: "--config", args: "reset", desc: "Back up the config and regenerate the default one", run: configCmd, verbs: []string{"reset"}},
		{name: "--connections", args: "[active N] [total N] [proxy URL|off [provider NAME | profile NAME]] [--json]", desc: "Show or set the upstream connections per backend (default 7 active, 10 total) and the proxy", ensureConfig: true, run: connectionsCmd, verbs: []string{"active", "total", "proxy", "proxy provider", "proxy profile"}},
		{name: "--peak", args: "[provider [set|add WINDOW...|rm N...|tz ZONE|clear|pause [DURATION]|resume]] [--json]", desc: "Show or set the peak hours of a provider, when its profiles' requests are refused", ensureConfig: true, run: peakCmd, verbs: []string{"set", "add", "rm", "tz", "clear", "pause", "resume"}},
		{name: "--which", args: "<profile> <model> [--json]", desc: "Show where a Claude model is routed in a profile (offline)", ensureConfig: true, run: whichCmd},
		{name: "--logs", args: "[-f] [-n N]", desc: "Show the background server log (-f follow, -n N lines, default 50)", run: logsCmd},
		{name: "--restart", args: "", desc: "Stop and start the background server", ensureConfig: true, run: restartCmd},
		{name: "--logout", args: "<provider> [--yes]", desc: "Remove the stored login (OAuth tokens or API key) of a provider", ensureConfig: true, run: logoutCmd},
		{name: "--completion", args: "bash|zsh|powershell", desc: "Print a shell completion script (see its usage error for the install line)", ensureConfig: false, run: completionCmd},
		{name: "--update", args: "[--check]", desc: "Self-update from the latest GitHub release (--check: only report)", run: updateCmd, verbs: []string{"--check"}},
		{name: "--doctor", args: "[--offline] [--json]", desc: "Run diagnostics (claude, config, server, logins, backends, files)", ensureConfig: false, run: doctorCmd},
		{name: "--version", args: "", desc: "Show version", run: versionCmd},
		{name: "--help", args: "", desc: "Show this help", run: helpCmd},
		{name: "--complete", args: "<prog> <word>...", desc: "", hidden: true, run: completeCmd},
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
	fmt.Printf("ccgp running (pid %d) at %s\n", h.PID, angel.BaseURL(cfg.Host, cfg.Port))
	return nil
}

func stopCmd(args []string) error {
	host, port := listenAddr()
	if err := angel.StopBy("ccgp --stop")(angel.BaseURL(host, port), 5*time.Second); err != nil {
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

// angelServeArgs builds the argv of the detached background server.
func angelServeArgs(serveArgs []string) []string {
	return append([]string{"--serve", "-s"}, serveArgs...)
}

// printHelp writes the help text, with the command lines from the table.
func printHelp(w io.Writer) {
	_, _ = fmt.Fprint(w, `ccgp - Claude Code bridge to OpenAI-compatible APIs and ChatGPT Codex

Usage:
  ccgp <profile> [claude args...]  Ensure server + credentials, then run claude
                                   (a word without dashes is always a profile: ccgp codex runs profile codex;
                                   commands start with --)
  ccgpy <profile> [claude args...] Same as ccgp <profile> --dangerously-skip-permissions ...
                                   (Claude Code without permission prompts)
`)
	for _, c := range commands() {
		if c.hidden {
			continue
		}
		head := strings.TrimSpace(c.name + " " + c.args)
		_, _ = fmt.Fprintf(w, "  ccgp %-34s %s\n", head, c.desc)
	}
	_, _ = fmt.Fprint(w, `
Profiles:
  ccgp --profile add [name] --like <profile>      New profile with its OWN login (e.g. a second Codex account: ccgp codex2)
  ccgp --profile add <name> --provider <p>        New profile on an existing login [--model M [--window N]]
  ccgp --profile add <name> --type <t> ...        New backend: codex | codex_api | anthropic_compat [--base-url URL] [--model M]
  ccgp --profile add <name> --type claude         The real Claude Code with its own login (a clean dir: settings only, no ids or tokens)
  ccgp --profile add <name> --type claude_api     The real Claude Code on the Claude Platform with an API key of its own [--base-url URL]
  ccgp --profile list                             Show profiles, providers, login state and Claude dirs
  ccgp --profile set <name> <key> <value>         One setting: default_model, context_window, compact_percent, provider (unset removes)
  ccgp --provider [name]                          Show the providers, or one in detail
  ccgp --provider set <name> <key> <value>        base_url or api_key_env of a provider (unset removes)
  ccgp --profile rm <name> [--yes] [--purge]      Remove a profile (its provider too when unused; --purge deletes data)

Proxy (SOCKS5 or HTTP; the nearest level replaces the ones above it: profile, provider, global):
  ccgp --connections proxy socks5://127.0.0.1:1080            Everybody goes through it (off removes it)
  ccgp --connections proxy http://corp:3128 provider codex    All profiles of one provider
  ccgp --connections proxy none profile home                  One profile, directly (inherit: the level above again)
  ccgp --connections                                          Show the limits and every proxy

Menus and peak hours:
  ccgp -i                                         Full-screen interface to view, edit and add profiles, providers, peak hours
  ccgp --peak                                     Peak hours of every provider, and whether it is peak now
  ccgp --peak zai set mon-fri 14:00-18:00         Refuse zai's requests in those hours (zone: ccgp --peak zai tz Asia/Shanghai)
  ccgp --peak zai pause 2h                        Work anyway for two hours (resume ends the pause)

Config (Ktav):
  Windows: %APPDATA%\cc-go-proxy\config.ktav   Linux: ~/.config/cc-go-proxy/config.ktav
  Override: CCGP_CONFIG. Created automatically on first run.
  Credentials are stored separately in <config dir>/cc-go-proxy/creds (owner-only).

Home (a setup that shares nothing with the default one: config, logins, URL tokens, Claude dirs, log):
  ccgp --local <profile|--command> ...   keep everything in ./.ccgp (made on first use, git-ignored)
  ccgp --home <dir> <profile|--command>  keep everything in <dir> (also --home=<dir>)
  The flags come first. Without them: CCGP_HOME=<dir>, else a .ccgp directory in the current directory is picked up.
  A new home gets its own free port, so it runs its own server. ccgpy takes the same flags.

Env-only mode (no config.ktav):
  OPENAI_API_KEY, PROXY_TOKEN (min 32 chars), OPENAI_BASE_URL, HOST, PORT
  Profile name: default

Example:
  ccgp codex -p "hello"
  ccgpy codex
  ccgp --local --login codex        a separate login and settings in ./.ccgp
  ccgp --home ~/work/ccgp work      profile work on the setup in ~/work/ccgp
`)
}
