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
	fmt.Fprint(w, commandHelpText(c))
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
