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

### Fixed
- Requests no longer fail together when the backend (or a proxy on the way) cuts a connection after a fixed lifetime:
  every 30 minutes all streams to `chatgpt.com` ended in `unexpected EOF` and new requests got a 502. Upstream
  connections are now renewed every 10 minutes without interrupting running requests, and a request whose
  connection breaks before the reply is sent again.
- HTTP/2 connections to the backends send a health ping when they have been quiet for 30 seconds and are closed when
  it is not answered within 15 (shorter limits cut streams that only waited for a slow backend): a connection that a proxy program or an antivirus killed silently while it was idle no
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
- When the Codex backend answers `server_is_overloaded` before sending any content, ccgp asks again after 2 s and
  then 5 s instead of passing the error on; the log shows `upstream retry`.
- Streams the client left are logged as `stream canceled by client`, apart from real failures.
- The timing in the log shows the real TLS handshake time of new connections (it read `tls 0s`), marks connections
  opened in the background as `conn warm (opened … before)` and names the error a request was retried after.
- The first launch of a profile that has no sessions copies the latest session of the current directory from your
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

## [0.2.0] - 2026-10-25

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
  release for the OS and CPU, checked against `SHA256SUMS`; the npm package `@phpcraftdream/ccgp`
  (`npm install -g @phpcraftdream/ccgp`, under the author's own scope so that it does not crowd the npm search; the
  platform packages are `@phpcraftdream/ccgp-<os>-<cpu>`) installs both commands. CI checks that the scripts install the newest release.
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

[Unreleased]: https://github.com/PHPCraftdream/cc-go-proxy/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/PHPCraftdream/cc-go-proxy/releases/tag/v0.2.0
