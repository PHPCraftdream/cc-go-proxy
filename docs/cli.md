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

## Running a profile

```sh
ccgp codex                         # Claude Code through profile codex
ccgp codex -c                      # everything after the profile goes to claude: continue the last session
ccgp codex -p "explain this repo"  # headless
ccgp zai --model opus              # a Claude model name; the profile maps it to a backend model
ccgp work                          # a profile of type claude: the real Claude Code, no proxy (see below)
ccgpy codex                        # same as: ccgp codex --dangerously-skip-permissions
```

For a proxy profile (`codex`, `codex_api`, `anthropic_compat`) `ccgp <profile>`, in order: loads the config, signs in if
the provider has no login yet (a link for Codex, a key prompt otherwise), generates the profile's URL token, starts the
background server if it is not running (or restarts an idle one from an older binary), prepares the profile's own
Claude Code directory, copies the latest session of the current directory into a profile that has none of it
([sessions](sessions.md#the-first-launch-of-a-profile)), and runs `claude` with `ANTHROPIC_BASE_URL` pointing at the
profile's token URL and the compaction window of the session's main model set. The exit code is Claude Code's.

A profile of type `claude` (`ccgp --profile add work --type claude`) skips all of that: no server, no token, no
login by ccgp. `claude` runs with a clean directory of its own and you sign in inside it with `/login`
([configuration](configuration.md#profiles-of-the-real-claude-code---type-claude)).

`ccgpy <profile> …` finds the `ccgp` next to it (else in `PATH`) and runs `ccgp <profile>
--dangerously-skip-permissions …`; it takes the home flags too (`ccgpy --local work`) and passes the terminal and the
exit code through. It is for places where running Claude Code without permission prompts is acceptable.

## Own home: a setup that shares nothing

By default the config, logins, URL tokens, Claude Code dirs and the server log live in the user's config and cache dirs.
Two flags before the profile or the command move **all** of them into one directory, for a second setup that shares
no settings, tokens, logins or paths with the first:

```
ccgp --local <profile | --command> …     keep everything in ./.ccgp (created on first use)
ccgp --home <dir> <profile | --command> … keep everything in <dir> (also --home=<dir>)
ccgpy --local <profile> …                 ccgpy takes the same flags
```

The home holds `config.ktav`, `creds/`, `claude/<profile>/` and `logs/ccgp.log`. Which home is used, first match wins:

1. `--home <dir>` or `--local` (the flags come first; after the profile they belong to `claude`; not both together);
2. the `CCGP_HOME` environment variable (a directory);
3. a `.ccgp` directory in the **current** directory (parents are not searched): a project that has one runs on it
   without any flag;
4. the default layout.

Things to know:

- A new home gets a **free port** of its own in its `config.ktav` (`listen.port`), so it runs its own background
  server. The server is found by `host:port`; two homes on one port would talk to the same server. `--stop`, `--restart`,
  `--status` and `--logs` act on the server of the home you are in.
- The flags set `CCGP_HOME` for the process, so the background server and `claude` started from it keep the same home.
- A directory made by `--local` / `--home` gets a `.gitignore` holding `*`: it keeps logins and tokens, keep it out of git.
- `CCGP_CONFIG` still selects another config file; the rest stays in the home.
- A new home has no logins: `ccgp --local --login codex`. `ccgp --status` shows a `home:` line (and `"home"` in `--json`)
  when a home is in use.
- Your own Claude Code dir (`~/.claude`, or `CLAUDE_CONFIG_DIR`) is still the read-only source of the agents, skills and
  plugins copied into each profile's dir.

Examples:

```
cd ~/projects/client-a
ccgp --local --login codex          # a separate ChatGPT login for this project, in ./.ccgp
ccgp codex                          # picks ./.ccgp up by itself from now on
ccgp --status                       # home:    …/client-a/.ccgp

ccgp --home ~/work/ccgp --profile list
ccgpy --home ~/work/ccgp work -p "hello"
CCGP_HOME=~/work/ccgp ccgp --logs -f
```

## Server

| Command | |
| --- | --- |
| `--start` | Start the background server (detached; log in the user cache directory) |
| `--stop` | Stop it |
| `--restart` | Stop and start it, also when requests are in flight |
| `--status [--json]` | Server, log, config and creds paths, login state of every provider and profile |
| `--logs [-f] [-n N]` | Last `N` lines of the server log (default 50); `-f` follows it, also across log rotation |
| `--serve [-d] [-s]` | Run the server in the foreground (`-d` dumps bodies, `-s` one line per request) |

```sh
ccgp --start && ccgp --status      # start it, see the pid, address, paths and logins
ccgp --status --json               # the same for scripts; exit code 3 when the server is down
ccgp --logs -n 200                 # the last 200 lines
ccgp --logs -f                     # follow
ccgp --restart                     # after an update: a new server from this binary
ccgp --stop
ccgp --serve -s                    # foreground, one line per request (stop with Ctrl+C)
```

### Reading the log

Every request has one line: start time, status, the time until the backend's reply headers, the route and, for
failures, the error and the connection it was on (`| conn reused (idle …)`). A request to Codex or to an
Anthropic-style provider (z.ai) that had to open a connection of its own ends with `| conn new (tls …, pool …)`;
over an open connection it has no such mark, so the share of marked lines shows whether connections are kept
(`upstream stats` below counts them too). A successful `GET …/model` or `/health`
has no line (Claude Code asks every minute); a failed one has. Lines carry the time only, so a line
`--- 2026-11-02 ---` opens the log of a server and every new day; a line is placed by the day its request began (one
that began before midnight and is written after it stays above the new line). A streamed Codex answer that goes wrong or is slow
gets a second line. It is written when the answer ends but stamped with the start of the call, like the request's own
line, so it can stand far below that line; its timing ends with `total 15m1.327s, ended 10:45:19`, the time it ended:

- `stream failed after …`: the stream broke after it started (`after` is the time since the start of the call, the
  same as `total`). The reason, then the raw backend event that ended it
  (`upstream event: {…}`, for `error` / `response.failed`), the backend's `x-request-id` / `cf-ray` and the timing.
  `not retried: content already sent (thinking)` names the kinds of block the client had got (`thinking`, `text`,
  `tool_use`): after thinking alone Claude Code asks again by itself (twice), after text or a tool call it keeps what
  came and shows `Server error mid-response`; ccgp cannot start the request over in either case.
- `slow stream …`: the reply headers took over 20 s or the answer over 2 minutes. The timing only.
- `stream canceled by client after …`: Claude Code left the stream (Esc, a newer request); the timing only.
- `upstream stats [client]: …`: every 10 minutes, for each upstream client that did something: connections opened
  (`dials 3 (h2 2, http/1.1 1, 2 resumed)`: how many, the protocol they agreed on and how many resumed the TLS session of an earlier connection, which makes the handshake short), background warm-ups (`opened`, `used`,
  `failed`) and retries (after a connection could not open, after one broke). A warm-up count of `used` far below
  `opened`, or `http/1.1` where `h2` is expected, shows a connection problem before it becomes errors.
- `upstream rate limit: retrying in …`: the backend answered 429 with a `Retry-After` of up to 5 s, so ccgp waited it
  out and asked once more. The line lists the limit headers (`retry-after`, `x-ratelimit-*`, `*reset*`, `x-codex-*`);
  a longer `Retry-After` is passed on to Claude Code in the 429 reply (header included) and the limits go to the log
  line of the 429.
- `upstream retry N/M in …`: the stream failed before ccgp had sent any content to Claude Code, so ccgp sends the
  request again after a pause and Claude Code just waits longer. Two kinds, each with its own budget: the backend
  said it is overloaded (`server_is_overloaded` and similar; pauses of 2 s, then 5 s), or the connection broke
  (`stream read error: unexpected EOF`, `http2: client connection lost`, a stream that ends early; pauses of 0.3 s,
  then 2 s). Only when the retries run out does the stream fail. A stream that already sent content is never
  started over, and neither is one the client canceled. A third kind comes from the edge in front of the backend:
  a gateway error (502, 503, 504) or a 403 with an HTML page (`html error page: Unable to load site … If you are
  using a VPN, try turning it off. … [IP: …]`: the edge turned this connection's route away, while other connections
  of the same machine may pass). Same pauses; after a page ccgp drops the connection's pool, so the retry opens a
  new connection, and the page's `[IP: …]` (the address the edge saw) stays in the line. A 403 with JSON is the
  backend's own answer and is passed on.
- `upstream reconnect N after …`: the connection of an attempt failed (could not open, or broke before the reply) and
  ccgp sent the request over another one. One line per retry, with the error and the kind of connection the failed
  try was on; the client sees nothing unless the retries run out. Lines with the same error in a row, or many
  `reused, idle …` ones, point at the network or a program on this machine, not at the backend.
- `server pid N stopped by …`: written by `ccgp --stop`, `ccgp --restart` or a `ccgp <profile>` launch that replaced
  an older server, with the pid of the command that did it and of its parent. A server that is gone without such a
  line was not stopped by ccgp.
- `previous server pid N (started …, last alive …) ended without a stop by ccgp or a clean exit`: the first line of
  the next server that finds the record of one that vanished (see below).

#### A server that vanished

A running server keeps a record of itself next to the log: `server.run` (pid, start time, the process that started it,
and the Windows job it lives in), touched every 15 s. A stop by ccgp or a clean exit removes it. The next server that
finds it left behind says so in its first log line: since when the previous one was alive and whether it left a crash
report. A panic or fatal error is printed to the log and, with the stacks of all goroutines, into `crash/` next to it
(`crash-<time>-pid<N>.txt`, the newest 20 are kept). No crash report means it was ended from outside: killed by a
program or a user, or its terminal or job closed. On Windows the banner then shows `in a job: …`: a job that kills its
processes when it closes (`wnice`, `capc`) takes a server started inside it along when the terminal that owns it goes.

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
| `--profile add\|list\|rm` | `add NAME --like P` (own login), `--provider P` (existing login), `--type codex\|codex_api\|anthropic_compat\|claude` (new backend; `claude` = the real Claude Code, own clean directory and sign-in), `list [--json]`, `rm NAME [--yes] [--purge]`; see [configuration](configuration.md#creating-and-removing-profiles) |
| `--models [apply] [-v] [profile]` | Show or write the model mapping |
| `--config reset` | Back up the config and write the default one |
| `--connections [active N] [total N] [proxy URL|off [profile NAME]] [--json]` | Show or set the connections per backend (default 7 active, 10 total) and the proxy, global or of one profile (`none` = direct, `inherit` = the global one again); see [configuration](configuration.md#layout) |

```sh
ccgp --login codex                       # sign in (a link; --device for a code)
ccgp --login zai                         # an API key, asked once
ccgp --logout codex --yes                # forget the login (the config stays)
ccgp --profile add codex2 --like codex   # a second account of the same kind, then: ccgp codex2
ccgp --profile add work --type claude    # a real Claude Code profile with its own account
ccgp --profile list --json
ccgp --models                            # what the backend offers and the tier mapping
ccgp --models apply codex                # write that mapping into the config
ccgp --which codex claude-opus-4-1       # where a model name goes, offline
ccgp --connections                       # show the limits and the proxy
ccgp --connections proxy socks5://127.0.0.1:1080
ccgp --config reset                      # default config, the old one is kept as a backup
```

## Sessions

`--send <from> <to> [count]`, `--import <profile> [count]`, `--export <profile> [count]`: see
[sessions](sessions.md).

```sh
cd ~/projects/app
ccgp --import codex          # your Claude Code's latest sessions of this directory -> profile codex
ccgp codex -c                # carry on
ccgp --send codex zai 3      # three latest sessions between two profiles
ccgp --export zai            # profile zai -> your own Claude Code
```

## Updating

`ccgp --update [--check]` looks up the latest release (`/releases/latest` redirect, no API rate limit), downloads the
archive for this OS and CPU and `SHA256SUMS`, verifies the checksum before touching anything, and replaces `ccgp`
(and `ccgpy`, if it sits next to it) in the directory of the running binary. On Windows the running file is renamed
to `.old` first. Config and credentials are not touched; `--check` only reports whether an update exists. When a
server is running, restart it afterwards with `ccgp --restart`.

Installed with npm, update with npm instead (`npm install -g @cc-go-proxy/ccgp@latest`): `--update` would replace the binary
inside the npm package, and the next npm install would put the packaged version back.

## Environment variables

| Variable | Used by | Meaning |
| --- | --- | --- |
| `CCGP_HOME` | all | A home directory for config, logins, tokens, Claude dirs and log ([own home](#own-home-a-setup-that-shares-nothing)) |
| `CCGP_CONFIG` | all | Another config file |
| `HOST`, `PORT` | server | Listen address; win over `listen` in the config |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | all outbound connections | Used when no `connections.proxy` / profile `proxy` is set |
| `CLAUDE_CONFIG_DIR` | profile runs | Your own Claude Code directory, the read-only source of the mirrored settings (a profile's own directory is set by ccgp) |
| `ANTHROPIC_MODEL` | profile runs | The session's main model, for the compaction window |
| `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` | profile runs | Set by ccgp from the profile; yours win |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL`, `PROXY_TOKEN`, `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` | env-only mode | See [configuration](configuration.md#env-only-mode) |
| `CCGP_VERSION`, `CCGP_INSTALL_DIR` | `install.sh`, `install.ps1` | Version and directory to install |

`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_OAUTH_TOKEN` and the Bedrock / Vertex /
Foundry switches of the parent are removed before `claude` starts, for proxy and `claude` profiles alike.

## Exit codes

| Code | When |
| --- | --- |
| `0` | success |
| `1` | an error (printed as `ccgp: …`); `--doctor` found a failing check |
| `3` | `--status`: the server is not running (the status is still printed) |
| anything | a profile run returns the exit code of `claude` |

## The HTTP API

The server's endpoints (`/health`, `/call/<token>/…`) are described in [api.md](api.md).

## Shell completion

```sh
source <(ccgp --completion bash)        # bash
source <(ccgp --completion zsh)         # zsh
ccgp --completion powershell | Out-String | Invoke-Expression   # PowerShell
```

Add the line to your shell profile to keep it. Commands, subcommands, flags and the names of your profiles and
providers are completed (`ccgpy` completes profile names). The scripts only call the hidden `ccgp --complete`; the
candidates are computed by ccgp from its command table and your config, and a broken config just means no names.
