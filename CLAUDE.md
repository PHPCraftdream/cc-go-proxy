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
| `cmd/ccgp` | CLI: `<profile>` launcher, `serve`, `start/stop/status`, `login` |
| `internal/config` | env + Ktav config (`providers[]`, `profiles[]`), model mapping (`ResolveModel`) |
| `internal/creds` | generated secrets: profile URL tokens, API keys, OAuth file paths; `TokenIndex` |
| `internal/securefile` | atomic owner-only writes (Windows DACL / Unix 0600) |
| `internal/oauth` | Codex device-code login, refresh rotation, `Session` |
| `internal/codex` | Messages → Responses request, Responses SSE → Messages events, backend client |
| `internal/sessions` | `ccgp import/export`: copy session transcripts (`projects/*/*.jsonl` + sidecar dir) between Claude Code config dirs; `StripThinking` |
| `internal/server` | fiber app, `/call/:token/...` auth, per-profile config, hot config reload |
| `internal/daemon` | detached background process, `/health`-based control |

## Invariants

- Every model endpoint is under `/call/:token/`; unknown token → 404. Never log tokens
  (`maskToken`).
- `config.ktav` is never written by code; secrets go through `securefile.WriteFile`.
- Rotated OAuth refresh tokens are persisted before use (`Session.refreshLocked`).
- Codex backend calls happen before streaming starts so upstream HTTP status reaches the client.
- The user's own Claude Code dir is only read, except `ccgp export`, which may add or extend
  files under `projects/` (append-only rule in `sessions.syncFile`; never overwrites diverged data).
  Credentials, `history.jsonl`, `sessions/` are never copied in either direction.

## Commands

```sh
go vet ./...
go test ./...
make build
```
