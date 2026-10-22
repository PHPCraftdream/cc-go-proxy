> **ccgp is a fork of [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy)** (MIT). Upstream is a general proxy that puts OpenAI-compatible APIs behind Claude Code; ccgp keeps that translation core and turns it into a headless launcher + proxy for daily use: `ccgp <profile>` runs Claude Code on your ChatGPT Codex subscription, an OpenAI-compatible API or z.ai GLM, with token-protected URLs, per-profile model mapping and context windows, and Claude credentials kept out of the session. [How it differs](#how-ccgp-differs-from-upstream)

# ccgp

[![CI](https://github.com/PHPCraftdream/cc-go-proxy/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/PHPCraftdream/cc-go-proxy/actions/workflows/ci.yml)
[![Licenses](https://github.com/PHPCraftdream/cc-go-proxy/actions/workflows/licenses.yml/badge.svg?branch=main)](https://github.com/PHPCraftdream/cc-go-proxy/actions/workflows/licenses.yml)
[![Release](https://img.shields.io/github/v/release/PHPCraftdream/cc-go-proxy?display_name=tag&sort=semver)](https://github.com/PHPCraftdream/cc-go-proxy/releases)
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
  Several logins are supported: `ccgp profile add` clones a profile with its own login (a second ChatGPT account),
  adds a profile to an existing login, or wires up a new backend.
- **Locked down.** Every request needs `/call/<token>/…`; the server listens on loopback; secrets are owner-only on disk.
- **Your Claude credentials stay out.** Each profile runs Claude Code in its own config directory with your agents,
  skills, commands and plugins copied in, but never an official login or API key.
- **Sessions travel.** `ccgp send` (aliases `ccgp import` / `ccgp export`) copy the latest sessions of the
  current directory between Claude Code and a profile, or between two profiles.
- **Plain Ktav config**, created on first run, hot-reloaded. ccgp changes it only on your command, and
  `ccgp profile add` / `rm` validate the result and keep a backup first ([details](docs/configuration.md)).
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

Other ways:

- Download the archive for your platform from [Releases](https://github.com/PHPCraftdream/cc-go-proxy/releases), unpack it
  and put `ccgp` on your `PATH`. Archives are checksummed and carry build-provenance attestations
  ([verify](docs/releasing.md#what-a-release-contains)).
- `go install github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp@latest` (from source, Go 1.26+).
- npm (`npm install -g cc-go-proxy`, commands `ccgp` and `ccgpy`) is being published and is not available yet.

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

For z.ai: `ccgp login zai` once (asks for your API key), then `ccgp zai`.

A second ChatGPT account: `ccgp profile add codex2 --like codex`, then `ccgp codex2` (signs in on first start).
Details: [profiles](docs/configuration.md#creating-and-removing-profiles).

## Commands

| Command | Purpose |
| --- | --- |
| `ccgp <profile> [claude args…]` | Ensure server and credentials, run `claude` through the profile |
| `ccgpy <profile> [claude args…]` | Same as `ccgp <profile> --dangerously-skip-permissions …`: Claude Code without permission prompts, so use it only where that is acceptable |
| `ccgp login <provider> [--device]` | (Re)authenticate. Codex: browser OAuth, or `--device` for a device code. Others: API key |
| `ccgp models [apply] [-v] [profile]` | Show, or write into the config, the newest models per Claude tier |
| `ccgp send <from> <to> [count]` | Copy the latest sessions (default 10) of the current directory between two profiles or `claude` (your own Claude Code) |
| `ccgp import <profile> [count]` | Alias of `ccgp send claude <profile>` |
| `ccgp export <profile> [count]` | Alias of `ccgp send <profile> claude` |
| `ccgp profile add\|list\|rm` | Manage profiles (and their providers) in the config: new logins (`--like`), profiles on an existing login (`--provider`), new backends (`--type`) |
| `ccgp start` / `stop` / `status` | Manage the background server and show paths and login state |
| `ccgp serve [-d] [-s]` | Run the server in the foreground (`-d` dumps bodies, `-s` logs requests) |
| `ccgp config reset` | Back up the config and write the default one |
| `ccgp version` | Print the version |

## How ccgp differs from upstream

| | [claude-code-proxy](https://github.com/nielspeter/claude-code-proxy) (upstream) | ccgp |
| --- | --- | --- |
| Shape | A proxy you configure with env vars and point Claude Code at | A launcher + proxy: `ccgp <profile>` does the whole run |
| Backends | OpenAI-compatible Chat Completions (OpenRouter, OpenAI, Ollama) | Those, plus ChatGPT Codex via subscription OAuth (Responses API) and Anthropic-compatible endpoints (z.ai) |
| Routing and access | One backend (`OPENAI_BASE_URL`) per proxy | Several providers; named profiles, each with its own URL token and its own provider |
| Config | `.env` and environment | Ktav `providers[]` / `profiles[]`, per-rule model mapping, generated mappings (`ccgp models`), hot reload |
| Context | — | Per-model context windows; compaction window set for the session's main model |
| Claude credentials | Not addressed | Never reach a ccgp session: isolated config directory per profile |
| Sessions | — | `ccgp send` copies the current directory's latest sessions between profiles (and your own Claude Code via `import` / `export`) |
| Secrets | `.env` | Separate owner-only credentials directory; OAuth refresh tokens rotate safely |
| Platforms | Builds for macOS, Linux and Windows | Windows first (owner-only ACLs, detached background process, tested in CI), also Linux and macOS |
| Distribution | Source builds and release binaries | Binaries for 6 platforms with provenance attestations, npm, `go install` |

The Anthropic ↔ OpenAI Chat Completions translation, streaming and adaptive per-model capability detection are
upstream's work, kept here for `codex_api`. Thank you, Niels Peter Strandberg and contributors.

## Documentation

| | |
| --- | --- |
| [Configuration](docs/configuration.md) | Config file, providers, profiles (`profile add` / `rm`), model rules, context windows |
| [Providers](docs/providers.md) | Codex, API keys, z.ai: sign-in, model mapping, effort levels |
| [Sessions](docs/sessions.md) | `ccgp send`, `import` / `export`, thinking blocks |
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
