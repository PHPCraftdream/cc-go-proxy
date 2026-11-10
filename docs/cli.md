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

A profile of type `claude_api` (`ccgp --profile add api --type claude_api`) is the same with an API key of the Claude
Platform instead of a login: `ccgp --login api` saves the key (the first start asks when there is none) and `ccgp api`
runs `claude` with it as `ANTHROPIC_API_KEY`, against the official server unless the provider has a `base_url`
([configuration](configuration.md#profiles-with-an-api-key---type-claude_api)).

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
failures, the error and the connection it was on (`| conn reused (idle …)`). A refusal by an Anthropic-style provider
(z.ai) gives the reason the provider stated, its error type or code and message (`429 … glm-5.3-flash: 1302: Rate limit
reached for requests | retry-after=30 | x-request-id=…`), with the limit headers for a 429 or 503 and the request id; keys
and tokens a message echoes are hidden (`sk-***`), and Claude Code gets the reply unchanged. A request to Codex or to an
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

  These three lines end with `| prompt key a1b2c3d4, system 5e4d3c2b, 63 tools 9f8e7d6c, 214 input items; …`: a
  fingerprint of what the backend caches by prefix. The full cache key is the identity (only its first digits are shown);
  it comes from `metadata.user_id`, so subagents with the same value share a key and comparisons. The line shows a hash of
  the system prompt and one of the tool definitions, and the number of input items; it logs no prompt text or session id.
  Model, reasoning settings and tool choice are retained for comparisons but are not rendered as hashes (a changed model's
  old/new names or a reasoning/tool-choice change note may appear). It also says how the request differs from the last
  request of this key since the server started: `same system prompt
  and tools as the last request of this key`, `system prompt changed …`, `tools changed …`, `model changed OLD -> NEW`,
  `reasoning changed`, `tool choice changed`, `history shorter (214 -> 40 items)`, or `first request of this key`. The
  history is compared item by item (a hash of each, kept per key): `history extends the last request by 2 items` (the
  prefix is the same), `history changed at item 7 of 214 (the last request had 212)` (item 0 is the system prompt; the
  items before the changed one can still be read from the cache), `history identical to the last request`. For Codex, prompt
  notes say `new turn`, `turn state sent`, and `turn state returned` (the backend gives the token with the first reply of
  a turn) or `no turn state returned` (only when no state was in hand and none came back; a step of a tool loop that sent
  one and got none back is normal and says nothing); the opaque token is never shown. Identical history alone does not prove the backend caused a miss; model or request controls may differ, and even
  with matching settings the backend may not have read its cache. When the reply of the last request had ended, the note adds `the last
  request had 711757 tokens in`: a `cached` below it means the backend dropped part of what it had cached. Concurrent
  calls report `2 other requests of this key running`; if history diverges while the last request is still running, the
  line marks `branch: history diverges from the last request while it is running`. Next to `| cache miss` (a prompt of 10000
  tokens or more with nothing cached) or `| cache partial 64% (469888 of 732972)` (under 90% cached) it tells a miss that
  has a cause (the prefix changed, a compaction, a subagent with a prompt of its own) from one the backend alone explains
  (same prefix and settings, part or all of it not read from the cache). The note ends with `edge LAX -> PIT` (or `edge same (LAX)`):
  the data centre in the `cf-ray` of the key's last reply and of this one, to see whether a cache that is lost goes with a
  change of the way in. Before it comes `the last request started 8m19s ago, ended 12s ago` (or `… and had not ended`
  when another request of the key was still running): the backend keeps a cache entry for a few minutes, so a long stream
  before this request may be why its cache is gone.
- `upstream cache [codex]: 41 big prompts, 12 read from the cache in part (under 90%), 1 not at all; 94% of 23.4M input
  tokens came from the cache`: every 10 minutes, next to the stats line, over all Codex replies of 10000 input tokens or
  more (not only the slow ones); nothing is printed in a quiet period. The next line, `upstream cache by [codex]:
  started <5m before: 30 prompts, 96% cached; started >10m before: 6 prompts, 41% cached; same edge: …; other edge: …`,
  splits the same prompts by how long before the last request of their key started and by whether its reply came through
  the same data centre; it shows which of the two goes with a lost cache. It also splits prompts by whether turn state
  was sent (`turn state sent` / `turn state not sent`).
- `upstream link [codex]: 41 replies; 9 new connections, TLS median 0.9s, max 2.7s; upload of 38 big bodies 612 KiB/s
  overall, slowest 90 KiB/s; 5 broken calls (3 sent again by ccgp, 2 reached the client): 3 connect timeout, 1 connection
  lost, 1 silence (health-ping close)`: every 10 minutes, next to the stats line, how the link to the backend behaved in
  the window (nothing is printed in a quiet one). Bodies of 256 KiB or more count for the upload speed. Causes: `connect
  timeout` (TLS handshake or no connection), `connection lost`, `silence (health-ping close)` (about 45 s without a
  byte), `no reply in time`, `reset`, `websocket ended by the backend`, `other`; a stream that broke and was sent
  again counts too; a call the client left is not counted. It tells a bad stretch of the
  network (slow handshakes and uploads, breaks everywhere) from a slow model (none of them).
- `upstream websocket [codex]: the backend ends connections after about 30m1s (2 breaks agree, close status 1001);
  replacing them after 27m1s`: once, when ccgp has learned from two breaks at the same age when the backend ends
  connections (see [API behavior](api.md#codex-websocket-transport)); from then on connections are replaced ahead of it.
- `upstream websocket [codex]: 12 delta, 3 full, 1 busy, 0 fallbacks, 2 dials; sent 41 KiB; 1 connection ended by the
  backend or the network; 2 connections replaced ahead of the backend's limit`: every 10 minutes, unless
  `CODEX_TRANSPORT=http` (see [API behavior](api.md#codex-websocket-transport)), and only when something happened:
  requests sent as a delta (`previous_response_id` plus the new items) or in full, requests that found the
  conversation's connection busy, deltas refused for a changed history, connections opened, and the bytes sent. A request's
  summary then carries `transport websocket (delta: sent 3 KiB of 4.7 MiB)`, `transport websocket (full: new connection,
  sent 4.7 MiB)` or `fallback to HTTP (<reason>)`. A full request names why: `new connection`, `connection broken`,
  `replaced ahead of the backend's 30m limit`, `history diverged at item 412 of 1079` (the item that differs, of the items
  the backend holds: a deep one is a rewritten history, one near the end a branch), `tool call changed (arguments,
  Read)` (what differs and in which tool), `assistant text changed`, `reasoning dropped`.
- `upstream speed [codex]: first token / whole reply, median: http 2.1s / 14s (40 replies), websocket full 2.3s / 15s
  (3 replies), websocket delta 1.2s / 11s (12 replies)`: every 10 minutes, when Codex replies completed: the median time
  to the first token and to the end of the reply, per transport. Medians over different prompts: a trend to read over
  hours, not a benchmark.
- A slow, failed or cancelled stream's line has `time connect 300ms, upload 21.7s, headers wait 8s, first token 2s,
  stream 8s (mostly upload on the link, 54%)`: where its time went. `connect` and `upload` (and the time before the
  last try) are the link's, the rest the backend's.
- `upstream stats [client]: …`: every 10 minutes, for each upstream client that did something: connections opened
  (`dials 3 (h2 2, http/1.1 1, 2 resumed)`: how many, the protocol they agreed on and how many resumed the TLS session of an earlier connection, which makes the handshake short), background warm-ups (`opened`, `used`,
  `failed`) and retries (after a connection could not open, after one broke). A warm-up count of `used` far below
  `opened`, or `http/1.1` where `h2` is expected, shows a connection problem before it becomes errors.
- `upstream rate limit: retrying in …`: the backend answered 429 with a `Retry-After` of up to 5 s, so ccgp waited it
  out and asked once more. The line lists the limit headers (`retry-after`, `x-ratelimit-*`, `*reset*`, and other
  rate-limit `x-codex-*` headers); it explicitly excludes the opaque `x-codex-turn-state`. A longer `Retry-After` is passed
  on to Claude Code in the 429 reply (header included) and the limits go to the log line of the 429.
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

`--status`, `--profile list`, `--models` (the read-only form), `--which`, `--connections`, `--peak` and `--doctor` accept `--json`: one
pretty-printed document on stdout. Field names are lowerCamelCase; URL tokens, API keys and OAuth tokens never appear.
Without the flag the text output is unchanged. `--status` still exits with code 3 when the server is not running,
after printing the JSON.

## Logins and profiles

| Command | |
| --- | --- |
| `--login <provider> [--device]` | Sign in (Codex: browser OAuth, or a device code; others, `claude_api` included: API key) |
| `--logout <provider> [--yes]` | Remove the stored OAuth tokens or API key of a provider after a confirmation. The config file is not touched, and `--yes` is required without a terminal |
| `--profile add\|list\|rm\|set\|unset` | `add NAME --like P` (own login), `--provider P` (existing login), `--type codex\|codex_api\|anthropic_compat\|claude\|claude_api` (new backend; `claude` = the real Claude Code, own clean directory and sign-in; `claude_api` = the same with an API key of the Claude Platform, `--base-url` for another server), `list [--json]`, `rm NAME [--yes] [--purge]`, `set NAME KEY VALUE` / `unset NAME KEY` (`default_model`, `context_window`, `compact_percent`, and `provider` to move the profile; `unset` not for `provider`); see [configuration](configuration.md#creating-and-removing-profiles) |
| `--provider [NAME] [--json] \| set\|unset NAME KEY [VALUE]` | The providers (type, peak hours, profiles), one provider in detail (base_url, api_key_env, login, proxy and where it comes from, effort map, peak hours), or set / remove its `base_url` or `api_key_env`. The proxy is `--connections proxy … provider NAME`, the peak hours `--peak` |
| `--models [apply] [-v] [profile]` | Show or write the model mapping |
| `--config reset` | Back up the config and write the default one |
| `--connections [active N] [total N] [proxy URL|off [provider NAME \| profile NAME]] [--json]` | Show or set the connections per backend (default 7 active, 10 total) and the proxy: global, of one provider or of one profile; each level replaces the ones above it (`none` = direct, `inherit` = the level above again); see [configuration](configuration.md#layout) |
| `--interactive`, `-i` | Full-screen interface to view, edit and add profiles, providers and peak hours; needs a terminal; see [interactive mode](#interactive-mode) |
| `--peak [provider [set\|add WINDOW...\|rm N...\|tz ZONE\|clear\|pause [LEN]\|resume]] [--json]` | Show or set the peak hours of a provider, when the requests of its profiles are refused; see [peak hours](peak-hours.md) |

```sh
ccgp --login codex                       # sign in (a link; --device for a code)
ccgp --login zai                         # an API key, asked once
ccgp --logout codex --yes                # forget the login (the config stays)
ccgp --profile add codex2 --like codex   # a second account of the same kind, then: ccgp codex2
ccgp --profile add work --type claude    # a real Claude Code profile with its own account
ccgp --profile add api --type claude_api # ... with an API key of the Claude Platform (ccgp --login api)
ccgp --profile list --json
ccgp --profile set zai default_model glm-5.3-flash   # one setting of a profile ...
ccgp --profile set zai2 provider codex               # ... also moving it to another provider
ccgp --profile unset zai compact_percent
ccgp --provider                          # the providers
ccgp --provider zai                      # one in detail
ccgp --provider set zai base_url https://api.z.ai/api/anthropic
ccgp --provider unset myai api_key_env
ccgp --models                            # what the backend offers and the tier mapping
ccgp --models apply codex                # write that mapping into the config
ccgp --which codex claude-opus-4-1       # where a model name goes, offline
ccgp --connections                       # show the limits and the proxy
ccgp --connections proxy socks5://127.0.0.1:1080                # everybody
ccgp --connections proxy http://corp:3128 provider codex        # all profiles of codex: replaces the global one
ccgp --connections proxy none profile home                      # one profile, direct: replaces both
ccgp --connections proxy inherit profile home                   # remove it: the level above applies again
ccgp --connections --json                                       # limits, proxies, and the level each profile's comes from
ccgp --peak                              # the peak hours of every provider and whether it is peak now
ccgp --peak zai set mon-fri 14:00-18:00  # refuse requests in them ...
ccgp --peak zai tz Asia/Shanghai         # ... the hours are given in this zone
ccgp --peak zai pause 2h                 # work anyway for two hours (resume ends it)
ccgp --config reset                      # default config, the old one is kept as a backup
```

## Interactive mode

`ccgp --interactive` (or `ccgp -i`) opens a full-screen interface, built on [Bubble Tea](https://github.com/charmbracelet/bubbletea),
to do everything the commands do: look at, change and add profiles, providers and peak hours, set the proxy and the
connections, map models, sign in and out, move sessions, run the server and its checks. **Only the flag starts it**: a word without dashes is
still a profile, and `ccgp` alone still prints the help. It needs a terminal (it draws menus and reads keys); for
scripts use the commands.

```
 ccgp
 /home/you/.config/cc-go-proxy/config.ktav

 › 1 Profiles (3)
   2 Providers (3)
   3 Peak hours
   4 Config and connections
   5 Models
   6 Sessions
   7 Server
   8 Quit

 ↑/↓ move · enter or number select · esc back · ctrl+c quit
```

| Keys | |
| --- | --- |
| `↑` `↓` (or `k` `j`), `enter` | Move, select. A number (1-9) selects that item at once; some items have a letter (`a` add, `r` remove, `p` peak hours) |
| `esc`, `←`, `q`, `backspace` | Go back; from the first screen, quit. `ctrl+c` quits from anywhere |
| in a field | `enter` accepts, `esc` cancels, `←` `→` `home` `end` move, `ctrl+u` clears, `ctrl+k` clears to the end, `ctrl+w` deletes a word |

A field starts with the current value; emptying it removes the setting. A value the loader refuses (a compact percent
of 150, a `base_url` removed from an `anthropic_compat` provider, a wrong window) is shown under the field and nothing
is written.

| Screen | What it does |
| --- | --- |
| **Profiles** | A list with provider, type and login state. Open one to see its provider, login, proxy (and the level it comes from: its own, its provider's, the global one) and model rules, and to set `default_model`, `context_window`, `compact_percent` and `proxy`, move it to another provider, or remove it: with its Claude dir and the logins nothing else uses kept, or deleted (`--purge`). A profile of the real Claude Code (`type: claude`) can only be removed: it has no settings of ccgp's; one with an API key (`type: claude_api`) can also set its server (`base_url`) and log in to save the key |
| **Profiles → add** | Three ways, as `ccgp --profile add`: *another account of the kind a profile has* (`--like`: its own login, copies the settings and the peak hours), *another profile on a login that exists* (`--provider`), *a new backend* (`--type`: `codex`, `codex_api`, `anthropic_compat`, `claude`, `claude_api`; the last asks for the server, empty = the official one). It asks for the name (a free one is suggested) and, except for a Claude profile, for a catch-all model and its context window |
| **Providers** | A list with type, profiles and a mark where there are peak hours. Open one to see `base_url`, `api_key_env`, the proxy, the login, the effort map, the peak hours (and whether it is peak now) and the profiles that use it; set `base_url`, `api_key_env` and `proxy` (for all its profiles; a profile's own wins); open its **peak hours** |
| **Providers → add** | A new backend with its first profile (a provider cannot exist without a profile). A provider goes when its last profile is removed |
| **Providers → a provider** (more) | Also: **log in**, **log in with a device code** (Codex) and **log out** (`--login`, `--login --device`, `--logout`; a sign-in gets the terminal while it runs) |
| **Peak hours** | Every provider with its windows and whether it is peak now. Open one: add windows, replace them all, remove one, set the time zone, pause (a length, or until the running peak ends), resume, clear — `ccgp --peak <provider>` as menus ([peak hours](peak-hours.md)) |
| **Config and connections** | `connections.active`, `connections.total`, the global proxy (`--connections`), and *reset to the default* (`--config reset`, after a question) |
| **Models** | Show the mapping of every profile or of one (`--models`), write the newest models into them (`--models apply`, after a question), and *where does a model name go?* (`--which`) |
| **Sessions** | Copy the latest sessions of the current directory: *import* into a profile, *export* from one, *send* from one profile to another (`--import`, `--export`, `--send`), with an optional count; it asks what to do with a session that went on differently on both sides: leave it, keep both (`--fork`), or replace the old one (`--force`) |
| **Server** | Status, start, stop, restart, the log, the checks (`--doctor`), the version, a look for a newer release (`--update --check`) and the update itself (`--update`, after a question) |

Every screen reads `config.ktav` afresh, and every change is the edit the matching command makes: only the line
concerned changes (comments and line endings stay), the result is validated with the real loader, and a
`config.ktav.bak-<time>` backup is kept. A running server applies a change on its next request. The model rule lists
and `effort_map` are shown, not edited here: `ccgp --models apply` or the file. What a command does besides editing
(the server, sign-in, models, sessions, update) the interface does by running that command: its output is shown on a
screen of its own, and a sign-in or an update gets the terminal.

### The command line and the interface cover each other

Whatever a command can do the interface can, and whatever the interface offers a command offers, down to single
settings (`ccgp --profile set zai default_model glm-5.3`, `ccgp --provider set zai base_url …`). The only commands
the interface does not have are `--serve` (it runs the server in the foreground), `--completion` (a script for the
shell), `--help` (every screen shows its keys) and the flag that starts the interface itself. A test
(`cmd/ccgp/parity_test.go`) crawls every screen and every command and fails when either side has something the other
has not; for the edits it also runs the command and the keys on the same config and compares the files they leave.

It needs `config.ktav`; in the env-only mode (no config file, `PROXY_TOKEN` in the environment) it says so and exits.

## Sessions

`--send <from> <to> [count] [--fork|--force]`, `--import <profile> [count] [--fork|--force]`,
`--export <profile> [count] [--fork|--force]`: see [sessions](sessions.md). A session the destination already has and
that went on differently ("diverged") is skipped, unless `--fork` writes it as a new session next to the old one or
`--force` replaces the destination (the old file is kept as `.bak-<time>`).

```sh
cd ~/projects/app
ccgp --import codex          # your Claude Code's latest sessions of this directory -> profile codex
ccgp codex -c                # carry on
ccgp --send codex zai 3      # three latest sessions between two profiles
ccgp --export zai            # profile zai -> your own Claude Code
ccgp --import codex 1 --fork   # it diverged: keep both, then: ccgp codex --resume <the new id>
ccgp --import codex 1 --force  # it diverged: the source's version wins, the old file is kept
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
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | all outbound connections | Used when no `proxy` is set at any level (profile, provider, `connections.proxy`) |
| `CLAUDE_CONFIG_DIR` | profile runs | Your own Claude Code directory, the read-only source of the mirrored settings (a profile's own directory is set by ccgp) |
| `ANTHROPIC_MODEL` | profile runs | The session's main model, for the compaction window |
| `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` | profile runs | Set by ccgp from the profile; yours win |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL`, `PROXY_TOKEN`, `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` | env-only mode | See [configuration](configuration.md#env-only-mode) |
| `CODEX_TRANSPORT` | server | `auto` (default) or `websocket` (alias `ws`): Codex requests try the websocket, HTTP is the fallback; `http`: HTTP only. Read at start ([Codex websocket transport](api.md#codex-websocket-transport)) |
| `CCGP_VERSION`, `CCGP_INSTALL_DIR` | `install.sh`, `install.ps1` | Version and directory to install |

`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_OAUTH_TOKEN` and the Bedrock / Vertex /
Foundry switches of the parent are removed before `claude` starts, for proxy, `claude` and `claude_api` profiles alike
(a `claude_api` profile then gets its own key and server).

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
