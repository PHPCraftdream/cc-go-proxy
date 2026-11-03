# Changelog

All notable changes to ccgp are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

ccgp is a fork of [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy); its own
history is kept in [docs/upstream-changelog.md](docs/upstream-changelog.md).

## [Unreleased]

### Changed
- Codex model mapping follows the GPT-6.1 line-up: Sonnet goes to the newest `terra` (was the newest `luna`), Haiku
  to the newest `luna` (was the `luna` of the release before). Fable stays on `astra`, Opus on `sol`. Run
  `ccgp --models apply` to rewrite a profile's mapping.

- Documentation and help brought up to date: every command has its own `--help` with the flags and examples
  (`--which`, `--logs`, `--restart` and `--completion` were one-liners before; `--status`, `--models`, `--send`,
  `--profile`, `--login` say more), `docs/cli.md` gained running a profile, per-area examples, environment variables and
  exit codes, and the new `docs/api.md` describes the HTTP endpoints (`/health`, `/call/<token>/…`) with `curl`
  examples. A test keeps every command's help complete.

- Log: a successful `GET …/model` and `/health` no longer get a line (a third of the log was them); a failed one does.
  A line `--- 2026-11-02 ---` opens the log of a server and every new day (lines carry the time only). The line of a
  failed request (403, 429, 502, …) ends with the connection it was on (`conn new (dns …, tls …)`, `conn reused (idle …)`),
  and the address in an edge's block page (`[IP: …]`) is no longer cut off.

- Log: a request to z.ai (or another Anthropic-style provider) that opened a connection of its own ends with
  `| conn new (tls …, pool …)`, and a refused or failed one with the connection it was on, as for Codex. A line written
  just after midnight by a request that began before it stays above the new `--- date ---` line.

- Log: the lines `slow stream`, `stream failed` and `stream canceled by client` are stamped with the start of the
  call (they were stamped with the moment the reply headers came in, except the slow one), count `after` from the same
  start as `total`, and the timing ends with `ended HH:MM:SS`, the time the answer ended: such a line is written when
  the answer ends and could not be placed in time before.

### Added
- A server that vanishes leaves a trace. It keeps `server.run` next to the log (pid, start, parent, the Windows job it
  lives in; touched every 15 s); `ccgp --stop`, `--restart` and the restart of an outdated server by a launch write
  `server pid N stopped by …` into the log and clear it, a clean exit clears it too. The next server that finds the
  record says in its first line that the previous one `ended without a stop by ccgp or a clean exit`, since when it
  was alive, and whether it ran in a job that kills its processes when it closes. A panic or fatal error, besides the
  log, goes with the stacks of all goroutines into `crash/crash-<time>-pid<N>.txt` (the newest 20 are kept). On
  3 November a server stopped at 08:10 with nothing in the log to say why.
- Profiles of the real Claude Code: `ccgp --profile add work --type claude` makes a profile (provider type `claude`)
  that does not use the proxy at all. `ccgp work` runs `claude` with a config directory of its own, created clean:
  only `CLAUDE.md`, `settings.json` (without credential entries), agents, commands, skills, output styles and plugins
  are mirrored from your own Claude Code; no `.claude.json` (account, `userID`, projects, MCP servers), no sessions, no
  history, no token. Your parent's `ANTHROPIC_*` / `CLAUDE_CODE_OAUTH_TOKEN` / Bedrock / Vertex variables are dropped.
  You sign in with `/login` inside it, so every profile can be a different account, and as many as you like. See
  `docs/configuration.md`.
- A setup of its own: `ccgp --local` keeps the config, logins, URL tokens, Claude Code dirs and the server log in a
  `.ccgp` directory in the current directory, `ccgp --home <dir>` (or `CCGP_HOME`) in any directory. The flags come
  first (`ccgp --local --login codex`, `ccgpy --home ~/work/ccgp work`); a `.ccgp` directory that already exists in the
  current directory is picked up without a flag. A new home gets a free port of its own and so its own server, and a
  `.gitignore` that keeps its secrets out of git. `ccgp --status` shows the `home:` line. See `docs/cli.md`.
- Codex request bodies of 32 KiB and more are sent zstd-compressed (`Content-Encoding: zstd`, as the official Codex
  client does): a long conversation is several MiB of JSON, and sending 6 MiB took 12-17 s on a slow uplink, more
  than half of the wait for the first reply. A backend that answers a compressed body with 400/415 gets the same
  request again plain, and when that is accepted the compression is off for the rest of the run (the log says so once). The log line of a
  slow or failed stream shows the compressed size: `request 6.2 MiB (zstd 1.1 MiB, written in 2.1s)`.
- A 502, 503 or 504 from the gateway in front of the Codex backend (`upstream connect error …`, a Cloudflare page)
  is asked again up to twice (0.3 s, 2 s) before Claude Code sees it; the log shows `upstream retry`. The log of an
  HTML error page now says `html error page: <title>` instead of a screen of markup.
- The request line of a Codex request that opened its own connection ends with `| conn new (tls …, pool …)`, so the
  share of such requests can be counted (warm and reused connections stay unmarked).
- The timing of a slow or failed stream shows how long the request took to leave (`request 5.4 MiB (written in 3.2s)`),
  so a slow upload can be told from a slow backend.
- A 502 `http2: client connection lost` that came after about one health-ping limit (45 s) of silence says so in the
  log: Go does not report that its ping closed the connection, the time is the only sign.
- A Codex call that fails before any reply (a 502 such as `timeout awaiting response headers`) and every
  `upstream reconnect` line now say how far the request got: `request 4.8 MiB (zstd 2.2 MiB): written in 5.4s, no reply
  for 44.6s`, `not written out, …` or `no connection after …`. A stuck upload and a silent backend look different.
- The token part of a call's timing ends with `| cache miss` when a prompt of 10000 tokens or more was not read from the
  backend's cache at all: such a request waits much longer for its first token.
- A `stream failed` line says `| not retried: content already sent (thinking, text)` when the client had got part of
  the answer, and which kinds of block: ccgp cannot start such a request over; Claude Code asks again by itself after
  thinking alone, and keeps what came after text or a tool call.
- A `stream failed` line says `silent for X`: how long the reply had been quiet before it broke. About 45 s means ccgp's
  health ping closed a connection the network no longer carried (the line says so); data right up to the end means a
  reset by the backend or the network.

### Fixed

- The interface options that only `~/.claude.json` holds did not reach the profiles: `leftArrowOpensAgents: false`
  (the `←` key that backgrounds the session and opens agent view) was set in your own Claude Code, and in a profile
  `←` still opened it. Only the theme, editor mode, verbose, MCP servers and the onboarding flag were copied, once, when
  the profile was created. Now `leftArrowOpensAgents`, `defaultToAgentsView`, `copyOnSelect`, `copyFullResponse`,
  `diffTool`, `externalEditorContext`, `prStatusFooterEnabled`, `autoConnectIde` and `autoInstallIdeExtension` are copied
  too, and a profile made earlier gets the ones it lacks at its next launch (what the profile has is its own and stays).
  Profiles of the real Claude Code (`--type claude`) still start without a `.claude.json`.

- Requests to z.ai never reused a connection: z.ai closes an HTTP/2 connection that was quiet for 30 seconds, and the
  health ping came at exactly 30, so every request (a few a minute, spread over the lanes) opened its own, 0.17 s or
  more of a 0.35 s round trip. The ping now comes after 15 seconds of quiet (a ping counts as traffic for z.ai, which
  answers it); the answer limit went from 15 to 30 seconds, so a dead connection is still dropped after at most 45 s of
  silence. The 10-minute `upstream stats [anthropic]` line shows the effect: `dials` falls from about one per request
  to the warm-ups of the renewed pools.

- An HTML page with a 403 from the edge in front of the Codex backend (Cloudflare's "Unable to load site … If you are
  using a VPN …": the route of one connection is blocked, others pass) is no longer handed to Claude Code at once: the
  request is asked again, up to twice (0.3 s, 2 s), after the pool of that connection is dropped, so the retry opens
  a new one. A 403 with JSON, the backend's own, is passed on as before. On 2 November 12 of 45 requests in 11 minutes
  got that page.
- `ccgp <profile> -c` said `No conversation found to continue` in a directory where the profile had no session yet but
  had sessions of other directories: the copy of the latest session from your Claude Code at launch ran only for a
  profile with no session at all. It now runs for a profile that has no session of the current directory.
- A refused Codex request (any non-2xx status) now logs the backend ids (`x-request-id`, `cf-ray`) and the edge verdict
  (`cf-mitigated`), and an HTML error page without a title shows the start of its text instead of a bare
  `html error page`: a 403 from the Cloudflare edge can be told from a challenge or a block.
- A Codex request without streaming (`"stream": false`) is asked again after an overloaded backend or a broken
  connection before any content, as a stream is. It returned a 502 at once before. Its failure and slow reply are
  logged like a stream's (`stream failed after …`, `slow stream …`).
- Requests no longer fail together when the backend (or a proxy on the way) cuts a connection after a fixed lifetime:
  every 30 minutes all streams to `chatgpt.com` ended in `unexpected EOF` and new requests got a 502. Upstream
  connections are now renewed every 10 minutes without interrupting running requests, and a request whose
  connection breaks before the reply is sent again.
- HTTP/2 connections to the backends send a health ping when they have been quiet for 15 seconds and are closed when
  it is not answered within 30 (shorter limits cut streams that only waited for a slow backend): a connection that a proxy program or an antivirus killed silently while it was idle no
  longer fails the next request (it was one retry each time: `wsasend: An established connection was aborted by the
  software in your host machine`). The log of a retried request says what kind of connection its first try was on.
- A 429 from the Codex backend with a `Retry-After` of up to 5 seconds is waited out once and the request is sent
  again; a longer one reaches Claude Code with its `Retry-After` header, and the log line of a 429 lists the limit
  headers (`retry-after`, `x-ratelimit-*`, `*reset*`).
- A request no longer fails when a new connection cannot be opened in time (`TLS handshake timeout`, a refused or
  unreachable dial): it goes out over another connection that works, and ccgp keeps trying with growing pauses for up
  to 30 s before it gives the 502; a connection that broke before the reply is retried up to twice. Before, there was
  one retry. A replaced connection is opened in the background before it takes requests,
  so no request waits for a handshake.
- A pool no longer closes a connection for being idle for 90 s (the default transport's limit also applied to
  HTTP/2): with a few requests a minute over several lanes up to every second request had opened a new connection and
  paid ~0.8 s for the TLS handshake. Pools are still renewed every 10 minutes and dead connections dropped by the ping.
- A connection that broke before the reply is retried even when the request had already waited longer than the 30 s
  connect budget (a health-ping close after 45 s of silence used to reach Claude Code as a 502).
- When the Codex backend answers `server_is_overloaded` before sending any content, ccgp asks again after 2 s and
  then 5 s instead of passing the error on; the log shows `upstream retry`.
- Streams the client left are logged as `stream canceled by client`, apart from real failures.
- The timing in the log shows the real TLS handshake time of new connections (it read `tls 0s`), marks connections
  opened in the background as `conn warm (opened … before)` and names the error a request was retried after.
- The launch of a profile that has no session of the current directory copies the latest session of this directory from your
  Claude Code (like `ccgp --import <profile> 1`), so `/resume` and `-c` can continue the work at once.
- The binary runs everywhere it can: release archives and `install.sh` now also cover Linux 32-bit ARM (built for
  armv6, so it runs on a Raspberry Pi 1 and later), Linux riscv64 and FreeBSD (x86-64, arm64), besides the six
  platforms before; the npm package stays at those six. The Linux builds are static (no libc: glibc and musl/Alpine
  alike) and carry a fallback set of root certificates for systems that have none, such as minimal containers (used
  only when the system store is empty). A CI job checks that the Linux binaries are static and runs them on Alpine,
  Debian 11, Ubuntu 20.04, busybox (HTTPS without a certificate store), arm64 and ARM.
- A Codex stream whose connection breaks before ccgp has sent any content to Claude Code (`unexpected EOF`,
  `http2: client connection lost`, a stream that ends early) is sent again, up to twice after 0.3 s and 2 s, instead
  of failing; the log shows `upstream retry N/2` with the reason. Streams that already sent content, and canceled
  ones, are never retried.
- New TLS connections resume the sessions of earlier ones (a short handshake instead of the full one, which was
  taking 1 to 4 s on a bad network); `upstream stats` shows how many resumed.
- A connection opened in the background no longer waits minutes for its first request: the lane switches to it as
  soon as it is open, the next request on equally busy lanes prefers it, and one that has waited over 20 s is
  closed and replaced by a fresh dial. Idle bare-TLS connections were being aborted on the way ("An established
  connection was aborted by the software in your host machine"), costing a retry each time. A lane removed by a
  config reload while its warm-up ran could be switched to the new pool; it no longer is.
- Every retry after a connection failure gets its own log line, `upstream reconnect N after …`, with the error and
  the kind of connection the failed try was on; before, a request saved by a retry showed only in the 10-minute
  counters. The timing also shows the age of the connection's pool (`pool 8m12s`), so a dropped connection can be
  told from a young or an old one.
- A Codex stream that the backend ends with an error event is logged with the backend's code and message, the raw
  event and the `x-request-id` / `cf-ray` of the response; before, such failures read only `upstream error`. The
  code and message of an `error` object inside the event also reach Claude Code now.
- Parallel requests no longer share one connection: they are spread over 7 connections per backend, so one
  busy connection does not slow down the others.

### Added
- `connections.proxy` in config.ktav and `ccgp --connections proxy URL|off`: all outbound connections (model
  requests, model catalog, login and token refresh, `--doctor`, `--update`) can go through a SOCKS5 (`socks5://`,
  `socks5h://`, optional user and password) or HTTP/HTTPS `CONNECT` proxy. The new connections are warmed up through
  it too; a change applies without a restart, and the password is shown as `***`.
- Proxy settings cascade: a profile's own `proxy:` (a proxy URL, or `none` for no proxy at all) overrides the global
  `connections.proxy`; a profile without one inherits it. `ccgp --connections proxy URL|none|inherit profile NAME`
  edits it. Every way out keeps its own connections; login, the model catalog and the `--doctor` probe use the proxy
  their provider's profiles agree on.
- Every 10 minutes the server logs `upstream stats` per upstream client: connections opened and their protocol
  (h2 or http/1.1), warm-ups opened/used/failed, retries.
- A slow Codex answer (reply headers after 20 s or the whole answer over 2 minutes) gets a `slow stream` log line
  with the timing: new or reused connection (DNS, connect, TLS), time to headers, to the first token and in total,
  request size and tokens. Broken streams carry the same timing. See
  [docs/cli.md](docs/cli.md#reading-the-log).
- `connections.active` / `connections.total` in config.ktav (default 7 / 10) and `ccgp --connections [active N]
  [total N] [--json]` to show and set them: connections taking new requests, and the limit for them plus the ones
  still finishing older requests. Applied by a running server without a restart.

## [0.1.0] - 2026-10-25

First release of ccgp as an independent project.

### Added
- `ccgp <profile>` launcher: starts the background server, makes sure credentials exist, runs Claude Code through
  the profile. Named profiles, each with its own URL token and its own provider. A background server started from an
  older binary is restarted when it is idle; with requests in flight ccgp warns and leaves it running.
- Command line: every command starts with `--` (`ccgp --status`, `--login`, `--models`, `--profile`, `--send`, ...);
  a word without dashes is always a profile, only `claude` is reserved, and an unknown `--command` is an error. One
  command table drives dispatch, `ccgp --help`, `ccgp --<command> --help` and shell completion. See
  [docs/cli.md](docs/cli.md).
- `ccgp --profile add` / `list` / `rm`: several logins side by side. `add --like` clones a profile with its own
  provider (a second ChatGPT account), `--provider` adds a profile to an existing login, `--type` wires up a
  new backend; `list` shows profiles, providers, login state and Claude dirs; `rm` removes a profile and its
  provider when unused (`--purge` also deletes the isolated Claude dir and credentials).
- `ccgp --send <from> <to> [count]`: copy the latest sessions of the current directory between profiles and `claude`
  (streaming, append-only, progress output); `--import` / `--export` are aliases. Thinking blocks are stripped
  whenever the two sides belong to different providers, since their signatures are only valid for the provider that
  wrote them.
- Safe config editing: `ccgp --profile add` / `rm` edit `config.ktav` surgically (comments, key order and line
  endings stay), validate the result with the real loader before replacing the file, and keep a
  `config.ktav.bak-<timestamp>` backup whose name is never overwritten.
- `ccgp --start` / `--stop` / `--status` / `--serve`, `ccgp --config reset`; Windows, Linux and macOS.
- `ccgp --restart`, `--logs [-f] [-n N]`, `--doctor [--offline] [--json]`, `--which <profile> <model>`,
  `--logout <provider>`, `--update [--check]` (checksum-verified self-update from the GitHub release) and
  `--completion bash|zsh|powershell`; `--json` for `--status`, `--profile list` and `--models`.
- Token-in-URL access: every model request is `/call/<token>/...`; unknown tokens get `404`. `/health` reports the
  server binary's path and mtime and the number of in-flight model requests.
- Authenticated model-label metadata at `/call/<token>/model?requested=<model-id>`, resolved with the inference
  routing rules without exposing tokens or invoking upstream inference.
- Ktav configuration with `providers[]` and `profiles[]`, created on first run, hot-reloaded, changed only
  by explicit commands. Secrets live in a separate owner-only `creds` directory. The parser is the pure-Go package
  of ktav-lang/golang: nothing is downloaded on first use and the binary works offline.
- `codex` provider: ChatGPT subscription through OAuth (browser PKCE by default, device code with `--device`),
  refresh-token rotation, Anthropic Messages translated to the Responses API with reasoning round-trip.
- `codex_api` provider (OpenAI-compatible API key) and `anthropic_compat` provider (unconverted passthrough, used for
  z.ai GLM, with an `effort_map`).
- `ccgp --models` / `ccgp --models apply`: newest Codex models per Claude tier from the account catalog, per-rule
  `context_window`, built-in z.ai preset; `ccgp --login zai`. For the GPT-6 generation they write a context window of
  922000 (OpenAI's documented input limit) when the catalog's `max_context_window` is lower; a larger catalog value
  wins.
- Per-model context windows and compaction: the launcher sets `CLAUDE_CODE_AUTO_COMPACT_WINDOW` from the rule of the
  session's main model, clamped to the 100k-1M range Claude Code accepts; `compact_percent`.
- `count_tokens` for `codex` and `codex_api` answers with a length-based estimate instead of a constant.
- Isolated Claude Code configuration per profile: official credentials never reach a ccgp session; agents, skills,
  commands, plugins and settings are mirrored one way.
- The model-identity sentence in Claude Code's prompt is replaced with the real backend model.
- `ccgpy <profile> [claude args…]`: a shortcut for `ccgp <profile> --dangerously-skip-permissions …` (Claude Code with
  permission prompts off), a second small binary that starts the `ccgp` next to it.
- Installation: `install.sh` (Linux, macOS) and `install.ps1` (Windows) install `ccgp` and `ccgpy` from the GitHub
  release for the OS and CPU, checked against `SHA256SUMS`; the npm package `@cc-go-proxy/ccgp`
  (`npm install -g @cc-go-proxy/ccgp`, in the npm organization `cc-go-proxy`; the
  platform packages are `@cc-go-proxy/ccgp-<os>-<cpu>`) installs both commands. CI checks that the scripts install the newest release.
- Releases: binaries for ten platforms with checksums and build-provenance attestations, npm packages (token or
  Trusted Publishing, switched on by the repository variable `NPM_PUBLISH`), third-party license notices; dual
  license check in CI.

### Changed
- The Go module is now `github.com/PHPCraftdream/cc-go-proxy`.
- Licensed under `MIT OR Apache-2.0` (the MIT-licensed upstream parts keep their notice, see `NOTICE`).
- The version is set from the release tag at build time.
- ccgp's own files are Ktav, not JSON: `oauth-<provider>.ktav`, `apikey-<provider>.ktav`, `profile-tokens.ktav` in the
  `creds` directory and `.ccgp-mirror.ktav` in a profile's Claude directory. The JSON files of earlier versions are read
  once, converted and removed, so existing logins and URL tokens keep working. A file is checked to read back as the same
  value before it is written. (Claude Code's own JSON files, such as `settings.json`, are untouched.)
- The request log says why a request failed (`502 - 2m12s POST … : <error>`, any status ≥ 400), and a Codex stream
  that breaks after the reply started gets a `stream failed` line; before, both were silent.
- Codex backend: the wait for the reply headers is limited to 90 seconds. A request the backend stalled on used to hang
  for several minutes before failing with 502; now it fails sooner and Claude Code retries it.
- Building from source needs Go 1.26 (the current x/sys, x/term and fasthttp require it); dependencies updated:
  fiber 2.52.15, fasthttp 1.76.0, x/sys 0.49.0, x/term 0.47.0 and others.

### Fixed
- Long sessions no longer die with `ECONNRESET`: the request body cap was fiber's 4 MiB default, which a conversation
  with many reasoning signatures outgrows. It is now `listen.max_body_mb` (default `256`, written into the default
  config); refused bodies get `413` and a `rejected` log line instead of vanishing silently.

[Unreleased]: https://github.com/PHPCraftdream/cc-go-proxy/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/PHPCraftdream/cc-go-proxy/releases/tag/v0.1.0
