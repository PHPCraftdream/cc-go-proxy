# Changelog

All notable changes to ccgp are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

ccgp is a fork of [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy); its own
history is kept in [docs/upstream-changelog.md](docs/upstream-changelog.md).

## [Unreleased]

## [0.1.0] - 2026-10-21

First release of ccgp as an independent project.

### Added
- `ccgp <profile>` launcher: starts the background server, makes sure credentials exist, runs Claude Code through
  the profile. Named profiles, each with its own URL token and its own provider.
- Token-in-URL access: every model request is `/call/<token>/...`; unknown tokens get `404`.
- Ktav configuration with `providers[]` and `profiles[]`, created on first run, hot-reloaded, never rewritten
  (except marked model-mapping blocks). Secrets live in a separate owner-only `creds` directory.
- `codex` provider: ChatGPT subscription through OAuth (browser PKCE by default, device code with `--device`),
  refresh-token rotation, Anthropic Messages translated to the Responses API with reasoning round-trip.
- `codex_api` provider (OpenAI-compatible API key) and `anthropic_compat` provider (unconverted passthrough, used for
  z.ai GLM, with an `effort_map`).
- `ccgp models` / `ccgp models apply`: newest Codex models per Claude tier from the account catalog, per-rule
  `context_window`, built-in z.ai preset; `ccgp login zai`.
- Per-model context windows and compaction: the launcher sets `CLAUDE_CODE_AUTO_COMPACT_WINDOW` from the rule of the
  session's main model; `compact_percent`.
- Isolated Claude Code configuration per profile: official credentials never reach a ccgp session; agents, skills,
  commands, plugins and settings are mirrored one way.
- The model-identity sentence in Claude Code's prompt is replaced with the real backend model.
- `ccgp import` / `ccgp export`: copy the latest sessions between your Claude Code and a profile (streaming,
  append-only, thinking blocks removed on export).
- `ccgp config reset`, `ccgp status`, `ccgp start` / `stop`; Windows, Linux and macOS.
- Releases: binaries for six platforms with checksums and build-provenance attestations, npm packages (token or
  Trusted Publishing), third-party license notices; dual license check in CI.

### Changed
- The Go module is now `github.com/PHPCraftdream/cc-go-proxy`.
- Licensed under `MIT OR Apache-2.0` (the MIT-licensed upstream parts keep their notice, see `NOTICE`).
- The version is set from the release tag at build time.

[Unreleased]: https://github.com/PHPCraftdream/cc-go-proxy/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/PHPCraftdream/cc-go-proxy/releases/tag/v0.1.0
