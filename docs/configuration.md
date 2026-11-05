# Configuration

ccgp is configured with one [Ktav](https://github.com/ktav-lang/golang) file. It is created on the
first run, from [`config.example.ktav`](../internal/config/config.example.ktav):

| OS | Config | Secrets and tokens | Server log |
| --- | --- | --- | --- |
| Windows | `%APPDATA%\cc-go-proxy\config.ktav` | `%APPDATA%\cc-go-proxy\creds\` | `%LOCALAPPDATA%\cc-go-proxy\logs\ccgp.log` |
| Linux / macOS | `~/.config/cc-go-proxy/config.ktav` | `~/.config/cc-go-proxy/creds/` | `~/.cache/cc-go-proxy/logs/ccgp.log` |

`ccgp --status` prints the real paths. `CCGP_CONFIG` points ccgp at another config file.

**Another place, all at once.** `ccgp --local` (a `.ccgp` directory in the current directory), `ccgp --home <dir>`,
`CCGP_HOME` or a `.ccgp` directory that already exists in the current directory move the config, `creds/`, the
Claude Code dirs (`claude/`) and the log (`logs/ccgp.log`) into one directory, so two setups share nothing
([details and order](cli.md#own-home-a-setup-that-shares-nothing)). A new home gets a port of its own.

The config is **yours**: ccgp reads it and changes it only on your command.

- `ccgp --profile add` / `rm` edit the text surgically (comments, key order and line endings, CRLF included, stay),
  validate the result with the real loader **before** replacing the file, and keep a backup
  `config.ktav.bak-<timestamp>` (with a `-N` suffix if the name is taken, so two edits in the same second never
  overwrite each other's backup).
- `ccgp --models apply`, and the refresh after a fresh `ccgp --login codex`, rewrite only the lines between a
  profile's `ccgp:managed-models` markers ([below](#generated-model-mappings)); no backup is made.
- `ccgp --connections active N total N proxy URL|off [provider NAME | profile NAME]` rewrites (or adds, or removes)
  the `connections.*` lines, or the `proxy` line of a provider or a profile, the same way, with a backup.
- `ccgp --peak <provider> set|add|rm|tz|clear` rewrites the provider's `peak` and `peak_tz` keys
  ([below](#peak-hours)) the same way, with a backup.
- `ccgp --interactive` ([full-screen interface](cli.md#interactive-mode)) makes the same kind of edit for one setting at a time:
  a profile's `default_model`, `context_window`, `compact_percent`, `proxy` or `provider`, a provider's `base_url`,
  `api_key_env`, `proxy` or peak hours, and the adding and removing of profiles.
- `ccgp --profile set|unset` and `ccgp --provider set|unset` rewrite one key of a profile or a provider the same way, with a backup.
- `ccgp --config reset` writes the default template and keeps the old file as a backup.

Everything machine-managed (profile URL tokens, API keys, the rotating OAuth tokens, and the pauses of
[peak hours](peak-hours.md)) lives in the separate `creds` directory, owner-only (Windows: explicit DACL, Unix: `0600`).
A running server picks up config edits without a restart; only a change of the listen address or `max_body_mb` needs `ccgp --stop`.

## Layout

Comments start with `##`. A single `#` is part of the text in Ktav, so `# peak: [` is not a comment but a key that
opens a list, and the file does not load; ccgp's own template uses `##` everywhere.

```
listen.host: 127.0.0.1
listen.port: 60093
listen.max_body_mb: 256

connections.active: 7
connections.total: 10
## connections.proxy: socks5://127.0.0.1:1080

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
                model: gpt-6.1-sol
                context_window: 922000
            }
            {
                match: *
                model: gpt-6.1-terra
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

`connections.active` / `connections.total`, default `7` / `10` (absent or `0`: the default): the connections ccgp
keeps to each backend. Active connections take new requests, each request goes to the least busy one, so parallel
sessions do not queue on one connection. Every connection takes new requests for 10 minutes and is then replaced,
the connections in turn, before backends cut it (connections to chatgpt.com were seen to drop after 30 minutes,
killing every request on them); the old one finishes its requests and closes with the last. `total` caps active plus
still finishing connections: at the limit a replacement waits, a running request is never cut. `total` must not be
below `active`. A running server applies a change on its next request. `ccgp --connections` shows the values,
`ccgp --connections active 7 total 10` sets them.

`connections.proxy` (optional, default none) sends every outbound connection of ccgp through a proxy: model requests,
the model catalog, `--login` and the token refresh, `--doctor` probes and `--update`. Forms:

| URL | |
| --- | --- |
| `socks5://[user:pass@]host:port` | SOCKS5; the user name and password (RFC 1929) are optional |
| `socks5h://…` | the same; the proxy always resolves the backend's name (ccgp sends names to a SOCKS5 proxy either way) |
| `http://[user:pass@]host:port` | HTTP `CONNECT` tunnel, Basic auth if a user is given |
| `https://…` | `CONNECT` over TLS to the proxy |

Without it ccgp follows `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` from the environment, like any Go program. The
connections to the proxy are renewed and warmed up like the direct ones. A change applies on the next request, with
no restart; `ccgp --connections proxy socks5://127.0.0.1:1080` sets it, `ccgp --connections proxy off` removes it.

**The proxy cascade: global, provider, profile.** `connections.proxy` is the global setting. A provider may override
it with `proxy:` in its object, and a profile with `proxy:` in its own. The nearest level that sets one wins, and it
**replaces** the levels above it whole (nothing is merged): a profile's proxy beats its provider's, the provider's
beats the global one, the global one beats the environment's.

| A level says | The requests of what it covers go |
| --- | --- |
| no `proxy` line | as the level above says: profile → its provider → `connections.proxy` → the environment's |
| `proxy: socks5://other:1080` | through that proxy, whatever the levels above say |
| `proxy: none` | directly: nothing above it applies, neither a proxy nor the environment's |

```
connections.proxy: socks5://10.0.0.2:1080       # everybody

providers: [
    {
        name: codex
        type: codex
        proxy: http://corp-proxy:3128      # every profile of codex goes through the corporate proxy ...
    }
    {
        name: zai
        type: anthropic_compat
        base_url: https://api.z.ai/api/anthropic
        proxy: none                        # ... every profile of zai goes out directly
    }
]

profiles: [
    {
        name: work
        provider: codex                    # no proxy line: the provider's, corp-proxy
    }
    {
        name: home
        provider: codex
        proxy: socks5://home-gw:1080       # its own beats the provider's
    }
    {
        name: lab
        provider: zai
        proxy: none                        # direct, as its provider
    }
]
```

`ccgp --connections proxy URL provider codex` sets a provider's proxy and `proxy URL profile work` a profile's;
`none` makes it direct and `inherit` removes the line, so the level above applies again. `ccgp --connections` lists
the providers and profiles that have their own, `--json` also gives the level each profile's proxy comes from
(`profile`, `provider`, `global` or empty for the environment's). The interface (`ccgp -i`) edits both: *Providers →
a provider → proxy* and *Profiles → a profile → proxy*; a profile's screen says where its proxy comes from. A provider
of type `claude` (the real Claude Code) and its profiles have no proxy setting.

Each way out has its own set of connections (up to `connections.active` / `total` each). Things that belong to a
provider, not to a request, use the proxy that all the profiles of the provider agree on, and the provider's own
(else the global) one when they differ or it has no profile: `--login`, the model catalog of `--models`, the backend
probe of `--doctor`. `--update` uses the global proxy. A provider made by `ccgp --profile add NAME --like P` has the
proxy of `P`'s provider. The password lives in `config.ktav` (every ccgp write of it is owner-only); `ccgp --connections`
and the server log show it as `***`.

### `providers[]`

| Key | Meaning |
| --- | --- |
| `name` | Referenced by profiles. |
| `type` | `codex` — ChatGPT subscription via OAuth (Responses API). `codex_api` — OpenAI-compatible Chat Completions with an API key, pay per token (`openai` is the old name and still works). `anthropic_compat` — an Anthropic-compatible endpoint such as z.ai, forwarded unconverted. `claude` — the real Claude Code with a login of its own, nothing goes through ccgp (see below). |
| `base_url` | Endpoint. Optional for `codex`; not allowed for `claude`. |
| `api_key_env` | Take the key from this environment variable instead of the creds directory. |
| `effort_map` | `anthropic_compat` only: rewrite Claude Code's effort level (`low`, `medium`, `high`, `xhigh`, `max`) before forwarding, for providers with fewer levels. |
| `peak`, `peak_tz` | Peak hours: when the provider's requests are refused ([below](#peak-hours)). Not allowed for `claude`. |
| `proxy` | A proxy URL, or `none`, for every profile of the provider; a profile's own `proxy` wins, `connections.proxy` is replaced ([the cascade](#layout)). Not allowed for `claude`. |

### Peak hours

A provider may list the hours in which its profiles' requests are refused (full description: [peak hours](peak-hours.md)):

```
{
    name: zai
    type: anthropic_compat
    base_url: https://api.z.ai/api/anthropic
    peak_tz: Asia/Shanghai
    peak: [
        {
            days: mon-fri
            from: 14:00
            to: 18:00
        }
        {
            from: 20:00
            to: 23:00
        }
    ]
}
```

| Key | Meaning |
| --- | --- |
| `peak[].from`, `peak[].to` | `HH:MM`, 24 hours; `to` is exclusive and may be `24:00`; a `to` that is not after `from` runs past midnight. Both required |
| `peak[].days` | `mon`, `sat,sun`, `mon-fri`, `fri-mon` (wraps the week), `mon-wed,fri`. Absent: every day |
| `peak_tz` | The zone the hours are given in: a name (`Asia/Shanghai`, `America/New_York`), an offset written with sign and colon (`+08:00`) or `UTC`. Absent: the machine's zone. Needs `peak` |

In the hours a request to `/v1/messages` is answered with an HTTP 400 that says when requests resume; nothing reaches
the provider. `ccgp --peak` edits these keys (and shows the state), `ccgp --peak <provider> pause [LEN]` lets requests
through for a while; the pause is kept in `creds/peak-pause.ktav`, not in this file. The loader names the provider
and the window in an error, and a running server keeps the previous settings until the file is right.

### `profiles[]`

| Key | Meaning |
| --- | --- |
| `name` | What you type: `ccgp <name>`. Not one of the [reserved names](#reserved-profile-names). |
| `provider` | A provider `name`. |
| `models[]` | Ordered rules; the first whose `match` glob fits the requested Claude model wins. |
| `context_window` | Optional override of the window the launcher tells Claude Code about. |
| `compact_percent` | Optional 1-100: compact earlier than Claude Code's default. |
| `proxy` | Optional: a proxy URL for this profile, or `none` for a direct connection; without it the proxy of its provider applies, else the global `connections.proxy` ([the cascade](#layout)). |
| `default_model` | Legacy; prefer a final `match: *` rule. |

A rule is `match` (glob on the Claude model name, e.g. `*opus*`), `model` (what the backend is asked for)
and `context_window` (that model's input window in tokens). Without any rule, the tiers map to `gpt-5` / `gpt-5-mini`.

## Creating and removing profiles

ccgp can edit the config for you; every change is surgical, validated and backed up (see above):

```sh
ccgp --profile add codex2 --like codex                       # new login + profile, copying the source profile
ccgp --profile add codex-alt --provider codex                # new profile on the existing codex login
ccgp --profile add myai --type anthropic_compat --base-url https://example.internal/api --model my-model
ccgp --profile add work --type claude                        # the real Claude Code, its own clean dir and login
ccgp --profile list
ccgp --profile rm codex2 [--yes] [--purge]
```

- A **login belongs to a provider** (its `oauth-<provider>.ktav` / `apikey-<provider>.ktav` in the `creds`
  directory); a **profile** has its URL token, model rules and an isolated Claude Code dir. A second ChatGPT
  account is a new provider + new profile; another mapping for the same account is a new profile on the
  existing provider.
- `--like <profile>` creates a new provider (its own login) and a profile named after it, copying the source
  profile's provider type, `base_url`, `effort_map`, [peak hours](peak-hours.md) and `proxy` (not `api_key_env`) and
  its model rules, managed markers included. Without a name the next free `<source>N` is used (`codex` -> `codex2`); the provider gets the same name.
- `--provider <provider>` adds a profile to an existing login. With `--model M` it gets one catch-all rule
  (`--window N` sets the window, default 272000 — the command says so); without, the rules of the provider's
  first profile (by name) are copied. If the provider has no profile yet, the new one gets no rules and the
  command tells you to run `ccgp --models apply <name>` (codex and preset providers) or to add a `models` list.
- `--type <codex|codex_api|anthropic_compat|claude>` [--base-url URL] creates a new backend (OpenRouter, Ollama, any
  Anthropic-compatible API). Without `--model`, a `codex` provider or one whose URL has a built-in preset gets
  an empty managed block, ready for `ccgp --models apply <name>`.
- `--profile add` never signs in; it prints `run: ccgp <name>` — the launcher signs in on first start.
  It refuses reserved, invalid or already-used names and unknown sources, and needs the config file
  (env-only mode has a single implicit profile).
- `--profile list` shows one row per profile: PROFILE, PROVIDER, TYPE, LOGIN, MAIN MODEL (what the `sonnet` tier
  resolves to) and CLAUDE DIR (yes/no).
- `--profile rm` asks `remove profile "<name>"? [y/N]` unless `--yes`, removes the profile and — if no other
  profile uses it — its provider, and drops the profile's URL token. It refuses to remove the last profile.
  Without `--purge` it lists what stays behind (the isolated Claude dir, and the provider's credentials if the
  provider was removed); `--purge` deletes `<config dir>/cc-go-proxy/claude/<name>` and, if the provider was
  removed, its credentials.

### Profiles of the real Claude Code (`--type claude`)

`ccgp --profile add work --type claude` (and `personal`, `team`, … as many as you need) makes a profile that
does **not** use the proxy: `ccgp work` starts no server, needs no token and sets no `ANTHROPIC_BASE_URL`; it runs
`claude` with `CLAUDE_CONFIG_DIR=<home>/claude/work`. The point is a clean, separate Claude Code per profile, for
example one per Anthropic account:

- The directory is created from nothing. Copied from your own Claude Code at every launch, one way: `CLAUDE.md`,
  `keybindings.json`, `settings.json` (without `apiKeyHelper`, `forceLogin*`, cloud-auth entries and the credential
  variables in `env`), and the `agents`, `commands`, `skills`, `output-styles` and `plugins` folders.
- **Never copied:** `.claude.json` (so no account, `userID`, project list or MCP server settings), `.credentials.json`,
  sessions and projects, history, todos, anything else. A first launch does not seed a session either (unlike the
  proxy profiles); move one on purpose with [`ccgp --send`](sessions.md) if you want it.
- The variables of the parent that choose an endpoint or a credential (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
  `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_OAUTH_TOKEN`, Bedrock, Vertex, Foundry) are dropped, so the profile can only use
  the account you sign in with.
- You sign in yourself: the first `ccgp work` shows Claude Code's own sign-in, or type `/login`. The login lives in the
  profile's directory only; ccgp never reads, copies or stores it, and a login made there is kept between launches.
  `--profile list` shows `signed in inside Claude Code` once the directory holds one.
- It takes no `base_url`, `models`, `default_model`, `context_window`, `compact_percent`, `proxy` or `peak` (the config
  loader rejects them), and `--model`, `--window`, `--base-url`, `--models apply`, `--which`, `--login`, `--peak` and
  `--connections proxy … profile` do not apply to it.
- `--profile rm work` keeps the directory (it holds the sign-in) unless `--purge` is given.

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
