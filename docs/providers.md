# Providers

A provider is a backend plus the way ccgp talks to it. A profile ties a name you type (`ccgp <profile>`)
to a provider and to model-mapping rules ([configuration.md](configuration.md)).

| Type | Backend | Protocol | Credentials |
| --- | --- | --- | --- |
| `codex` | ChatGPT Codex, with a ChatGPT subscription | Anthropic Messages → OpenAI Responses API | OAuth, refresh token rotates |
| `codex_api` | OpenAI, OpenRouter, Ollama, any OpenAI-compatible API | Anthropic Messages → Chat Completions | API key |
| `anthropic_compat` | z.ai GLM, any Anthropic-compatible endpoint | Anthropic Messages, forwarded unconverted | API key |

`ccgp --login <provider>` authenticates; every `ccgp <profile>` run also does it when credentials are missing.

Claude Code asks for token counts all the time (`/v1/messages/count_tokens`). `anthropic_compat` forwards the call to
the endpoint; `codex` and `codex_api` have no counting endpoint, so ccgp answers with a length-based estimate
(about four bytes of request per token).

## `codex` — ChatGPT subscription

```sh
ccgp --login codex            # browser sign-in: prints a link, never opens a browser itself
ccgp --login codex --device   # device code instead, for machines without a browser
```

- **Browser flow** (default) is OAuth with PKCE and a loopback callback on `localhost:1455`.
- **Device flow** works when the account allows device-code sign-in (a ChatGPT workspace setting).
- Tokens are stored in the `creds` directory. The refresh token **rotates**: each refresh is persisted
  before the new access token is used, so a crash cannot lose the chain. Tokens are refreshed five minutes
  before they expire, a rejected one (401) triggers one forced refresh and retry, and a permanently dead
  refresh token (`refresh_token_expired`, `reused`, `invalidated`) asks you to sign in again.
- The session is separate from the Codex CLI's own login, so the two never invalidate each other.
- Requests become Responses API calls to `https://chatgpt.com/backend-api/codex/responses`: text, images,
  tools, tool results and reasoning. The reasoning `encrypted_content` travels in the thinking block's
  signature (prefix `ccgp1:`), so reasoning survives across turns.

### Model mapping for Codex

`ccgp --models` reads your account's model catalog (`…/codex/models`) and maps Claude's tiers to the newest releases:

| Claude tier | Codex model |
| --- | --- |
| Fable | newest `astra` |
| Opus | newest `sol` |
| Sonnet | newest `luna` |
| Haiku | `luna` of the release before the newest one |
| anything else | the Sonnet model (the final `match: *` rule) |

`ccgp --models apply [profile]` writes it into the profile's managed block, and every fresh
`ccgp --login codex` does too. `ccgp --models -v` shows every catalog field.

The catalog has two numbers: `context_window` is only the default of Codex clients and
`max_context_window` is the largest input the backend takes. For the GPT-6 generation and newer, ccgp
writes `max_context_window` or, when that is lower, OpenAI's documented input limit of 922000 (a 1,050,000 window
less 128,000 for output; the catalog's 872000 is conservative); for earlier releases such as `gpt-5.6-luna`, the
default (272000).
A model with no window at all is written as 272000 and noted.

> **Terms.** This path uses the private ChatGPT Codex backend that OpenAI's own clients use. Using a
> ChatGPT subscription through a third-party client is subject to OpenAI's terms and may stop working
> or be restricted at any time. That decision is yours.

## Several accounts

`ccgp --profile add codex2 --like codex` creates a second `codex`-type provider (its own login, `oauth-codex2.ktav`)
and a profile named `codex2`; `ccgp codex2` signs in on first start. Use this for separate personal and work
accounts. Pooling accounts to get around subscription limits is against OpenAI's terms.

The browser sign-in link opens in a browser where another account may already be signed in: use a private window
or another browser profile, or `ccgp --login codex2 --device`. Any provider type can be cloned with `--like`
(not only `codex`). Details: [configuration.md](configuration.md#creating-and-removing-profiles).

## `codex_api` — API key

Any OpenAI-compatible Chat Completions endpoint. `base_url` defaults to OpenAI; OpenRouter and
Ollama work by changing it. The key is asked for on first use and stored owner-only in `creds`, or read
from the environment variable named by `api_key_env`. Check the model names and `context_window`
values in the profile against your account: ccgp cannot know them for an arbitrary endpoint.

## `anthropic_compat` — z.ai (GLM)

```sh
ccgp --login zai     # asks for your z.ai API key once (hidden input), saves it owner-only
ccgp zai           # Claude Code through z.ai; extra arguments go to claude
```

Requests go to `https://api.z.ai/api/anthropic` unconverted: only the model is mapped and the model identity
in the system prompt is corrected. `ccgp --models apply zai` (also run when the key is saved) writes the
built-in preset:

| Claude tier | z.ai model | Context |
| --- | --- | --- |
| Fable | `glm-5.3` | 1M |
| Opus, Sonnet, Haiku, anything else | `glm-5.3-flash` | 1M |

z.ai has three effort levels, so Claude Code's five are converted by the provider's `effort_map`:
`low` → `low`, `medium` → `high`, `high` / `xhigh` / `max` → `max`. Any `anthropic_compat` provider can set its own.
`count_tokens` is forwarded; if an endpoint lacks it, a length estimate keeps Claude Code working.

Endpoint and model data: [docs.z.ai](https://docs.z.ai/devpack/tool/claude).

## The model identity

Claude Code writes "You are powered by the model named …" into its prompt. ccgp replaces that sentence (and
the knowledge-cutoff line) with the real backend model, in the system prompt and in system-role or user
context messages, but never in tool results. The request log shows the mapping as `requested -> backend`.
