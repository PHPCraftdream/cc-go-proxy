# Architecture

```
Claude Code ──ANTHROPIC_BASE_URL=http://127.0.0.1:60093/call/<token>──▶ ccgp server ──▶ backend
   ▲                                                                     │
   └── launched by `ccgp <profile>` with an isolated CLAUDE_CONFIG_DIR   ├─ codex:            Messages → Responses API (OAuth)
                                                                         ├─ codex_api:        Messages → Chat Completions (API key)
                                                                         └─ anthropic_compat: Messages passthrough (API key)
```

`ccgp <profile> [claude args]` does, in order: load the config → start the background server if it is not
running → make sure the provider has credentials (sign-in link or key prompt) → make sure the profile has a URL
token → prepare the isolated Claude Code config directory → run `claude` with `ANTHROPIC_BASE_URL` pointing at
`/call/<token>` and with the context-window variables set.

## Layout

| Path | Role |
| --- | --- |
| `cmd/ccgp` | CLI: `<profile>` launcher, `serve`, `start` / `stop` / `status`, `login`, `models`, `config`, `import` / `export` |
| `internal/config` | Ktav config (`providers[]`, `profiles[]`), model rules (`ResolveModel`), managed blocks, defaults |
| `internal/creds` | Generated secrets: profile URL tokens, API keys, OAuth file paths; the token index |
| `internal/securefile` | Atomic owner-only writes (Windows DACL / Unix `0600`) |
| `internal/oauth` | Codex browser (PKCE) and device-code login, refresh rotation, `Session` |
| `internal/codex` | Messages → Responses request, Responses SSE → Messages events, backend client, model catalog |
| `internal/converter` | Messages ↔ Chat Completions for `codex_api` |
| `internal/server` | fiber app, `/call/:token/...` auth, per-profile config, hot config reload, handlers |
| `internal/claudehome` | The isolated Claude Code config directory and its one-way mirror |
| `internal/sessions` | `import` / `export`: streaming, append-only session copy and thinking-block removal |
| `internal/identity` | Model-identity sentence rewrite |
| `internal/preset` | Built-in mappings and effort maps for known providers (z.ai) |
| `internal/daemon` | Detached background process, `/health`-based control |

## Invariants

- Every model endpoint lives under `/call/:token/`; an unknown token is `404`. Tokens are never logged.
- `config.ktav` is never written by code, apart from the `ccgp:managed-models` blocks; secrets go through `securefile`.
- A rotated OAuth refresh token is persisted before it is used.
- Codex backend calls happen before streaming starts, so the upstream HTTP status reaches the client.
- The user's own Claude Code directory is only read, except `ccgp export` (adds or extends files under
  `projects/`, append-only). Credentials, prompt history and the process registry are never copied.

## Dependencies

[fiber](https://github.com/gofiber/fiber) for HTTP, [ktav-lang/golang](https://github.com/ktav-lang/golang) for the
config (it loads a small native library, downloaded from GitHub Releases on first use; set `KTAV_LIB_PATH` to
use a local copy), `golang.org/x/sys` and `x/term`. All are permissively licensed; CI enforces an allowlist
(see [releasing.md](releasing.md)).
