# Configuration

ccgp is configured with one [Ktav](https://github.com/ktav-lang/golang) file. It is created on the
first run, from [`config.example.ktav`](../internal/config/config.example.ktav):

| OS | Config | Secrets and tokens | Server log |
| --- | --- | --- | --- |
| Windows | `%APPDATA%\cc-go-proxy\config.ktav` | `%APPDATA%\cc-go-proxy\creds\` | `%LOCALAPPDATA%\cc-go-proxy\logs\ccgp.log` |
| Linux / macOS | `~/.config/cc-go-proxy/config.ktav` | `~/.config/cc-go-proxy/creds/` | `~/.cache/cc-go-proxy/logs/ccgp.log` |

`ccgp --status` prints the real paths. `CCGP_CONFIG` points ccgp at another config file.

The config is **yours**: ccgp reads it and changes it only on your command.

- `ccgp --profile add` / `rm` edit the text surgically (comments, key order and line endings, CRLF included, stay),
  validate the result with the real loader **before** replacing the file, and keep a backup
  `config.ktav.bak-<timestamp>` (with a `-N` suffix if the name is taken, so two edits in the same second never
  overwrite each other's backup).
- `ccgp --models apply`, and the refresh after a fresh `ccgp --login codex`, rewrite only the lines between a
  profile's `ccgp:managed-models` markers ([below](#generated-model-mappings)); no backup is made.
- `ccgp --config reset` writes the default template and keeps the old file as a backup.

Everything machine-managed (profile URL tokens, API keys, the rotating OAuth tokens) lives in the
separate `creds` directory, owner-only (Windows: explicit DACL, Unix: `0600`).
A running server picks up config edits without a restart; only a change of the listen address or `max_body_mb` needs `ccgp --stop`.

## Layout

```
listen.host: 127.0.0.1
listen.port: 60093
listen.max_body_mb: 256

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
                context_window: 922000
            }
            {
                match: *
                model: gpt-6-luna
                context_window: 922000
            }
        ]
    }
]
```

### `listen`

`host` / `port`, default `127.0.0.1:60093`. Keep it on loopback: the URL token is the only access control.

`max_body_mb` is the largest request body in MiB, default `256` (an absent key means the default; `0` too). Claude Code resends the
whole conversation on every call, and the reasoning signatures of a long session alone take megabytes, so a small cap
cuts live sessions off with a connection reset. A larger body is answered with `413` and a `rejected` line in the log.
Changing it needs `ccgp --stop` (like the listen address).

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
| `name` | What you type: `ccgp <name>`. Not one of the [reserved names](#reserved-profile-names). |
| `provider` | A provider `name`. |
| `models[]` | Ordered rules; the first whose `match` glob fits the requested Claude model wins. |
| `context_window` | Optional override of the window the launcher tells Claude Code about. |
| `compact_percent` | Optional 1-100: compact earlier than Claude Code's default. |
| `default_model` | Legacy; prefer a final `match: *` rule. |

A rule is `match` (glob on the Claude model name, e.g. `*opus*`), `model` (what the backend is asked for)
and `context_window` (that model's input window in tokens). Without any rule, the tiers map to `gpt-5` / `gpt-5-mini`.

## Creating and removing profiles

ccgp can edit the config for you; every change is surgical, validated and backed up (see above):

```sh
ccgp --profile add codex2 --like codex                       # new login + profile, copying the source profile
ccgp --profile add codex-alt --provider codex                # new profile on the existing codex login
ccgp --profile add myai --type anthropic_compat --base-url https://example.internal/api --model my-model
ccgp --profile list
ccgp --profile rm codex2 [--yes] [--purge]
```

- A **login belongs to a provider** (its `oauth-<provider>.ktav` / `apikey-<provider>.ktav` in the `creds`
  directory); a **profile** has its URL token, model rules and an isolated Claude Code dir. A second ChatGPT
  account is a new provider + new profile; another mapping for the same account is a new profile on the
  existing provider.
- `--like <profile>` creates a new provider (its own login) and a profile named after it, copying the source
  profile's provider type, `base_url` and `effort_map` (not `api_key_env`) and its model rules, managed markers
  included. Without a name the next free `<source>N` is used (`codex` -> `codex2`); the provider gets the same name.
- `--provider <provider>` adds a profile to an existing login. With `--model M` it gets one catch-all rule
  (`--window N` sets the window, default 272000 — the command says so); without, the rules of the provider's
  first profile (by name) are copied. If the provider has no profile yet, the new one gets no rules and the
  command tells you to run `ccgp --models apply <name>` (codex and preset providers) or to add a `models` list.
- `--type <codex|codex_api|anthropic_compat>` [--base-url URL] creates a new backend (OpenRouter, Ollama, any
  Anthropic-compatible API). Without `--model`, a `codex` provider or one whose URL has a built-in preset gets
  an empty managed block, ready for `ccgp --models apply <name>`.
- `profile add` never signs in; it prints `run: ccgp <name>` — the launcher signs in on first start.
  It refuses reserved, invalid or already-used names and unknown sources, and needs the config file
  (env-only mode has a single implicit profile).
- `profile list` shows one row per profile: PROFILE, PROVIDER, TYPE, LOGIN, MAIN MODEL (what the `sonnet` tier
  resolves to) and CLAUDE DIR (yes/no).
- `profile rm` asks `remove profile "<name>"? [y/N]` unless `--yes`, removes the profile and — if no other
  profile uses it — its provider, and drops the profile's URL token. It refuses to remove the last profile.
  Without `--purge` it lists what stays behind (the isolated Claude dir, and the provider's credentials if the
  provider was removed); `--purge` deletes `<config dir>/cc-go-proxy/claude/<name>` and, if the provider was
  removed, its credentials.

### Reserved profile names

Only `claude` is reserved: it is the endpoint name of your own Claude Code directory in `ccgp --send claude <profile>`.
Commands start with `--`, so a profile may be called `status`, `login` or `serve`. `ccgp --profile add` applies the
same rule to the provider it creates, which is named like its profile.

## Context windows and compaction

Claude Code assumes a window per model; backends differ, so every rule states its own and compaction
has to trigger early enough. Claude Code accepts **one** window per process, therefore the launcher sets
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` to the `context_window` of the rule that serves the session's **main
model** (`--model`, `ANTHROPIC_MODEL`, `model` in `settings.json`, else `sonnet`).

Claude Code accepts 100k to 1M for this variable and prints a warning for anything else, so ccgp clamps the value
into that range. A backend model with a window below 100k therefore cannot be protected by compaction.

A smaller window on another rule (say the Haiku model's 272000) does not shrink the main session, but
it is not covered either. A profile-level `context_window` overrides the choice, `compact_percent: 70`
compacts earlier still (`CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`), and variables you set yourself are never overridden.

## Generated model mappings

`ccgp --models` prints what the backend offers and how Claude's tiers would map (read-only);
`ccgp --models apply [profile]` writes it. It rewrites only the lines between the markers

```
## ccgp:managed-models begin <profile>
## ccgp:managed-models end <profile>
```

of that profile (added if missing; the profile's old `default_model`, `context_window` and `models`
are then replaced). Your comments and everything else stay. For `codex`, the mapping is also refreshed
after every fresh `ccgp --login codex`. Details per provider: [providers.md](providers.md).

## Env-only mode

If there is no config file — ccgp does not create one when `PROXY_TOKEN` is set — a single implicit
profile `default` is built from the environment: `OPENAI_API_KEY`, `OPENAI_BASE_URL`, `PROXY_TOKEN`
(at least 32 characters), `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL`, `HOST`, `PORT`.
See [`.env.example`](../.env.example). Login flows, profiles and `ccgp --models` need the config file.
