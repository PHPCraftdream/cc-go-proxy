# CLI reference

```
ccgp <profile> [claude args…]     run Claude Code through a profile
ccgp --<command> [args…]          everything else
```

Commands start with `--`. A word without dashes is always a profile, so a profile may be called `status` or `login`;
only `claude` is reserved (the endpoint name in `ccgp --send claude <profile>`). An unknown first argument that
starts with a dash is an error (`unknown command "--stauts", see: ccgp --help`), never a profile run. Everything after
the profile goes to `claude` untouched: `ccgp codex --help` is Claude Code's help.

`ccgp --<command> --help` prints the help of that command, `ccgp --help` lists them all, `ccgp --version` prints the
version. `ccgpy <profile> …` is `ccgp <profile> --dangerously-skip-permissions …`.

## Server

| Command | |
| --- | --- |
| `--start` | Start the background server (detached; log in the user cache directory) |
| `--stop` | Stop it |
| `--restart` | Stop and start it, also when requests are in flight |
| `--status [--json]` | Server, log, config and creds paths, login state of every provider and profile |
| `--logs [-f] [-n N]` | Last `N` lines of the server log (default 50); `-f` follows it, also across log rotation |
| `--serve [-d] [-s]` | Run the server in the foreground (`-d` dumps bodies, `-s` one line per request) |

### Reading the log

Every request has one line: start time, status, the time until the backend's reply headers, the route and, for
failures, the error. A streamed Codex answer that goes wrong or is slow gets a second line:

- `stream failed after …`: the stream broke after it started. The reason, then the raw backend event that ended it
  (`upstream event: {…}`, for `error` / `response.failed`), the backend's `x-request-id` / `cf-ray` and the timing.
- `slow stream …`: the reply headers took over 20 s or the answer over 2 minutes. The timing only.
- `stream canceled by client after …`: Claude Code left the stream (Esc, a newer request); the timing only.
- `upstream stats [client]: …`: every 10 minutes, for each upstream client that did something: connections opened
  (`dials 3 (h2 2, http/1.1 1)`: how many and the protocol they agreed on), background warm-ups (`opened`, `used`,
  `failed`) and retries (after a connection could not open, after one broke). A warm-up count of `used` far below
  `opened`, or `http/1.1` where `h2` is expected, shows a connection problem before it becomes errors.
- `upstream rate limit: retrying in …`: the backend answered 429 with a `Retry-After` of up to 5 s, so ccgp waited it
  out and asked once more. The line lists the limit headers (`retry-after`, `x-ratelimit-*`, `*reset*`, `x-codex-*`);
  a longer `Retry-After` is passed on to Claude Code in the 429 reply (header included) and the limits go to the log
  line of the 429.
- `upstream retry N/M in …`: the backend said it is overloaded (`server_is_overloaded` and similar) before sending
  any content, so ccgp sends the request again after the pause (2 s, then 5 s); Claude Code just waits longer.
  Only when the retries run out does the stream fail.
- `upstream reconnect N after …`: the connection of an attempt failed (could not open, or broke before the reply) and
  ccgp sent the request over another one. One line per retry, with the error and the kind of connection the failed
  try was on; the client sees nothing unless the retries run out. Lines with the same error in a row, or many
  `reused, idle …` ones, point at the network or a program on this machine, not at the backend.

The timing: `conn new (dns, connect, tls, pool …)`, `conn reused (idle …, pool …)` or `conn warm (opened … before)` (a
connection opened in the background for this lane). `pool` is the age of the pool the request went to: a connection
is at most that old, and a `conn new` on an old pool means its earlier connection was lost. Then the number of upstream calls when there was more than one (a retry, a
token refresh) and `retried after: <error> (the first try was on a new / warm / reused, idle … connection)` when the first attempt
failed. An error such as `wsasend: An established connection was aborted by the software in your host machine` is
Windows closing the connection itself (a proxy program or an antivirus); the idle time of the connection shows
whether it is the long-idle ones that die; then, from the start of the call, `headers`,
`first token` (first thinking, text or tool input) and `total`; the request size; the tokens (input, cached, output,
reasoning). Long `headers` with a reused connection means the backend was slow to start; a long gap from `headers` to
`first token` is the model reading a long prompt (input tokens) or thinking (reasoning tokens); a long `total` after
an early first token means a long answer.

### A server from an older binary

After you reinstall or update ccgp, a server that is still running keeps the old code. `/health` reports the
server's binary path and its modification time at start, and the number of model requests in flight (a streamed
answer counts until it ends). `ccgp <profile>` compares them with its own binary:

- idle server, older binary: it is restarted (`ccgp: restarted the server (…)`);
- requests in flight: a warning, the server is kept; `ccgp --restart` forces the restart.

Servers started before this check lack the fields and are only compared by version.

## Checks

`ccgp --doctor [--offline] [--json]` runs one diagnosis and exits non-zero if something is broken (warnings do not):

| Check | |
| --- | --- |
| `claude` | found on `PATH` |
| `config` | loads (or does not exist yet) |
| `server` | running, and not older than this binary |
| `login <provider>` | signed in; age of the Codex refresh token |
| `backend <provider>` | the base URL answers (any HTTP status), skipped with `--offline`; no credentials are sent |
| `creds dir` | owner-only permissions (Unix; Windows ACLs are set by ccgp) |
| `log file` | the log directory is writable |

`ccgp --which <profile> <model> [--json]` answers offline where a Claude model name goes in a profile: the backend
model, provider, context window and which rule matched (`rule <glob>`, `default_model`, `tier setting` or
`passthrough`). It is the command-line twin of `GET /call/<token>/model?requested=<model>`.

## Machine-readable output

`--status`, `--profile list`, `--models` (the read-only form), `--which` and `--doctor` accept `--json`: one
pretty-printed document on stdout. Field names are lowerCamelCase; URL tokens, API keys and OAuth tokens never appear.
Without the flag the text output is unchanged. `--status` still exits with code 3 when the server is not running,
after printing the JSON.

## Logins and profiles

| Command | |
| --- | --- |
| `--login <provider> [--device]` | Sign in (Codex: browser OAuth, or a device code; others: API key) |
| `--logout <provider> [--yes]` | Remove the stored OAuth tokens or API key of a provider after a confirmation. The config file is not touched, and `--yes` is required without a terminal |
| `--profile add\|list\|rm` | See [configuration](configuration.md#creating-and-removing-profiles) |
| `--models [apply] [-v] [profile]` | Show or write the model mapping |
| `--config reset` | Back up the config and write the default one |
| `--connections [active N] [total N] [proxy URL|off [profile NAME]] [--json]` | Show or set the connections per backend (default 7 active, 10 total) and the proxy, global or of one profile (`none` = direct, `inherit` = the global one again); see [configuration](configuration.md#layout) |

## Sessions

`--send <from> <to> [count]`, `--import <profile> [count]`, `--export <profile> [count]`: see
[sessions](sessions.md).

## Updating

`ccgp --update [--check]` looks up the latest release (`/releases/latest` redirect, no API rate limit), downloads the
archive for this OS and CPU and `SHA256SUMS`, verifies the checksum before touching anything, and replaces `ccgp`
(and `ccgpy`, if it sits next to it) in the directory of the running binary. On Windows the running file is renamed
to `.old` first. Config and credentials are not touched; `--check` only reports whether an update exists. When a
server is running, restart it afterwards with `ccgp --restart`.

Installed with npm, update with npm instead (`npm install -g cc-go-proxy@latest`): `--update` would replace the binary
inside the npm package, and the next npm install would put the packaged version back.

## Shell completion

```sh
source <(ccgp --completion bash)        # bash
source <(ccgp --completion zsh)         # zsh
ccgp --completion powershell | Out-String | Invoke-Expression   # PowerShell
```

Add the line to your shell profile to keep it. Commands, subcommands, flags and the names of your profiles and
providers are completed (`ccgpy` completes profile names). The scripts only call the hidden `ccgp --complete`; the
candidates are computed by ccgp from its command table and your config, and a broken config just means no names.
