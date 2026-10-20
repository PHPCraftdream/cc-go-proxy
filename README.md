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
- `providers[]` — `name`, `type` (`codex` — ChatGPT subscription via OAuth; `codex_api` — API key, pay per token, `openai` is the old name; `anthropic_compat` — an Anthropic-compatible endpoint such as z.ai, forwarded unconverted), `base_url`, optional `api_key_env`, optional `effort_map`.
- `profiles[]` — `name`, `provider`, ordered `models[]` rules (`match` glob → `model`),
  `default_model` (legacy; prefer a final `match: *` rule). Each rule can carry `context_window`.
  Without rules or default, tier names map to `gpt-5` / `gpt-5-mini`.

`config.ktav` is created once and afterwards only read, never rewritten by ccgp (your comments are safe). All machine-managed state — profile tokens, API keys and the rotating OAuth tokens — lives in the separate `creds` directory (`ccgp status` shows both paths). Config edits apply to a running server
without restart (listen address changes need `ccgp stop`).

## Codex models and context

`ccgp models` reads your account's Codex model catalog and maps Claude tiers to the
newest releases (read-only preview); `ccgp models apply [profile]` writes the mapping:

| Claude tier | Codex model |
| --- | --- |
| Fable | newest `astra` |
| Opus | newest `sol` |
| Sonnet | newest `luna` |
| Haiku | `luna` of the release before the newest one (e.g. `gpt-5.6-luna`) |
| anything else | the Sonnet model (final `match: *` rule) |

Every rule states its own context window:

```
models: [
    {
        match: *opus*
        model: gpt-6.1-sol
        context_window: 872000
    }
    ...
]
```

It rewrites only the block between the `ccgp:managed-models begin/end <profile>` marker
lines of a profile (added automatically if missing; the profile's old `default_model`,
`context_window` and `models` entries are then replaced). The mapping is also refreshed
after every fresh `ccgp login <codex provider>`.

Windows come from the account catalog (`ccgp models -v` shows every field). The catalog has two
numbers: `context_window` is only the default of Codex clients, `max_context_window` is the
largest input the backend takes. For the GPT-6 generation and newer ccgp writes
`max_context_window` (872000), for earlier releases such as `gpt-5.6-luna` the default
(272000). A model without any window is written as 272000 and noted.

Claude Code accepts one window per process, so the launcher passes
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` = the `context_window` of the rule that serves the
session's **main model** (`--model`, `ANTHROPIC_MODEL`, `model` in settings.json, default
`sonnet`). A smaller window on another rule (the Haiku model's 272000) does not shrink the
main session but is not covered either. A profile-level `context_window: N` overrides this,
`compact_percent: 70` compacts earlier still (`CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`), and
variables you set yourself are never overridden.

## z.ai (GLM)

```
ccgp login zai     # asks for your z.ai API key once (hidden input), saves it owner-only
ccgp zai           # runs Claude Code through z.ai; extra arguments go to claude
```

The `zai` provider (type `anthropic_compat`) talks to z.ai's Anthropic-compatible endpoint
`https://api.z.ai/api/anthropic`: requests are forwarded unconverted, only the model is
mapped and the model identity in the system prompt is corrected. Built-in mapping
(`ccgp models apply zai` rewrites it; saving the key does it automatically):

| Claude tier | z.ai model | context |
| --- | --- | --- |
| Fable | `glm-5.3` | 1M |
| Opus, Sonnet, Haiku, anything else | `glm-5.3-flash` | 1M |

z.ai accepts only three effort levels, so Claude Code's five are converted (the provider's
`effort_map`): `low` → `low`, `medium` → `high`, `high` / `xhigh` / `max` → `max`. Any
provider of type `anthropic_compat` can set its own `effort_map`.

Endpoint and model data come from [docs.z.ai](https://docs.z.ai/devpack/tool/claude).
`ccgp config reset` backs up your config and writes the current default one.

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

To keep working conditions familiar, everything except credentials is copied from your own
Claude Code config dir at every launch, one way (your own dir is only ever read):

- `CLAUDE.md`, `settings.json` and `keybindings.json` are overwritten from the source;
- `agents`, `commands`, `skills`, `output-styles` and `plugins` are mirrored incrementally
  (only changed files are copied; files you delete at the source are removed from the copy;
  files created inside the isolated dir are kept; credential files are never copied).
  Absolute paths in `plugins/*.json` are rewritten to point at the copy.

History, sessions and projects stay separate.

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
