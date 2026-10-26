# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Project

`ccgp` translates Anthropic Messages API requests from Claude Code to:

- OpenAI-compatible Chat Completions (`internal/converter`, `internal/server/handlers.go`);
- the ChatGPT Codex Responses API via OAuth (`internal/codex`, `internal/oauth`,
  `internal/server/codex.go`).

## Layout

| Path | Role |
| --- | --- |
| `cmd/ccgpy` | `ccgpy <profile> …`: runs the sibling `ccgp <profile> --dangerously-skip-permissions …`, nothing else |
| `cmd/ccgp` | CLI: `<profile>` launcher plus `--commands` (one table in `commands.go`: dispatch, `--help`, completion; a bare word is always a profile). Commands: `--serve/--start/--stop/--restart/--status/--logs/--doctor`, `--login/--logout`, `--models/--which`, `--config`, `--profile add/list/rm`, `--send/--import/--export`, `--update`, `--completion`; `--json` where documented |
| `internal/config` | env + Ktav config (`providers[]`, `profiles[]`), model mapping (`ResolveModel`), surgical config editing (`EditFile`, `AddProfileText`, `RemoveProfileText`) with backup and loader validation |
| `internal/creds` | generated secrets: profile URL tokens, API keys, OAuth file paths; `TokenIndex` |
| `internal/ktavfile` | read/write ccgp's own files (logins, URL tokens, mirror manifest) as Ktav, verified to read back; converts and removes the JSON files of earlier versions |
| `internal/securefile` | atomic owner-only writes (Windows DACL / Unix 0600) |
| `internal/oauth` | Codex device-code login, refresh rotation, `Session` |
| `internal/codex` | Messages → Responses request, Responses SSE → Messages events, backend client |
| `internal/connpool` | upstream transport: lanes (fewest in flight), pools renewed every 10 min at staggered times, old pools drained, one retry when the connection breaks before the reply |
| `internal/sessions` | `ccgp --send` (aliases `--import`/`--export`): copy session transcripts (`projects/*/*.jsonl` + sidecar dir) between Claude Code config dirs; `StripThinking` |
| `internal/server` | fiber app, `/call/:token/...` auth, per-profile config, hot config reload |
| `internal/angel` | detached background process, `/health`-based control |

## Invariants

- Every model endpoint is under `/call/:token/`; unknown token → 404. Never log tokens
  (`maskToken`).
- `config.ktav` is written only on the user's command: `ccgp --profile add` / `rm` (surgical edit through
  `config.EditFile`: validated with the real loader, `config.ktav.bak-<timestamp>` backup), `ccgp --models apply`
  and the refresh after `ccgp --login` (only the marked `ccgp:managed-models` block) and `ccgp --config reset` (default
  template, old file backed up). Secrets go through `securefile.WriteFile`.
- Rotated OAuth refresh tokens are persisted before use (`Session.refreshLocked`).
- Codex backend calls happen before streaming starts so upstream HTTP status reaches the client.
- The user's own Claude Code dir is only read, except `ccgp --send ... claude` / `ccgp --export`, which may add or
  extend files under `projects/` (append-only rule in `sessions.syncFile`; never overwrites diverged data).
  Credentials, `history.jsonl`, `sessions/` are never copied in either direction.

## Releases and licensing

- Licensed `MIT OR Apache-2.0` (`LICENSE-MIT`, `LICENSE-APACHE`, `NOTICE`); the fork origin
  nielspeter/claude-code-proxy stays credited. Dependencies must be permissive (allowlist in
  `.github/workflows/licenses.yml`).
- A release is a tag `vX.Y.Z` on main; `.github/workflows/release.yml` builds, publishes and
  sets the version via ldflags. Do not tag or bump versions unless the user asks. Process and npm
  token / Trusted Publishing setup: `docs/releasing.md`.
- Keep the README's first line (fork notice) and `CHANGELOG.md` `## [Unreleased]` up to date.

## Commands

```sh
go vet ./...
go test ./...
make build
```
