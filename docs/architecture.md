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
| `cmd/ccgpy` | `ccgpy <profile> …`: starts the `ccgp` next to it (or in `PATH`) as `ccgp <profile> --dangerously-skip-permissions …`; passes the terminal and exit code through |
| `cmd/ccgp` | CLI: `<profile>` launcher plus `--commands` from one table (`commands.go`); see [cli.md](cli.md) |
| `internal/config` | Ktav config (`providers[]`, `profiles[]`), model rules (`ResolveModel`), managed blocks, surgical profile add/remove text editing with backup and validation, defaults |
| `internal/creds` | Generated secrets: profile URL tokens, API keys, OAuth file paths; the token index |
| `internal/securefile` | Atomic owner-only writes (Windows DACL / Unix `0600`) |
| `internal/oauth` | Codex browser (PKCE) and device-code login, refresh rotation, `Session` |
| `internal/codex` | Messages → Responses request, Responses SSE → Messages events, backend client (large request bodies are zstd-compressed, plain again if the backend refuses), model catalog |
| `internal/connpool` | Transport of every upstream client (a `Router` keeps one `Transport` per way out: global proxy, a profile's own, none): an active window of lanes (`connections.active`, default 7; own pool and connection each, fewest in flight wins); each lane renews its pool every 10 minutes, at staggered times, and an old pool closes its connections as their last request ends; `connections.total` (default 10) caps active plus draining pools, so a renewal waits for room and a running request is never cut; the server applies the limits at start and on config reload; HTTP/2 connections ping when they are quiet for 30 s and are closed when the ping is not answered in 15 s, so a connection that something on the way killed silently leaves the pool before a request picks it; a lane's next pool opens its connection in the background and takes over the moment it is up (requests never wait for a handshake; the old pool drains), among equally busy lanes the one with a fresh warm connection goes first, and a warm connection that waited over 20 s is closed and replaced by a fresh dial, because bare TLS without HTTP/2 traffic gets cut by firewalls and antivirus on the way; a request whose connection could not be opened (never sent) goes to a lane with a working one and is tried again, with growing pauses, for up to 30 s; one whose connection broke before the reply is sent again at most twice. Connections that the far end cuts after a fixed lifetime (seen: 30 minutes to chatgpt.com) never reach it |
| `internal/proxydial` | SOCKS5 (RFC 1928/1929) and HTTP(S) `CONNECT` dialers for `connections.proxy`; `Choice` (a proxy, none, or the environment's) carried on the request context, so the profile's choice (`Config.ProxyChoice`) picks the way out per request; a default for requests without one |
| `internal/converter` | Messages ↔ Chat Completions for `codex_api` |
| `internal/server` | fiber app, `/call/:token/...` auth, per-profile config, hot config reload, handlers |
| `internal/claudehome` | The isolated Claude Code config directory and its one-way mirror |
| `internal/sessions` | `--send` (aliases `--import` / `--export`): streaming, append-only session copy and thinking-block removal |
| `internal/identity` | Model-identity sentence rewrite |
| `internal/preset` | Built-in mappings and effort maps for known providers (z.ai) |
| `internal/angel` | Detached background process, `/health`-based control (health also reports the server binary path, its mtime and the in-flight request count, which `ccgp <profile>` uses to restart a stale idle server) |

## Invariants

- Every model endpoint lives under `/call/:token/`; an unknown token is `404`. Tokens are never logged.
- `config.ktav` is written only on the user's command: `ccgp --profile add` / `rm` (surgical text edit, validated
  with the real loader before the file is replaced, `config.ktav.bak-<timestamp>` backup), `ccgp --models apply` and
  the refresh after a fresh `ccgp --login` (only the marked block), `ccgp --config reset` (default template, old file
  backed up). Secrets go through `securefile`.
- A rotated OAuth refresh token is persisted before it is used.
- Codex backend calls happen before streaming starts, so the upstream HTTP status reaches the client.
- The user's own Claude Code directory is only read, except `ccgp --send ... claude` / `ccgp --export` (adds or
  extends files under `projects/`, append-only). Credentials, prompt history and the process registry are
  never copied.

## Dependencies

[fiber](https://github.com/gofiber/fiber) for HTTP, [ktav-lang/golang](https://github.com/ktav-lang/golang) for the
config (its pure-Go package, no native library), `golang.org/x/sys` and `x/term`. All are permissively licensed; CI enforces an allowlist
(see [releasing.md](releasing.md)).
