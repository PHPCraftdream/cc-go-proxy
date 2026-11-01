# HTTP API

The background server (`ccgp --start`, or `ccgp --serve` in the foreground) listens on `listen.host:listen.port`
(default `127.0.0.1:60093`). Claude Code talks to it through `ANTHROPIC_BASE_URL=http://127.0.0.1:60093/call/<token>`,
set by `ccgp <profile>`. You rarely call it by hand; this page is for scripts, health checks and other clients.

| Method and path | Auth | Purpose |
| --- | --- | --- |
| `GET /health` | none | Liveness and identity of the server |
| `GET /call/<token>/` | token | Routing summary of the profile |
| `GET /call/<token>/model?requested=<model>` | token | Which backend model a Claude model name maps to |
| `POST /call/<token>/v1/messages` | token | Anthropic Messages API (streaming or not) |
| `POST /call/<token>/v1/messages/count_tokens` | token | Token count of a Messages request |

There is no other endpoint and no CORS header: browsers cannot call the server. Anything else answers `404`.

## Tokens

Every profile has its own 256-bit URL token, generated on first use and kept in the `creds` directory (never in the
config). The token in the path **is** the credential and selects the profile, hence its provider and model rules:
`/call/<token-of-zai>/…` always goes to z.ai. An unknown token answers `404` with an empty body. Tokens are masked as
`/call/***/…` in the log. Profiles of type `claude` have no token and no endpoint. Keep the server on loopback.

## `GET /health`

No token. Used by `ccgp --status`, `--stop` and the launcher to find and judge the server.

```sh
curl -s http://127.0.0.1:60093/health
```

```json
{"status":"ok","service":"ccgp","version":"0.1.0","pid":4242,"exe":"…/ccgp","exe_mod":1761900000,"active":0}
```

`pid` is the server process, `exe` and `exe_mod` (Unix seconds) the binary it was started from, and `active` the
number of model requests in flight (a streamed answer counts until it ends).

## `GET /call/<token>/`

```sh
curl -s http://127.0.0.1:60093/call/$TOKEN/
```

```json
{"version":"0.1.0","profile":"codex","provider":"codex","type":"codex","models":[{"match":"*opus*","model":"gpt-6.1-sol","context_window":922000}],"default":""}
```

The profile name, its provider and type, its model rules and `default_model`. It exposes the routing config, which is
why it needs the token.

## `GET /call/<token>/model?requested=<model>`

```sh
curl -s "http://127.0.0.1:60093/call/$TOKEN/model?requested=claude-opus-4-1"
```

```json
{"kind":"ccgp:model-route:v1","requestedModel":"claude-opus-4-1","backendModel":"gpt-6.1-sol"}
```

Resolves a Claude model name with the same rules inference uses (profile rules, then `default_model`, then the tier
defaults), hot-reloaded with the config. No request goes to the backend and no login is needed. The answer is
`Cache-Control: no-store` and states the configured route, not the model an upstream alias finally selects. A missing,
over 256 characters long or control-character `requested` answers `400`. The command-line twin is
[`ccgp --which`](cli.md#checks). Clients must fall back to their own labels when the endpoint is unavailable.

## `POST /call/<token>/v1/messages`

The [Anthropic Messages API](https://docs.claude.com/en/api/messages) as Claude Code sends it, with `stream` true or
false. What happens depends on the profile's provider:

| Provider type | The request becomes |
| --- | --- |
| `codex` | A ChatGPT Codex Responses API call, authenticated with the provider's OAuth login; the answer is converted back to Messages events |
| `codex_api` | An OpenAI-compatible Chat Completions call with the provider's API key |
| `anthropic_compat` | The same Messages request, forwarded with the provider's API key; only the model (and effort level) is changed |

The model is rewritten by the profile's rules and the model-identity sentence in the system prompt is corrected
([providers](providers.md#the-model-identity)). Bodies up to `listen.max_body_mb` (default 256 MiB) are accepted;
larger ones answer `413`.

```sh
curl -s http://127.0.0.1:60093/call/$TOKEN/v1/messages \
  -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"sonnet","max_tokens":64,"messages":[{"role":"user","content":"Say hi"}]}'
```

Errors come in the Anthropic shape, `{"type":"error","error":{"type":"…","message":"…"}}`. Besides the backend's own
errors passed on with their status:

| Status | Meaning |
| --- | --- |
| `400` | the body is not a valid request (`invalid_request_error`) |
| `404` | unknown token |
| `503` | the profile cannot be served now, for example the provider has no API key (`run ccgp --login <provider>`) or the profile was removed from the config |
| `413` | the body is larger than `listen.max_body_mb` |
| `429` | the backend rate-limited the request; a `Retry-After` of up to 5 s is waited out once, a longer one is passed on with its header |
| `502` | the connection to the backend failed after the retries |

Codex calls are made before streaming starts, so the backend's HTTP status reaches the client as the status of the
reply. See [reading the log](cli.md#reading-the-log) for what each retry and failure line means.

## `POST /call/<token>/v1/messages/count_tokens`

The same body as Messages without the generation parameters; the answer is `{"input_tokens": N}`. For
`anthropic_compat` the call is forwarded to the endpoint; if the endpoint fails or has no such route, and always for
`codex` and `codex_api` (which have none), ccgp answers with an estimate of about four bytes of request per token so
that Claude Code keeps working.

## Control from the command line

| Task | Command |
| --- | --- |
| Is the server up, and which one? | `ccgp --status [--json]`, `curl /health` |
| Start, stop, replace | `ccgp --start`, `--stop`, `--restart` |
| Watch requests | `ccgp --logs -f`, or `ccgp --serve -s` in the foreground |
| See the token URL of a profile | it is not printed on purpose; it lives in `creds/` and is used by `ccgp <profile>` |
