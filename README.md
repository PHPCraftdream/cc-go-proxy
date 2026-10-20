# ccgp — Claude Code bridge for OpenAI and ChatGPT Codex

`ccgp` runs Claude Code against non-Anthropic backends:

- **OpenAI-compatible** Chat Completions (OpenAI, OpenRouter, Ollama, …) with an API key;
- **ChatGPT Codex** (subscription) via OAuth device-code login, with refresh-token rotation.

It is a fork of [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy)
(MIT), reworked for headless use: profiles, Ktav config, per-profile URL tokens,
background server management.

## Quick start

1. Copy [`config.example.ktav`](config.example.ktav) to
   `%APPDATA%\cc-go-proxy\config.ktav` (Linux: `~/.config/cc-go-proxy/config.ktav`)
   and edit providers and profiles.
2. Run Claude Code through a profile:

   ```sh
   ccgp work                 # interactive
   ccgp work -p "hello"      # headless; everything after the profile goes to claude
   ```

On first run for a profile `ccgp`:

1. starts the proxy in the background if it is not running;
2. ensures provider credentials — prints a device-code link for `codex`
   (the browser is never opened automatically), or prompts for an API key;
3. generates the profile's URL token;
4. runs `claude` with `ANTHROPIC_BASE_URL=http://127.0.0.1:60093/call/<token>`.

## Commands

| Command | Purpose |
| --- | --- |
| `ccgp <profile> [claude args…]` | Ensure server + credentials, run `claude` |
| `ccgp serve [-d] [-s]` | Run the server in the foreground (`-d` debug bodies, `-s` request log) |
| `ccgp start` / `stop` / `status` | Manage the background server |
| `ccgp login <provider>` | (Re)authenticate a provider |

Background server log: `%LOCALAPPDATA%\cc-go-proxy\logs\ccgp.log`
(Linux: `~/.cache/cc-go-proxy/logs/ccgp.log`), rotated at 10 MB.

## Configuration

See [`config.example.ktav`](config.example.ktav). Summary:

- `listen.host` / `listen.port` — default `127.0.0.1:60093` (loopback only).
- `providers[]` — `name`, `type` (`openai` | `codex`), `base_url`, optional `api_key_env`.
- `profiles[]` — `name`, `provider`, ordered `models[]` rules (`match` glob → `model`),
  `default_model`. Without rules or default, tier names map to `gpt-5` / `gpt-5-mini`.

`config.ktav` is only read, never written; edits apply to a running server
without restart (listen address changes need `ccgp stop`).

## Security model

- Every request must carry a profile token in the path: `/call/<token>/v1/messages`.
  Unknown tokens get `404`. Only `/health` is open. Tokens are masked in logs.
- No CORS: browsers cannot call the proxy.
- Secrets live in `<config dir>/cc-go-proxy/creds/`, written atomically and readable
  only by the current user (Windows: explicit owner-only DACL; Unix: `0600`).
- The Codex OAuth session is separate from the Codex CLI's own login, so the two
  never invalidate each other's rotating refresh tokens.

## Codex backend notes

Requests are translated from Anthropic Messages to the Responses API
(`https://chatgpt.com/backend-api/codex/responses`): text, images, tools,
tool results and reasoning. Reasoning `encrypted_content` is carried in
thinking-block signatures (`ccgp1:` prefix) so it round-trips across turns.
Using a ChatGPT subscription through a third-party client is subject to
OpenAI's terms; that decision is yours.

## Env-only mode

Without `config.ktav`, one implicit profile `default` is built from env:
`OPENAI_API_KEY`, `OPENAI_BASE_URL`, `PROXY_TOKEN` (min 32 chars),
`ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL`, `HOST`, `PORT`.

## Development

```sh
make build      # ./ccgp
make test
```

Ktav parsing uses [`ktav-lang/golang`](https://github.com/ktav-lang/golang), which
downloads its native library from GitHub Releases on first use (or set `KTAV_LIB_PATH`).

## License

MIT, see [LICENSE](LICENSE).
