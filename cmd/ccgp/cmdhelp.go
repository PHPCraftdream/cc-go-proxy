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
	"--status": `ccgp --status

Show whether the background server runs, where its log and config live, and
the login state of every configured provider and profile.

Example:
  ccgp --status
`,
	"--login": `ccgp --login <provider> [--device]

Authenticate a provider.

  codex      browser OAuth flow by default; --device uses the device-code flow
  codex_api  asks for an API key, or reads the provider's api_key_env
  anthropic_compat
             asks for an API key

Examples:
  ccgp --login codex
  ccgp --login codex --device
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
	"--models": `ccgp --models [apply] [-v] [profile]

Show the newest Codex models and how they map to the Claude model tiers.
With "apply", write the mapping into the ccgp:managed-models block of the
profile's provider in config.ktav (a backup is kept).

  -v, --verbose    list every model seen upstream

Examples:
  ccgp --models
  ccgp --models apply codex
`,
	"--send": `ccgp --send <from> <to> [count]

Copy the latest session transcripts of the current directory from one side
to the other ("claude" or a profile name). Default count: 10. Never
overwrites diverged data.

Examples:
  ccgp --send claude codex
  ccgp --send codex claude 3
`,
	"--import": `ccgp --import <profile> [count]

Same as: ccgp --send claude <profile>

Example:
  ccgp --import codex
`,
	"--export": `ccgp --export <profile> [count]

Same as: ccgp --send <profile> claude

Example:
  ccgp --export codex
`,
	"--profile": `ccgp --profile add|list|rm

Manage profiles (and their providers) in config.ktav.

  add <name> --like <profile>      new profile with its OWN login
  add <name> --provider <provider> new profile on an existing login
                                   both accept [--model M [--window N]]
  add <name> --type <t> ...        new backend: codex | codex_api |
                                   anthropic_compat [--base-url URL]
  list                             profiles, providers, login state
  rm <name> [--yes] [--purge]      remove a profile; --purge also deletes its
                                   Claude dir and unused provider credentials

Examples:
  ccgp --profile add codex2 --like codex
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
	"--update": `ccgp --update [--check]

Update ccgp (and ccgpy, when installed next to it) from the latest GitHub
release. The download is verified against the release's SHA256SUMS before
anything is replaced; the running binary is replaced safely (a backup is
kept while it runs). Config and credentials are not touched.
Installed with npm (cc-go-proxy)? Update with npm install -g cc-go-proxy.

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
