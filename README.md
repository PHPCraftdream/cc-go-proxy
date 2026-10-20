# ccgp — Claude Code bridge for OpenAI and ChatGPT Codex

`ccgp` runs Claude Code against non-Anthropic backends:

- **OpenAI-compatible** Chat Completions (OpenAI, OpenRouter, Ollama, …) with an API key;
- **ChatGPT Codex** (subscription) via OAuth (browser sign-in or device code), with refresh-token rotation.

It is a fork of [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy)
(MIT), reworked for headless use: profiles, Ktav config, per-profile URL tokens,
background server management.

## Quick start

1. Run `ccgp codex` (or any profile/`serve`/`start`/`login`): on first
   run it creates a default
   [`config.ktav`](internal/config/config.example.ktav) in
   `%APPDATA%\cc-go-proxy\` (Linux: `~/.config/cc-go-proxy/`) with a `codex` and a
   `codex_api` provider/profile. Edit model names to what your account can use.
2. Run Claude Code through a profile:

   ```sh
   ccgp codex                # interactive
   ccgp codex -p "hello"     # headless; everything after the profile goes to claude
   ```

On first run for a profile `ccgp`:

1. starts the proxy in the background if it is not running;
2. ensures provider credentials — prints a sign-in link for `codex`
   (the browser is never opened automatically), or prompts for an API key;
3. generates the profile's URL token;
4. runs `claude` with `ANTHROPIC_BASE_URL=http://127.0.0.1:60093/call/<token>`.

## Commands

| Command | Purpose |
| --- | --- |
| `ccgp <profile> [claude args…]` | Ensure server + credentials, run `claude` |
| `ccgp serve [-d] [-s]` | Run the server in the foreground (`-d` debug bodies, `-s` request log) |
| `ccgp start` / `stop` / `status` | Manage the background server |
| `ccgp login <provider> [--device]` | (Re)authenticate a provider. Codex: browser OAuth by default, `--device` for device code |

Background server log: `%LOCALAPPDATA%\cc-go-proxy\logs\ccgp.log`
(Linux: `~/.cache/cc-go-proxy/logs/ccgp.log`), rotated at 10 MB.

## Configuration

See the generated file (template: [`config.example.ktav`](internal/config/config.example.ktav)). Summary:

- `listen.host` / `listen.port` — default `127.0.0.1:60093` (loopback only).
- `providers[]` — `name`, `type` (`codex` — ChatGPT subscription via OAuth, or `codex_api` — API key, pay per token; `openai` is accepted as the old name of `codex_api`), `base_url`, optional `api_key_env`.
- `profiles[]` — `name`, `provider`, ordered `models[]` rules (`match` glob → `model`),
  `default_model`. Without rules or default, tier names map to `gpt-5` / `gpt-5-mini`.

`config.ktav` is created once and afterwards only read, never rewritten by ccgp (your comments are safe). All machine-managed state — profile tokens, API keys and the rotating OAuth tokens — lives in the separate `creds` directory (`ccgp status` shows both paths). Config edits apply to a running server
without restart (listen address changes need `ccgp stop`).

## Codex models and context

`ccgp models` reads your account's Codex model catalog and maps Claude tiers to the
newest releases (read-only preview); `ccgp models apply [profile]` writes the mapping:

| Claude tier | Codex model |
| --- | --- |
| Fable | newest `astra` |
| Opus | newest `sol` |
| Sonnet | newest `luna` (also `default_model`) |
| Haiku | `luna` of the release before the newest one (e.g. `gpt-5.6-luna`) |

It rewrites only the block between the `ccgp:managed-models begin/end <profile>` marker
lines of a profile (added automatically if missing; a profile's old `default_model`,
`context_window` and `models` entries are then replaced). The mapping is also refreshed
after every fresh `ccgp login <codex provider>`.

Codex models have smaller context windows than the Claude models Claude Code thinks it
talks to, so the block also stores `context_window`: the smallest window among the
Fable/Opus/Sonnet models (the main conversation; unknown windows count as 272000). The
launcher passes it to Claude Code as `CLAUDE_CODE_AUTO_COMPACT_WINDOW`, so conversations
are compacted before the backend limit. Add `compact_percent: 70` to a profile to
compact earlier still (`CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`). Claude Code accepts one window
per process: Haiku-tier models with a smaller window are reported by `ccgp models` but do
not lower it. Variables you set yourself are never overridden.

## Official credentials never reach a ccgp session

`ccgp <profile>` runs `claude` with its own config directory,
`%APPDATA%\cc-go-proxy\claude\<profile>` (`CLAUDE_CONFIG_DIR`), so your claude.ai login
and Anthropic API keys are not available to it:

- no `.credentials.json` there; one left behind (e.g. by `/login`) is deleted at the next launch;
- parent environment variables that select an official endpoint or credential
  (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`,
  `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_CODE_USE_BEDROCK/VERTEX/FOUNDRY`) are dropped;
- `settings.json` is mirrored without `apiKeyHelper`, `forceLogin*` and those variables in `env`;
- `.claude.json` is seeded once with onboarding state, theme and MCP servers only (no account data).

To keep working conditions familiar, `CLAUDE.md`, `settings.json` and `keybindings.json`
are copied from your own Claude Code config dir at every launch (edits made in the isolated
copy are overwritten), and `agents`, `commands`, `skills`, `output-styles` and `plugins`
are shared with it through junctions/symlinks. History, sessions and projects stay separate.

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
