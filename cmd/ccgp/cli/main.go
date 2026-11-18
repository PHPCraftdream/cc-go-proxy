// Command ccgp is a headless Claude Code -> OpenAI/Codex bridge.
//
//	ccgp [--local|--home <dir>] ...   keep all files in ./.ccgp or in <dir> (also CCGP_HOME, or a .ccgp dir in the current directory)
//	ccgp <profile> [claude args...]   start server if needed, ensure creds, run claude
//	ccgp . [claude args...]           choose the profile (or your own claude) from a list
//	ccgp --serve [-d] [-s]            run server in foreground
//	ccgp --start|--stop|--status      manage background server
//	ccgp --login <provider> [--device] (re)authenticate a provider
//	ccgp --send <from> <to> [n] [--fork] [--force]  copy the n latest sessions of the current directory between "claude" and profiles
//	ccgp --import|--export <profile> [n] [--fork] [--force]  aliases of send: claude -> profile / profile -> claude
//	ccgp --profile add|list|rm        manage profiles (and their providers) in config.ktav
//
// Commands are written with a leading "--"; a bare word is always a profile.

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/common"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/core"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/ops"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/sess"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/tune"
	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/config/cfgedit"
	"github.com/PHPCraftdream/cc-go-proxy/internal/home"
	"github.com/PHPCraftdream/cc-go-proxy/internal/server"
	_ "golang.org/x/crypto/x509roots/fallback"
)

var version = "ccgp v" + server.ProxyVersion

// pickArg in place of a profile opens a list to choose one from:
// ccgp . [claude args...]. ccgpy without a profile passes it.
const PickArg = "."

// InteractiveHook and LaunchPickedHook are the interface's entry points,
// set by the program (the interface sits above the commands).
var (
	InteractiveHook  func(args []string) error
	LaunchPickedHook func(args []string) error
)

func Main() {
	if err := run(os.Args[1:]); err != nil {
		var ee common.ExitError
		if errors.As(err, &ee) {
			os.Exit(int(ee))
		}
		fmt.Fprintf(os.Stderr, "ccgp: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opt, args, err := home.Parse(args)
	if err != nil {
		return err
	}
	if err := opt.Use(createsHome(args)); err != nil {
		return err
	}
	inv, err := ParseInvocation(args)
	if err != nil {
		return err
	}
	if inv.Cmd == nil {
		if inv.Profile == "" {
			printHelp(os.Stdout)
			return nil
		}
		if err := ensureConfig(); err != nil {
			return err
		}
		if inv.Profile == PickArg {
			return LaunchPickedHook(inv.Args)
		}
		return core.Launch(inv.Profile, inv.Args)
	}
	// Per-command help runs before ensureConfig, so --help always works.
	if handleHelp(inv, os.Stdout) {
		return nil
	}
	if inv.Cmd.EnsureConfig {
		if err := ensureConfig(); err != nil {
			return err
		}
	}
	return inv.Cmd.Run(inv.Args)
}

// createsHome tells whether the command may make the directory of --local and
// --home: help, the version and completion write nothing.
func createsHome(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-h", "--help", "--version", "--complete":
		return false
	}
	return len(args) < 2 || (args[1] != "--help" && args[1] != "-h")
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
	Home      string           `json:"home,omitempty"` // the home directory in use; empty: the default layout
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
	case cfgErr != nil && common.MissingDefaultConfig():
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
	if s.Home != "" {
		_, _ = fmt.Fprintf(w, "home:    %s\n", s.Home)
	}
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
func Status(args []string) error {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		} else {
			return fmt.Errorf("--status: unknown flag %q", a)
		}
	}
	host, port := common.ListenAddr()
	base := angel.BaseURL(host, port)
	logPath, _ := angel.LogPath()
	h, healthErr := angel.Check(base)
	cfg, cfgErr := config.Load()
	credsDir := ""
	var credsErr error
	var providers []statusProvider
	var profiles []statusProfile
	if cfgErr == nil && !cfg.IsEnvMode() {
		store, err := common.CredsStore()
		if err != nil {
			credsErr = err
		} else {
			credsDir = store.Dir
			for _, name := range common.SortedKeys(cfg.Providers) {
				pr := cfg.Providers[name]
				providers = append(providers, statusProvider{Name: name, Type: pr.Type, Login: common.LoginState(store, pr)})
			}
			for _, name := range common.SortedKeys(cfg.Profiles) {
				profiles = append(profiles, statusProfile{Name: name, Provider: cfg.Profiles[name].Provider})
			}
		}
	}
	st := buildStatus(base, logPath, h, healthErr, cfg, cfgErr, credsDir, credsErr, providers, profiles)
	st.Home = home.Root()
	out := io.Writer(os.Stdout)
	var err error
	if jsonOut {
		err = common.WriteJSON(out, st)
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
		return common.ExitError(3)
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
	// From here on the Codex client version comes only from the config.
	if seeded, err := cfgedit.SeedCodexClientVersion(cfg); err != nil {
		fmt.Printf("[WARN] could not write codex.client_version: %v\n", err)
	} else if seeded {
		fmt.Printf("codex.client_version %s written to %s\n", cfg.CodexClientVersion(), cfg.ConfigFile)
	}
	return server.Start(cfg)
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

// Command is one CLI Command, named with a leading "--". The table below is
// the single source for dispatch and the help text.
type Command struct {
	Name         string   // "--status"
	Args         string   // synopsis after the name
	Desc         string   // one-line description
	EnsureConfig bool     // create the default config first
	Hidden       bool     // helper command, skipped by printHelp
	Verbs        []string // entry points beyond the command itself ("set default_model"); the parity test and the completion read them
	Run          func(args []string) error
}

// commands is ordered; later tasks append entries.
func Commands() []Command {
	return []Command{
		{Name: "--serve", Args: "[-d] [-s]", Desc: "Run server in foreground (-d debug, -s request log)", EnsureConfig: true, Run: serveCmd},
		{Name: "--start", Args: "", Desc: "Start background server", EnsureConfig: true, Run: startCmd},
		{Name: "--stop", Args: "", Desc: "Stop background server", Run: stopCmd},
		{Name: "--status", Args: "[--json]", Desc: "Show background server status", Run: statusCmd},
		{Name: "--login", Args: "<provider> [--device]", Desc: "Authenticate (codex: browser OAuth, --device: device code; codex_api: API key)", EnsureConfig: true, Run: loginCmd, Verbs: []string{"--device"}},
		{Name: "--models", Args: "[apply] [--json] [-v] [profile]", Desc: "Show (or write into config) the newest Codex models per Claude tier", EnsureConfig: true, Run: modelsCmd, Verbs: []string{"apply"}},
		{Name: "--send", Args: "<from> <to> [count] [--fork] [--force]", Desc: "Copy the latest sessions (default 10) of the current directory between \"claude\" and a profile (--fork: keep a diverged one as a new session, --force: replace it, both: write a diverged one afresh)", EnsureConfig: true, Run: sessionsSendCmd, Verbs: sess.SessionVerbs},
		{Name: "--import", Args: "<profile> [count] [--fork] [--force]", Desc: "Same as: ccgp --send claude <profile>", EnsureConfig: true, Run: sessionsImportCmd, Verbs: sess.SessionVerbs},
		{Name: "--export", Args: "<profile> [count] [--fork] [--force]", Desc: "Same as: ccgp --send <profile> claude", EnsureConfig: true, Run: sessionsExportCmd, Verbs: sess.SessionVerbs},
		{Name: "--profile", Args: "add|list [--json]|rm|set|unset", Desc: "Manage profiles (and their providers) in config.ktav", EnsureConfig: true, Run: profileDispatchCmd, Verbs: core.ProfileVerbs()},
		{Name: "--provider", Args: "[NAME] [--json] | set|unset NAME KEY [VALUE]", Desc: "Show the providers, or set base_url / api_key_env of one", EnsureConfig: true, Run: core.ProviderCmd, Verbs: core.ProviderVerbs()},
		{Name: "--interactive", Args: "", Desc: "Full-screen interface that does everything the commands do (also: -i)", EnsureConfig: true, Run: func(a []string) error { return InteractiveHook(a) }},
		{Name: "-i", Args: "", Desc: "Same as: ccgp --interactive", EnsureConfig: true, Run: func(a []string) error { return InteractiveHook(a) }},
		{Name: "--config", Args: "reset", Desc: "Back up the config and regenerate the default one", Run: configCmd, Verbs: []string{"reset"}},
		{Name: "--connections", Args: "[active N] [total N] [proxy URL|off [provider NAME | profile NAME]] [--json]", Desc: "Show or set the upstream connections per backend (default 7 active, 10 total) and the proxy", EnsureConfig: true, Run: tune.ConnectionsCmd, Verbs: []string{"active", "total", "proxy", "proxy provider", "proxy profile"}},
		{Name: "--speed", Args: "[<profile> <family> [fast|standard|unset|inherit]]", Desc: "Show or set per-profile Codex family speed rules", Run: tune.SpeedCmd},
		{Name: "--peak", Args: "[provider [set|add WINDOW...|rm N...|tz ZONE|clear|pause [DURATION]|resume]] [--json]", Desc: "Show or set the peak hours of a provider, when its profiles' requests are refused", EnsureConfig: true, Run: tune.PeakCmd, Verbs: []string{"set", "add", "rm", "tz", "clear", "pause", "resume"}},
		{Name: "--which", Args: "<profile> <model> [--json]", Desc: "Show where a Claude model is routed in a profile (offline)", EnsureConfig: true, Run: core.WhichCmd},
		{Name: "--logs", Args: "[-f] [-n N]", Desc: "Show the background server log (-f follow, -n N lines, default 50)", Run: ops.LogsCmd},
		{Name: "--restart", Args: "", Desc: "Stop and start the background server", EnsureConfig: true, Run: ops.RestartCmd},
		{Name: "--logout", Args: "<provider> [--yes]", Desc: "Remove the stored login (OAuth tokens or API key) of a provider", EnsureConfig: true, Run: core.LogoutCmd},
		{Name: "--completion", Args: "bash|zsh|powershell", Desc: "Print a shell completion script (see its usage error for the install line)", EnsureConfig: false, Run: completionCmd},
		{Name: "--update", Args: "[--check]", Desc: "Self-update from the latest GitHub release (--check: only report)", Run: ops.UpdateCmd, Verbs: []string{"--check"}},
		{Name: "--doctor", Args: "[--offline] [--json]", Desc: "Run diagnostics (claude, config, server, logins, backends, files)", EnsureConfig: false, Run: ops.DoctorCmd},
		{Name: "--version", Args: "", Desc: "Show version", Run: versionCmd},
		{Name: "--help", Args: "", Desc: "Show this help", Run: helpCmd},
		{Name: "--complete", Args: "<prog> <word>...", Desc: "", Hidden: true, Run: completeCmd},
	}
}

func FindCommand(name string) (Command, bool) {
	for _, c := range Commands() {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// Invocation is a parsed command line: either a command (cmd != nil) or a
// profile run (profile != ""). cmd == nil with an empty profile means help
// (no args, -h or --help); the caller prints it.
type Invocation struct {
	Cmd     *Command
	Profile string
	Args    []string
}

// parseInvocation classifies args without side effects. A first argument
// starting with "-" must name a command; a bare word is always a profile and
// the rest of the arguments go to claude verbatim.
func ParseInvocation(args []string) (Invocation, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return Invocation{}, nil
	}
	if args[0] == "" {
		return Invocation{}, errors.New("empty first argument: give a profile or a --command, see: ccgp --help")
	}
	if strings.HasPrefix(args[0], "-") {
		c, ok := FindCommand(args[0])
		if !ok {
			return Invocation{}, fmt.Errorf("unknown command %q, see: ccgp --help", args[0])
		}
		return Invocation{Cmd: &c, Args: args[1:]}, nil
	}
	return Invocation{Profile: args[0], Args: args[1:]}, nil
}

func serveCmd(args []string) error {
	return serve(args)
}

func startCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h, err := ops.EnsureServer(cfg, args)
	if err != nil {
		return err
	}
	fmt.Printf("ccgp running (pid %d) at %s\n", h.PID, angel.BaseURL(cfg.Host, cfg.Port))
	return nil
}

func stopCmd(args []string) error {
	host, port := common.ListenAddr()
	if err := angel.StopBy("ccgp --stop")(angel.BaseURL(host, port), 5*time.Second); err != nil {
		return err
	}
	fmt.Println("ccgp stopped")
	return nil
}

func statusCmd(args []string) error {
	return Status(args)
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
	return core.Login(cfg, rest[0], true, device)
}

func modelsCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return core.Models(cfg, args)
}

func sessionsSendCmd(args []string) error { return sess.SessionsCmd("send", args) }

func sessionsImportCmd(args []string) error { return sess.SessionsCmd("import", args) }

func sessionsExportCmd(args []string) error { return sess.SessionsCmd("export", args) }

func profileDispatchCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return core.ProfileCmd(cfg, args)
}

func versionCmd(args []string) error {
	fmt.Println(version)
	return nil
}

func helpCmd(args []string) error {
	printHelp(os.Stdout)
	return nil
}

// helpColumn is the width of the command column of the help text.
const helpColumn = 34

// printHelp writes the help text, with the command lines from the table.
func printHelp(w io.Writer) {
	_, _ = fmt.Fprint(w, `ccgp - Claude Code bridge to OpenAI-compatible APIs and ChatGPT Codex

Usage:
  ccgp <profile> [claude args...]  Ensure server + credentials, then run claude
                                   (a word without dashes is always a profile: ccgp codex runs profile codex;
                                   commands start with --)
  ccgp . [claude args...]          Choose the profile from a list (or claude: your own Claude Code, plain)
  ccgpy <profile> [claude args...] Same as ccgp <profile> --dangerously-skip-permissions ...
                                   (Claude Code without permission prompts; ccgpy alone = ccgpy .)
`)
	for _, c := range Commands() {
		if c.Hidden {
			continue
		}
		head := strings.TrimSpace(c.Name + " " + c.Args)
		if len(head) > helpColumn {
			// Too long for the column: the description goes under it.
			_, _ = fmt.Fprintf(w, "  ccgp %s\n  %-*s %s\n", head, helpColumn+5, "", c.Desc)
			continue
		}
		_, _ = fmt.Fprintf(w, "  ccgp %-*s %s\n", helpColumn, head, c.Desc)
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
  macOS:   ~/Library/Application Support/cc-go-proxy/config.ktav   (ccgp --status prints the real paths)
  Override: CCGP_CONFIG. Created automatically on first run.
  Credentials are stored separately in creds/ next to it (owner-only).

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
