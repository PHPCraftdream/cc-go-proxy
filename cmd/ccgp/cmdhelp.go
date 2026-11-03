package main

import (
	"fmt"
	"io"
	"strings"
)

// commandHelp holds the long help text of commands that need one, keyed by
// the command's name. Commands without an entry get the generic fallback from
// commandHelpText, so rows added later still answer --help.
var commandHelp = map[string]string{
	"--serve": `ccgp --serve [-d] [-s]

Run the proxy server in the foreground.

  -d    debug logging
  -s    simple request log (one line per request)

Fails when a server is already running; use ccgp --start for the background
server.

Example:
  ccgp --serve -s
`,
	"--start": `ccgp --start

Start the proxy server as a detached background process and wait until it
answers on /health. Does nothing but report when it already runs.

Example:
  ccgp --start
`,
	"--stop": `ccgp --stop

Stop the background proxy server.

Example:
  ccgp --stop
`,
	"--status": `ccgp --status [--json]

Show whether the background server runs, where its log and config live, and
the login state of every configured provider and profile. A "home:" line
appears when a separate home (--local, --home, CCGP_HOME, ./.ccgp) is in use.

  --json    print the same as one JSON document

Exit code 3 when the server is not running (after printing).

Examples:
  ccgp --status
  ccgp --status --json
  ccgp --local --status
`,
	"--login": `ccgp --login <provider> [--device]

Authenticate a provider (the provider name, not a profile name; a profile made
with --like has a provider of the same name).

  codex      browser OAuth flow by default (prints a link, never opens the
             browser); --device uses the device-code flow
  codex_api  asks for an API key, or reads the provider's api_key_env
  anthropic_compat
             asks for an API key
  claude     nothing to do here: start the profile (ccgp <profile>) and sign
             in inside Claude Code with /login

Examples:
  ccgp --login codex
  ccgp --login codex --device
  ccgp --login zai
`,
	"--which": `ccgp --which <profile> <model> [--json]

Show, offline, where a Claude model name goes in a profile: the backend model,
the provider, the context window and what matched (rule <glob>, default_model,
tier setting or passthrough). The command-line twin of
GET /call/<token>/model?requested=<model>. A profile of type claude has no
mapping and says so.

  --json    print the answer as one JSON document

Examples:
  ccgp --which codex claude-opus-4-1
  ccgp --which zai sonnet --json
`,
	"--logs": `ccgp --logs [-f] [-n N]

Show the tail of the background server's log (see ccgp --status for its path).

  -n N    how many lines (default 50)
  -f      follow the log as it grows, also across log rotation (Ctrl+C stops)

Examples:
  ccgp --logs
  ccgp --logs -n 200
  ccgp --logs -f
`,
	"--restart": `ccgp --restart

Stop the background server (when one runs) and start a new one from this
binary, also when requests are in flight. Use it after ccgp --update or a
reinstall; ccgp <profile> restarts an idle outdated server by itself and only
warns when requests are running.

Example:
  ccgp --restart
`,
	"--completion": `ccgp --completion bash|zsh|powershell

Print a shell completion script for ccgp and ccgpy. Commands, flags and the
names of your profiles and providers are completed.

Examples:
  source <(ccgp --completion bash)
  source <(ccgp --completion zsh)
  ccgp --completion powershell | Out-String | Invoke-Expression

Put the line in your shell profile to keep it.
`,
	"--logout": `ccgp --logout <provider> [--yes]

Remove the stored login of a provider (OAuth tokens or API key). The config
file is not touched, and other providers keep their logins.

  --yes    skip the confirmation question

Examples:
  ccgp --logout codex
  ccgp --logout codex --yes
`,
	"--doctor": `ccgp --doctor [--offline] [--json]

Run diagnostics and report every check as ok, warn or FAIL; exit code 1 when
a check fails.

Checked: the claude binary, the config, the background server (staleness vs.
this binary), the login state of every provider (token age for Codex),
reachability of each backend base URL, the creds directory permissions and
the server log directory.

  --offline    skip the external backend reachability probes (the local server check still runs)
  --json       print the report as one JSON document

Example:
  ccgp --doctor
`,
	"--models": `ccgp --models [apply] [--json] [-v] [profile]

Show the newest Codex models and how they map to the Claude model tiers (and
the built-in mapping of providers with a preset, such as z.ai). With "apply",
write the mapping into the ccgp:managed-models block of the profile in
config.ktav. Without a profile name, every generated profile is shown or
written. Needs config.ktav.

  apply            write the mapping into the config
  --json           print the mapping as JSON (the show form only)
  -v, --verbose    list every model seen upstream, with all catalog fields

Examples:
  ccgp --models
  ccgp --models --json codex
  ccgp --models apply codex
  ccgp --models apply zai
`,
	"--send": `ccgp --send <from> <to> [count]

Copy the latest session transcripts of the current directory from one side
to the other. A side is "claude" (your own Claude Code) or a profile name;
the two must differ. Default count: 10, newest first. Only sessions of the
directory you run it in are copied (what "ccgp <profile> -c" and /resume
list). Never overwrites diverged data; a session being written is copied up
to its last complete line. Thinking blocks are removed when the two sides use
different providers (their signatures are not valid elsewhere).

Examples:
  ccgp --send claude codex
  ccgp --send codex claude 3
  ccgp --send codex codex2 1
`,
	"--import": `ccgp --import <profile> [count]

Same as: ccgp --send claude <profile>

Examples:
  ccgp --import codex
  ccgp --import work 3
`,
	"--export": `ccgp --export <profile> [count]

Same as: ccgp --send <profile> claude

Examples:
  ccgp --export codex
  ccgp --export zai 3
`,
	"--profile": `ccgp --profile add|list|rm

Manage profiles (and their providers) in config.ktav. Every change is
validated with the real loader and keeps a config.ktav.bak-<time> backup.

  add [name] --like <profile>      new profile with its OWN login, copying the
                                   source's provider type, URL and model rules
                                   (no name: the next free <source>N)
  add <name> --provider <provider> new profile on an existing login
  add <name> --type <t> ...        new backend: codex | codex_api |
                                   anthropic_compat [--base-url URL]
  add <name> --type claude         the real Claude Code with its own login,
                                   nothing through ccgp; only settings, skills
                                   and CLAUDE.md are copied, no ids, no tokens
      --like, --provider and --type (not claude) accept [--model M [--window N]]:
      one catch-all rule; without it the rules of the source are copied
  list [--json]                    profiles, providers, login state, Claude dirs
  rm <name> [--yes] [--purge]      remove a profile (and its provider when
                                   nothing else uses it); --purge also deletes
                                   its Claude dir and unused provider credentials

add never signs in: ccgp <name> does on its first start.

Examples:
  ccgp --profile add codex2 --like codex
  ccgp --profile add codex-alt --provider codex
  ccgp --profile add myai --type anthropic_compat --base-url https://example.internal/api --model my-model
  ccgp --profile add work --type claude
  ccgp --profile list --json
  ccgp --profile rm codex2 --purge
`,
	"--config": `ccgp --config reset

Back up the current config.ktav and write the default template in its place.
Edit the new file to configure providers, profiles and model names.

Example:
  ccgp --config reset
`,
	"--connections": `ccgp --connections [active N] [total N] [proxy URL|off [profile NAME]] [--json]

Show or set how many connections ccgp keeps to each backend (config.ktav:
connections.active, connections.total). Active connections take new
requests; each request goes to the least busy one. Every connection is
replaced after 10 minutes, before backends cut it, and the old one closes
when its last request ends; total caps active plus still finishing ones, so
a replacement waits while the limit is reached. A running request is never
cut. The running server applies changes on its next request.

  active N   connections taking new requests (default 7, 0: default)
  total N    limit for active plus finishing (default 10, at least active)
  proxy URL  send every upstream request (models, logins, catalog) through a
             proxy: socks5://[user:pass@]host:port (socks5h too: the proxy
             resolves names), http://… or https://… (CONNECT); off: none
  proxy V profile NAME
             a proxy of one profile; it replaces the global one for it.
             V: a proxy URL; none (or off): no proxy at all for the profile;
             inherit: remove the override, the global proxy applies again
  --json     print the values as JSON

Examples:
  ccgp --connections
  ccgp --connections active 7 total 10
  ccgp --connections proxy socks5://127.0.0.1:1080
  ccgp --connections proxy off
  ccgp --connections proxy socks5://10.0.0.2:1080 profile work
  ccgp --connections proxy none profile home
  ccgp --connections proxy inherit profile work
`,
	"--interactive": `ccgp --interactive
ccgp -i

Menus to look at, change and add profiles and providers without remembering
the commands. Only this flag starts it; every other command stays as it is.
Each screen reads config.ktav afresh and every change goes through the same
editor as the commands: validated with the real loader, with a
config.ktav.bak-<time> backup. Answer with the number (or letter) of a choice;
an empty line goes back, the end of the input (Ctrl-D, Ctrl-Z on Windows)
quits. Answers can also come from a pipe.

  profiles   list them (login, provider, type); open one to see its settings and
             set default_model, context_window, compact_percent, proxy, move it
             to another provider, or remove it; add one:
             - another account of the kind a profile has (--profile add --like)
             - another profile on a login that exists (--provider)
             - a new backend: codex, codex_api, anthropic_compat, claude (--type)
  providers  list them (type, profiles, peak hours); open one to see its
             base_url, api_key_env, login, effort_map, peak hours and profiles,
             set base_url and api_key_env, and manage its peak hours (windows,
             zone, pause); add a provider (it comes with its first profile)

A provider goes when its last profile is removed. Models (the rule lists) and
effort_map are shown and edited with ccgp --models or in the file.

Examples:
  ccgp --interactive
  ccgp -i
  printf '1\n0\n0\n' | ccgp -i       (profiles, back, quit)
`,
	"-i": `ccgp -i

Same as: ccgp --interactive

Example:
  ccgp -i
`,
	"--peak": `ccgp --peak [provider [set|add WINDOW...|rm N...|tz ZONE|clear|pause [DURATION]|resume]] [--json]

Peak hours of a provider (config.ktav: peak, peak_tz on the provider). In
them ccgp refuses the requests of its profiles with an error that Claude
Code shows in the chat (400, never sent to the model or the provider, so it
does not touch the cache or the conversation); a running request is not cut.
Token counting and the other routes go on. The running server applies a
change on its next request.

A WINDOW is [days] HH:MM-HH:MM in the provider's zone:
  days    mon-fri, sat,sun, fri-mon (wraps the week); without it: every day
  time    to is exclusive; 24:00 ends the day; to before from runs past
          midnight (22:00-06:00; its days are the days it starts on)

  (no provider)  every provider: windows, zone, and whether it is peak now
  <provider>     one provider (a profile name stands for its provider)
  set W...       replace all windows
  add W...       add windows
  rm N...        remove windows by their numbers in the list
  tz ZONE        the zone the windows are given in: a name (Asia/Shanghai), an
                 offset (+08:00), UTC, or local (the machine's, the default)
  clear          remove the peak hours (windows and zone)
  pause [LEN]    let requests through for LEN (90m, 2h, 1d; at most 7d), by
                 default until the running peak ends; kept apart from the config
  resume         end the pause
  --json         print the state as JSON

Examples:
  ccgp --peak
  ccgp --peak zai set mon-fri 14:00-18:00
  ccgp --peak zai tz Asia/Shanghai
  ccgp --peak zai add sat,sun 10:00-12:00 20:00-23:00
  ccgp --peak zai rm 2
  ccgp --peak zai pause 2h
  ccgp --peak zai resume
  ccgp --peak zai clear
`,
	"--update": `ccgp --update [--check]

Update ccgp (and ccgpy, when installed next to it) from the latest GitHub
release. The download is verified against the release's SHA256SUMS before
anything is replaced; the running binary is replaced safely (a backup is
kept while it runs). Config and credentials are not touched.
Installed with npm (@cc-go-proxy/ccgp)? Update with npm install -g @cc-go-proxy/ccgp.

  --check    only report whether an update is available

Examples:
  ccgp --update
  ccgp --update --check
`,
	"--version": `ccgp --version

Print the ccgp version.

Example:
  ccgp --version
`,
	"--help": `ccgp --help

Print the command overview.

Example:
  ccgp --help
`,
}

// commandHelpText returns the long help of a command: the entry from
// commandHelp, or a generic fallback for commands without one.
func commandHelpText(c command) string {
	if s, ok := commandHelp[c.name]; ok {
		return s
	}
	head := strings.TrimSpace(c.name + " " + c.args)
	return fmt.Sprintf("ccgp %s\n\n%s\n", head, c.desc)
}

// wantsHelp reports whether an invocation asks for per-command help
// (--help or -h as the first argument after a command). Profile runs are
// never intercepted: their --help goes to claude.
func wantsHelp(inv invocation) bool {
	return inv.cmd != nil && len(inv.args) > 0 && (inv.args[0] == "--help" || inv.args[0] == "-h")
}

// printCommandHelp writes the help text of a command.
func printCommandHelp(w io.Writer, c command) {
	_, _ = fmt.Fprint(w, commandHelpText(c))
}

// handleHelp prints the per-command help and reports whether it did. run()
// calls it before ensureConfig and dispatch, so `ccgp --cmd --help` never
// runs the command; profile runs are not intercepted (wantsHelp).
func handleHelp(inv invocation, w io.Writer) bool {
	if !wantsHelp(inv) {
		return false
	}
	printCommandHelp(w, *inv.cmd)
	return true
}
