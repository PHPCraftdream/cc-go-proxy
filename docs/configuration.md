# Configuration

ccgp is configured with one [Ktav](https://github.com/ktav-lang/golang) file. It is created on the
first run, from [`config.example.ktav`](../internal/config/config.example.ktav):

| OS | Config | Secrets and tokens | Server log |
| --- | --- | --- | --- |
| Windows | `%APPDATA%\cc-go-proxy\config.ktav` | `%APPDATA%\cc-go-proxy\creds\` | `%LOCALAPPDATA%\cc-go-proxy\logs\ccgp.log` |
| Linux / macOS | `~/.config/cc-go-proxy/config.ktav` | `~/.config/cc-go-proxy/creds/` | `~/.cache/cc-go-proxy/logs/ccgp.log` |

`ccgp status` prints the real paths. `CCGP_CONFIG` points ccgp at another config file.

The config is **yours**: ccgp reads it and never rewrites it, except the marked blocks of
[generated model mappings](#generated-model-mappings) and `ccgp config reset` (which keeps a backup).
Everything machine-managed (profile URL tokens, API keys, the rotating OAuth tokens) lives in the
separate `creds` directory, owner-only (Windows: explicit DACL, Unix: `0600`).
A running server picks up config edits without a restart; only a change of the listen address needs `ccgp stop`.

## Layout

```
listen.host: 127.0.0.1
listen.port: 60093

providers: [
    {
        name: codex
        type: codex
    }
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
        effort_map: {
            low: low
            medium: high
            high: max
            xhigh: max
            max: max
        }
    }
]

profiles: [
    {
        name: codex
        provider: codex
        models: [
            {
                match: *opus*
                model: gpt-6-sol
                context_window: 872000
            }
            {
                match: *
                model: gpt-6-luna
                context_window: 872000
            }
        ]
    }
]
```

### `listen`

`host` / `port`, default `127.0.0.1:60093`. Keep it on loopback: the URL token is the only access control.

### `providers[]`

| Key | Meaning |
| --- | --- |
| `name` | Referenced by profiles. |
| `type` | `codex` — ChatGPT subscription via OAuth (Responses API). `codex_api` — OpenAI-compatible Chat Completions with an API key, pay per token (`openai` is the old name and still works). `anthropic_compat` — an Anthropic-compatible endpoint such as z.ai, forwarded unconverted. |
| `base_url` | Endpoint. Optional for `codex`. |
| `api_key_env` | Take the key from this environment variable instead of the creds directory. |
| `effort_map` | `anthropic_compat` only: rewrite Claude Code's effort level (`low`, `medium`, `high`, `xhigh`, `max`) before forwarding, for providers with fewer levels. |

### `profiles[]`

| Key | Meaning |
| --- | --- |
| `name` | What you type: `ccgp <name>`. Not one of the command names (`serve`, `start`, `stop`, `status`, `login`, `models`, `config`, `import`, `export`, `help`, `version`). |
| `provider` | A provider `name`. |
| `models[]` | Ordered rules; the first whose `match` glob fits the requested Claude model wins. |
| `context_window` | Optional override of the window the launcher tells Claude Code about. |
| `compact_percent` | Optional 1-100: compact earlier than Claude Code's default. |
| `default_model` | Legacy; prefer a final `match: *` rule. |

A rule is `match` (glob on the Claude model name, e.g. `*opus*`), `model` (what the backend is asked for)
and `context_window` (that model's input window in tokens). Without any rule, the tiers map to `gpt-5` / `gpt-5-mini`.

## Context windows and compaction

Claude Code assumes a window per model; backends differ, so every rule states its own and compaction
has to trigger early enough. Claude Code accepts **one** window per process, therefore the launcher sets
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` to the `context_window` of the rule that serves the session's **main
model** (`--model`, `ANTHROPIC_MODEL`, `model` in `settings.json`, else `sonnet`).

A smaller window on another rule (say the Haiku model's 272000) does not shrink the main session, but
it is not covered either. A profile-level `context_window` overrides the choice, `compact_percent: 70`
compacts earlier still (`CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`), and variables you set yourself are never overridden.

## Generated model mappings

`ccgp models` prints what the backend offers and how Claude's tiers would map (read-only);
`ccgp models apply [profile]` writes it. It rewrites only the lines between the markers

```
## ccgp:managed-models begin <profile>
## ccgp:managed-models end <profile>
```

of that profile (added if missing; the profile's old `default_model`, `context_window` and `models`
are then replaced). Your comments and everything else stay. For `codex`, the mapping is also refreshed
after every fresh `ccgp login codex`. Details per provider: [providers.md](providers.md).

## Env-only mode

If there is no config file — ccgp does not create one when `PROXY_TOKEN` is set — a single implicit
profile `default` is built from the environment: `OPENAI_API_KEY`, `OPENAI_BASE_URL`, `PROXY_TOKEN`
(at least 32 characters), `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL`, `HOST`, `PORT`.
See [`.env.example`](../.env.example). Login flows, profiles and `ccgp models` need the config file.
