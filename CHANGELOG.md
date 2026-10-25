# Changelog

All notable changes to ccgp are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

ccgp is a fork of [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy); its own
history is kept in [docs/upstream-changelog.md](docs/upstream-changelog.md).

## [Unreleased]

## [0.2.0] - 2026-10-25

### Changed
- **Breaking:** CLI commands start with `--`: `ccgp --status`, `--login`, `--models`, `--profile`, `--send`, `--import`,
  `--export`, `--start`, `--stop`, `--serve`, `--config`, `--version`, `--help` (the bare words are gone). A word
  without dashes is always a profile, so profiles may now be named `status`, `login`, `serve`; only `claude` stays
  reserved. An unknown first argument with a dash is an error, not a profile run.
- `ccgp <profile>` restarts a background server that was started from an older binary when it is idle, and warns
  when requests are in flight. `/health` now also reports the server binary's path, its mtime and the number of
  in-flight model requests.

- The release workflow publishes to npm only when the repository variable `NPM_PUBLISH` is `true` (the job still
  builds and dry-runs the packages); the installer scripts and the CI check that they install the newest release.

### Added
- `ccgp --restart`, `--logs [-f] [-n N]`, `--doctor [--offline] [--json]`, `--which <profile> <model>`,
  `--logout <provider>`, `--update [--check]` (checksum-verified self-update from the GitHub release) and
  `--completion bash|zsh|powershell`; `--json` for `--status`, `--profile list` and `--models`;
  `ccgp --<command> --help` for every command. See [docs/cli.md](docs/cli.md).
- Authenticated model-label metadata at `/call/<token>/model?requested=<model-id>`, resolved with the inference routing rules without exposing tokens or invoking upstream inference.
- `install.sh` (Linux, macOS) and `install.ps1` (Windows): install `ccgp` and `ccgpy` from the GitHub release for the OS and
  CPU, checked against `SHA256SUMS`. CI runs both against the published release.

## [0.1.1] - 2026-10-22

### Fixed
- The npm package is `cc-go-proxy` (`npm install -g cc-go-proxy`; the commands stay `ccgp` and `ccgpy`): npm refused
  the name `ccgp` as too similar to `ncp`, so 0.1.0 reached GitHub Releases but not npm.

## [0.1.0] - 2026-10-22

First release of ccgp as an independent project.

### Added
- `ccgp profile add` / `list` / `rm`: several logins side by side. `add --like` clones a profile with its own
  provider (a second ChatGPT account), `--provider` adds a profile to an existing login, `--type` wires up a
  new backend; `list` shows profiles, providers, login state and Claude dirs; `rm` removes a profile and its
  provider when unused (`--purge` also deletes the isolated Claude dir and credentials).
- `ccgp send <from> <to> [count]`: copy the latest sessions between profiles and `claude`; `import` / `export`
  are aliases. Thinking blocks are now stripped whenever the two sides belong to different providers (including
  `import`, which used to keep them), since their signatures are only valid for the provider that wrote them.
- Safe config editing: `ccgp profile add` / `rm` edit `config.ktav` surgically (comments, key order and line
  endings stay), validate the result with the real loader before replacing the file, and keep a
  `config.ktav.bak-<timestamp>` backup whose name is never overwritten.
- `ccgp <profile>` launcher: starts the background server, makes sure credentials exist, runs Claude Code through
  the profile. Named profiles, each with its own URL token and its own provider.
- Token-in-URL access: every model request is `/call/<token>/...`; unknown tokens get `404`.
- Ktav configuration with `providers[]` and `profiles[]`, created on first run, hot-reloaded, changed only
  by explicit commands. Secrets live in a separate owner-only `creds` directory.
- `codex` provider: ChatGPT subscription through OAuth (browser PKCE by default, device code with `--device`),
  refresh-token rotation, Anthropic Messages translated to the Responses API with reasoning round-trip.
- `codex_api` provider (OpenAI-compatible API key) and `anthropic_compat` provider (unconverted passthrough, used for
  z.ai GLM, with an `effort_map`).
- `ccgp models` / `ccgp models apply`: newest Codex models per Claude tier from the account catalog, per-rule
  `context_window`, built-in z.ai preset; `ccgp login zai`.
- Per-model context windows and compaction: the launcher sets `CLAUDE_CODE_AUTO_COMPACT_WINDOW` from the rule of the
  session's main model, clamped to the 100k-1M range Claude Code accepts; `compact_percent`.
- `count_tokens` for `codex` and `codex_api` answers with a length-based estimate instead of a constant.
- Isolated Claude Code configuration per profile: official credentials never reach a ccgp session; agents, skills,
  commands, plugins and settings are mirrored one way.
- The model-identity sentence in Claude Code's prompt is replaced with the real backend model.
- `ccgp import` / `ccgp export`: copy the latest sessions between your Claude Code and a profile (streaming,
  append-only, thinking blocks removed on export).
- `ccgp config reset`, `ccgp status`, `ccgp start` / `stop`; Windows, Linux and macOS.
- Releases: binaries for six platforms with checksums and build-provenance attestations, npm packages (token or
  Trusted Publishing), third-party license notices; dual license check in CI.
- For the GPT-6 generation `ccgp models` / `apply` write a context window of 922000 (OpenAI's documented input limit)
  when the catalog's `max_context_window` is lower (872000), so Claude Code compacts later; a larger catalog value wins.
- `ccgpy <profile> [claude args…]`: a shortcut for `ccgp <profile> --dangerously-skip-permissions …` (Claude Code with
  permission prompts off), a second small binary that starts the `ccgp` next to it. It is built and packaged with every
  release (archives, and the npm package installs both commands).

### Changed
- The Go module is now `github.com/PHPCraftdream/cc-go-proxy`.
- Licensed under `MIT OR Apache-2.0` (the MIT-licensed upstream parts keep their notice, see `NOTICE`).
- The version is set from the release tag at build time.
- ccgp's own files are Ktav now, not JSON: `oauth-<provider>.ktav`, `apikey-<provider>.ktav`, `profile-tokens.ktav` in the
  `creds` directory and `.ccgp-mirror.ktav` in a profile's Claude directory. The JSON files of earlier versions are read
  once, converted and removed, so existing logins and URL tokens keep working. A file is checked to read back as the same
  value before it is written. (Claude Code's own JSON files, such as `settings.json`, are untouched.)
- The config parser is now the pure-Go package of ktav-lang/golang 0.8.1: no native library is downloaded on first
  use, `KTAV_LIB_PATH` is no longer needed, and the binary works offline from the first run.
- `ccgp send` / `import` / `export` copy only the sessions of the directory they are run in (the latest 10 of that
  directory by default), not the latest sessions of every project, and print progress: `[k/N]` per session and a
  percentage line every two seconds on large transcripts.
- The request log now says why a request failed (`502 - 2m12s POST … : <error>`, any status ≥ 400), and a Codex stream
  that breaks after the reply started gets a `stream failed` line; before, both were silent.
- Codex backend: the wait for the reply headers is limited to 90 seconds. A request the backend stalled on used to hang
  for several minutes before failing with 502; now it fails sooner and Claude Code retries it.
- Building from source needs Go 1.26 (the current x/sys, x/term and fasthttp require it); dependencies updated:
  fiber 2.52.15, fasthttp 1.76.0, x/sys 0.49.0, x/term 0.47.0 and others.

### Fixed
- Long sessions no longer die with `ECONNRESET`: the request body cap was fiber's 4 MiB default, which a conversation
  with many reasoning signatures outgrows. It is now `listen.max_body_mb` (default `256`, written into the default
  config); refused bodies get `413` and a `rejected` log line instead of vanishing silently.

[Unreleased]: https://github.com/PHPCraftdream/cc-go-proxy/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/PHPCraftdream/cc-go-proxy/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/PHPCraftdream/cc-go-proxy/releases/tag/v0.1.0
