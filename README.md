> **ccgp is a fork of [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy)** (MIT). Upstream is a general proxy that puts OpenAI-compatible APIs behind Claude Code; ccgp keeps that translation core and turns it into a headless launcher + proxy for daily use: `ccgp <profile>` runs Claude Code on your ChatGPT Codex subscription, an OpenAI-compatible API or z.ai GLM, with token-protected URLs, per-profile model mapping and context windows, and Claude credentials kept out of the session. [How it differs](#how-ccgp-differs-from-upstream)

# ccgp

[![CI](https://github.com/PHPCraftdream/cc-go-proxy/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/PHPCraftdream/cc-go-proxy/actions/workflows/ci.yml)
[![Licenses](https://github.com/PHPCraftdream/cc-go-proxy/actions/workflows/licenses.yml/badge.svg?branch=main)](https://github.com/PHPCraftdream/cc-go-proxy/actions/workflows/licenses.yml)
[![Release](https://img.shields.io/github/v/release/PHPCraftdream/cc-go-proxy?display_name=tag&sort=semver)](https://github.com/PHPCraftdream/cc-go-proxy/releases)
[![npm](https://img.shields.io/npm/v/@cc-go-proxy/ccgp?logo=npm)](https://www.npmjs.com/package/@cc-go-proxy/ccgp)
[![Go version](https://img.shields.io/github/go-mod/go-version/PHPCraftdream/cc-go-proxy)](go.mod)
[![License: MIT OR Apache-2.0](https://img.shields.io/badge/license-MIT%20OR%20Apache--2.0-blue)](#license)
[![Platforms](https://img.shields.io/badge/platforms-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey)](#install)
[![Go Report Card](https://goreportcard.com/badge/github.com/PHPCraftdream/cc-go-proxy)](https://goreportcard.com/report/github.com/PHPCraftdream/cc-go-proxy)

Run [Claude Code](https://claude.com/claude-code) on other models. ccgp is one small Go binary: a local proxy that speaks
Claude's API to Claude Code and the backend's API to the other side, and a launcher that wires the two together.

```sh
ccgp codex                     # Claude Code on your ChatGPT subscription (Codex)
ccgp zai                       # Claude Code on z.ai GLM
ccgp codex -p "explain this"   # headless: everything after the profile goes to claude
ccgpy codex                    # same, with every permission prompt off (--dangerously-skip-permissions)
```

## Features

- **Launcher and proxy in one.** `ccgp <profile>` starts the background server if needed, makes sure you are signed in,
  then runs `claude` through it. No service to install, no env files to juggle.
- **Backends.** ChatGPT Codex with a subscription (OAuth, refresh-token rotation, browser or device-code sign-in),
  OpenAI-compatible APIs with a key (OpenAI, OpenRouter, Ollama, …), and Anthropic-compatible endpoints such as z.ai.
- **Named profiles.** Each profile has its own URL token and its own provider; Claude's `fable` / `opus` / `sonnet` /
  `haiku` map to the models you choose, with the real context window of each, so compaction fires at the right time.
  Several logins are supported: `ccgp --profile add` clones a profile with its own login (a second ChatGPT account),
  adds a profile to an existing login, or wires up a new backend. `--type claude` makes a profile of the real Claude
  Code (no proxy): a clean config directory of its own and its own sign-in, so you can keep several Claude accounts
  ([details](docs/configuration.md#profiles-of-the-real-claude-code---type-claude)). `--type claude_api` is the same with
  an API key of the Claude Platform of its own and a server you can set, the official one by default
  ([details](docs/configuration.md#profiles-with-an-api-key---type-claude_api)).
- **Locked down.** Every request needs `/call/<token>/…`; the server listens on loopback; secrets are owner-only on disk.
- **Your Claude credentials stay out.** Each profile runs Claude Code in its own config directory with your agents,
  skills, commands and plugins copied in, but never an official login or API key.
- **Sessions travel.** `ccgp --send` (aliases `ccgp --import` / `ccgp --export`) copy the latest sessions of the
  current directory between Claude Code and a profile, or between two profiles; a session continued on both sides is
  kept as a second one (`--fork`) or replaced with the old file kept (`--force`).
- **Separate setups.** `ccgp --local` / `ccgp --home <dir>` (or a `.ccgp` directory in the project) keep the config,
  logins, tokens, Claude dirs and the log in one directory that shares nothing with the default setup
  ([details](docs/cli.md#own-home-a-setup-that-shares-nothing)).
- **Through a proxy.** SOCKS5, HTTP `CONNECT` and HTTPS proxies for every outbound connection (models, sign-in, token
  refresh, update), global, per provider or per profile (the nearest level replaces the ones above it), with `none`
  for a direct connection; without a setting the `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` environment applies
  ([details](docs/configuration.md#layout)).
- **Peak hours.** Give a provider its peak hours (`ccgp --peak zai set mon-fri 14:00-18:00`, in the provider's zone)
  and ccgp refuses its requests then, with a notice in the chat that says when work can go on; `pause` lets you
  through for a while. The provider never sees the refused request ([details](docs/peak-hours.md)).
- **Interactive setup.** `ccgp --interactive` (or `-i`) is a full-screen interface ([Bubble Tea](https://github.com/charmbracelet/bubbletea))
  to do everything the commands do without remembering them: profiles and providers (settings, proxy, sign-in), peak
  hours, connections, models, sessions, the server. The commands and the interface cover each other both ways, and a
  test keeps it so ([details](docs/cli.md#interactive-mode)).
- **Plain Ktav config**, created on first run, hot-reloaded. ccgp changes it only on your command (`--profile`,
  `--connections`, `--peak`, `--models apply`, the interactive interface), and every change is validated with the
  real loader and keeps a backup first ([details](docs/configuration.md)).
- **Windows first**, also Linux and macOS; Windows is tested in CI.

## Install

Linux and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/PHPCraftdream/cc-go-proxy/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/PHPCraftdream/cc-go-proxy/main/install.ps1 | iex
```

The scripts download the archive of the latest [release](https://github.com/PHPCraftdream/cc-go-proxy/releases) for your
OS and CPU, check it against `SHA256SUMS` and install `ccgp` and `ccgpy` (Linux and macOS: `~/.local/bin`; Windows:
`%LOCALAPPDATA%\Programs\ccgp`, added to your user `PATH`). Set `CCGP_VERSION` to pick a version and
`CCGP_INSTALL_DIR` to pick the directory; the scripts are short, read them first if you like:
[install.sh](install.sh), [install.ps1](install.ps1).

Platforms of the release archives: Linux (x86-64, arm64, 32-bit ARM from the Raspberry Pi 1 on, riscv64), macOS
(Intel, Apple silicon), Windows (x86-64, arm64) and FreeBSD (x86-64, arm64). The binaries are static and need no
libc, so the same Linux build runs on glibc and on musl systems (Alpine); CI runs it on Alpine, Debian 11 and
Ubuntu 20.04 and on arm64 and ARM. It carries a fallback set of root certificates, used only when the system has none
(minimal containers), so HTTPS works there too. The npm package covers the six most common platforms (Linux,
macOS and Windows on x86-64 and arm64); on the others use the archive or `install.sh`.

With npm (any platform, Node.js 18 or newer):

```sh
npm install -g @cc-go-proxy/ccgp
```

The package [`@cc-go-proxy/ccgp`](https://www.npmjs.com/package/@cc-go-proxy/ccgp) (in the npm organization
[`cc-go-proxy`](https://www.npmjs.com/org/cc-go-proxy)) installs the same two commands, `ccgp` and `ccgpy`, from a prebuilt binary for your
platform (no install scripts run). Update it with npm, not with `ccgp --update`.

You also need Claude Code (`claude` on `PATH`). ccgp is one self-contained binary: the
[Ktav](https://github.com/ktav-lang/golang) config parser is pure Go, nothing is downloaded on first use.

## Quick start

```sh
ccgp codex
```

On the first run ccgp:

1. creates a default [`config.ktav`](internal/config/config.example.ktav) with `codex`, `codex_api` and `zai` profiles
   (`%APPDATA%\cc-go-proxy\` on Windows, `~/.config/cc-go-proxy/` elsewhere);
2. starts the proxy in the background (`127.0.0.1:60093`);
3. prints a sign-in link for Codex (the browser is never opened for you) or asks for an API key;
4. generates the profile's URL token;
5. runs `claude` with `ANTHROPIC_BASE_URL=http://127.0.0.1:60093/call/<token>`.

For z.ai: `ccgp --login zai` once (asks for your API key), then `ccgp zai`.

A second ChatGPT account: `ccgp --profile add codex2 --like codex`, then `ccgp codex2` (signs in on first start).
Details: [profiles](docs/configuration.md#creating-and-removing-profiles).

Several Claude accounts, each clean and separate (no proxy, nothing of your own login, ids or sessions copied):
`ccgp --profile add work --type claude`, then `ccgp work` and `/login` inside. Details:
[profiles of the real Claude Code](docs/configuration.md#profiles-of-the-real-claude-code---type-claude).

A Claude profile on the Claude Platform with an API key of its own, next to the others: `ccgp --profile add api --type
claude_api`, `ccgp --login api` (the key), then `ccgp api`. The server is the official one; another:
`--base-url https://…` or `ccgp --provider set api base_url https://…`. Details:
[profiles with an API key](docs/configuration.md#profiles-with-an-api-key---type-claude_api).

Bring a conversation along: `cd` into the project and `ccgp --import codex` (your Claude Code's latest sessions of this
directory into profile `codex`), then `ccgp codex -c`. A proxy profile that has none of this directory's sessions gets
the latest one by itself on its first start. Details: [sessions](docs/sessions.md).

A setup of its own that shares nothing with the default one: `ccgp --local --login codex` (everything in `./.ccgp`),
`ccgp --home ~/work/ccgp work`. Details: [own home](docs/cli.md#own-home-a-setup-that-shares-nothing).

Prefer menus to commands? `ccgp -i` opens a full-screen interface that lists, edits and adds profiles and providers
and edits their peak hours ([interactive mode](docs/cli.md#interactive-mode)).

Keep a provider's expensive hours free of work: `ccgp --peak zai set mon-fri 14:00-18:00` and
`ccgp --peak zai tz Asia/Shanghai`; in those hours its profiles answer with a notice instead of working, and
`ccgp --peak zai pause 2h` is the way past it. Details: [peak hours](docs/peak-hours.md).

Check everything: `ccgp --doctor`; look at the server: `ccgp --status`, `ccgp --logs -f`; where a model goes:
`ccgp --which codex claude-opus-4-1`.

Behind a proxy: `ccgp --connections proxy socks5://127.0.0.1:1080` (or `http://user:pass@host:3128`) for everything,
`ccgp --connections proxy http://corp:3128 provider codex` for all profiles of one provider (it replaces the global
one), `... profile work` for one profile (it replaces both), `proxy none profile home` to bypass them,
`ccgp --connections proxy off` to remove the global one. A change applies on the next request.

## Commands

Commands start with `--`; a word without dashes is always a profile (`ccgp status` would run a profile called
`status`). Per command: `ccgp --<command> --help`. Details: [CLI reference](docs/cli.md).

| Command | Purpose |
| --- | --- |
| `ccgp <profile> [claude args…]` | Ensure server and credentials, run `claude` through the profile |
| `ccgp --local` / `--home <dir>` `…` | First flags of any command or profile run: keep config, logins, tokens and log in `./.ccgp` or in `<dir>` instead of the default places; a `.ccgp` directory in the current directory is picked up by itself ([own home](docs/cli.md#own-home-a-setup-that-shares-nothing)) |
| `ccgpy <profile> [claude args…]` | Same as `ccgp <profile> --dangerously-skip-permissions …`: Claude Code without permission prompts, so use it only where that is acceptable |
| `ccgp --login <provider> [--device]` | (Re)authenticate. Codex: browser OAuth, or `--device` for a device code. Others: API key |
| `ccgp --logout <provider> [--yes]` | Remove the stored login (OAuth tokens or API key) of a provider; the config is not touched |
| `ccgp --models [apply] [-v] [--json] [profile]` | Show, or write into the config, the newest models per Claude tier |
| `ccgp --which <profile> <model> [--json]` | Show where a Claude model name is routed in a profile, offline |
| `ccgp --send <from> <to> [count] [--fork\|--force]` | Copy the latest sessions (default 10) of the current directory between two profiles or `claude` (your own Claude Code). A session continued on both sides is skipped; `--fork` keeps both versions, `--force` replaces the destination (the old file is kept) |
| `ccgp --import <profile> [count] [--fork\|--force]` | Alias of `ccgp --send claude <profile>` |
| `ccgp --export <profile> [count] [--fork\|--force]` | Alias of `ccgp --send <profile> claude` |
| `ccgp --profile add\|list\|rm\|set\|unset` | Manage profiles (and their providers) in the config: new logins (`--like`), profiles on an existing login (`--provider`), new backends (`--type codex\|codex_api\|anthropic_compat`), profiles of the real Claude Code with their own account (`--type claude`) or API key (`--type claude_api`); `set` / `unset` change one setting of a profile (`default_model`, `context_window`, `compact_percent`, `provider`) |
| `ccgp --provider [NAME] [--json]` / `set\|unset NAME KEY [VALUE]` | Show the providers, or one in detail (base URL, key variable, login, proxy, peak hours, profiles); set or remove its `base_url` or `api_key_env` |
| `ccgp --start` / `--stop` / `--restart` | Manage the background server |
| `ccgp --status [--json]` | Show the server, paths and login state |
| `ccgp --logs [-f] [-n N]` | Show the tail of the background server log, `-f` follows it |
| `ccgp --doctor [--offline] [--json]` | Check `claude`, config, server, logins, backend reachability and permissions |
| `ccgp --serve [-d] [-s]` | Run the server in the foreground (`-d` dumps bodies, `-s` logs requests) |
| `ccgp --config reset` | Back up the config and write the default one |
| `ccgp --connections [active N] [total N] [proxy URL\|off [provider NAME \| profile NAME]]` | Show or set the connections kept to each backend (default 7 active, 10 total) and the SOCKS5 / HTTP proxy: global, with a different one or none per provider or per profile (the nearest level wins) |
| `ccgp --interactive` / `-i` | Full-screen interface that does everything the commands do: profiles, providers (settings, proxy, sign-in), peak hours, connections, models, sessions, the server; needs a terminal, only the flag starts it. The commands and the interface cover each other, and a test keeps it so ([interactive mode](docs/cli.md#interactive-mode)) |
| `ccgp --peak [provider [set\|add WINDOW...\|rm N...\|tz ZONE\|clear\|pause [LEN]\|resume]] [--json]` | Show or set the peak hours of a provider: in them its profiles' requests are refused with a notice in the chat (`mon-fri 14:00-18:00`, in the provider's zone); `pause` works anyway for a while ([peak hours](docs/peak-hours.md)) |
| `ccgp --update [--check]` | Update ccgp and ccgpy from the latest GitHub release (checksum verified) |
| `ccgp --completion bash\|zsh\|powershell` | Print a shell completion script |
| `ccgp --version` | Print the version |

## Model label metadata

`GET /call/<token>/model?requested=<model-id>` resolves a request alias with the same `MapModel` used for inference. The normal profile-token authentication and hot-reloaded profile configuration apply; no upstream inference or login request is made by this handler.

The response contains only `kind: "ccgp:model-route:v1"`, `requestedModel`, and `backendModel`, with `Cache-Control: no-store`. It reports the configured route, not the model ultimately selected behind an upstream alias. Clients must fall back to their original labels when the endpoint is unavailable. This also supports `ccgpy`, which uses the same launcher and proxy.

[cah (cc-arch-hands)](https://github.com/PHPCraftdream/cc-arch-hands) supports ccgp's model mappings: its status line,
chat stamp and context gauge show the model a request is really routed to (`gpt-6.1-sol`, `gpt-6.1-terra`,
`gpt-6.1-luna`, `gpt-6.1-astra`, `glm-5.3`, …) by ccgp's own mapping rules, not the Claude alias Claude Code sent.

## How ccgp differs from upstream

| | [claude-code-proxy](https://github.com/nielspeter/claude-code-proxy) (upstream) | ccgp |
| --- | --- | --- |
| Shape | A proxy you configure with env vars and point Claude Code at | A launcher + proxy: `ccgp <profile>` does the whole run |
| Backends | OpenAI-compatible Chat Completions (OpenRouter, OpenAI, Ollama) | Those, plus ChatGPT Codex via subscription OAuth (Responses API) and Anthropic-compatible endpoints (z.ai) |
| Routing and access | One backend (`OPENAI_BASE_URL`) per proxy | Several providers; named profiles, each with its own URL token and its own provider |
| Config | `.env` and environment | Ktav `providers[]` / `profiles[]`, per-rule model mapping, generated mappings (`ccgp --models`), hot reload |
| Context | — | Per-model context windows; compaction window set for the session's main model |
| Claude credentials | Not addressed | Never reach a ccgp session: isolated config directory per profile |
| Sessions | — | `ccgp --send` copies the current directory's latest sessions between profiles (and your own Claude Code via `--import` / `--export`) |
| Secrets | `.env` | Separate owner-only credentials directory; OAuth refresh tokens rotate safely |
| Platforms | Builds for macOS, Linux and Windows | Windows first (owner-only ACLs, detached background process, tested in CI), also Linux and macOS |
| Distribution | Source builds and release binaries | Binaries for 10 platforms with provenance attestations, install scripts, npm package |

The Anthropic ↔ OpenAI Chat Completions translation, streaming and adaptive per-model capability detection are
upstream's work, kept here for `codex_api`. Thank you, Niels Peter Strandberg and contributors.

## Documentation

| | |
| --- | --- |
| [CLI reference](docs/cli.md) | Every command with examples: running a profile, own home, server and log, `--json`, `--doctor`, `--update`, completion, environment variables, exit codes |
| [HTTP API](docs/api.md) | `/health` and `/call/<token>/…`: the endpoints, tokens, errors, `curl` examples |
| [Configuration](docs/configuration.md) | Config file, providers, profiles (`--profile add` / `rm`), model rules, context windows |
| [Providers](docs/providers.md) | Codex, API keys, z.ai, the real Claude Code: sign-in, model mapping, effort levels |
| [Peak hours](docs/peak-hours.md) | Refuse a provider's requests at set hours: windows, zones, what Claude Code shows, `ccgp --peak`, pause |
| [Sessions](docs/sessions.md) | `ccgp --send`, `--import` / `--export`, thinking blocks |
| [Security](docs/security.md) | Token URLs, secrets at rest, why official credentials never reach a session |
| [Architecture](docs/architecture.md) | Layout, request flow, invariants |
| [Releasing](docs/releasing.md) | How releases, npm (token and Trusted Publishing) and license checks work |
| [Changelog](CHANGELOG.md) | What changed |

## Terms and disclaimer

ccgp is an independent project, not affiliated with or endorsed by Anthropic, OpenAI or z.ai; product names belong to
their owners. The Codex sign-in uses the private backend of OpenAI's own clients, and using a ChatGPT subscription
through a third-party client is subject to OpenAI's terms and may stop working at any time. ccgp never reads, forwards
or stores a Claude subscription's credentials. You are responsible for complying with the terms of every service you
connect. The software is provided as is, without warranty (see the licenses).

## Development

```sh
make build        # ./ccgp
go vet ./...
go test ./...
node --test "scripts/npm/*.test.mjs"   # npm packaging
```

CI runs tests on Linux and Windows, golangci-lint, a license allowlist, and a type-check of all six release targets.
See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md).

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE))
- MIT license ([LICENSE-MIT](LICENSE-MIT))

at your option. Parts derived from [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy)
remain available under the MIT license with their copyright notice ([NOTICE](NOTICE)).

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in ccgp by you, as defined
in the Apache-2.0 license, shall be dual licensed as above, without any additional terms or conditions.
