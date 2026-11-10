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

For `codex`, ccgp may replay the opaque `x-codex-turn-state` response header unchanged on a later upstream request only
when its last input item is a `function_call_output` continuing the same tool loop. A new user turn drops it, while
retries retain it; a retry for a new turn may acquire and send state during that request. State is bounded to 512 FIFO
slots with a 30-minute TTL, keyed by the full `prompt_cache_key` plus system/tools hashes, and stale concurrent replies
cannot overwrite newer state. `thread-id` and `x-client-request-id` use stable hexadecimal bucket-derived values, never
the raw key. `session-id` and the model hint are unchanged. Without a returned hint there is no replay and no content
behavior change. The backend returns the token with the first reply of a turn; the later steps of a tool loop send it
back and usually get no new one (the log does not call that a miss).

<a id="codex-websocket-transport"></a>
**Codex websocket transport.** With `CODEX_TRANSPORT=auto` (the default) or `websocket` (alias `ws`) a Codex request goes
out over one websocket per conversation instead of an HTTP POST, the way the official Codex client does it;
`CODEX_TRANSPORT=http` turns it off. Only the Responses protocol has a websocket: `codex` providers use it, Chat
Completions (`codex_api`) and Anthropic Messages (`anthropic_compat`) providers always use HTTP. The first request of a
conversation (and any request after the connection was replaced) is sent in full; a later request whose history extends
the last completed response is sent as a *delta*: `previous_response_id` plus only the new items (a tool result, a new
user message), so a 4.7 MiB request becomes a few hundred bytes (live check: 24.6 KB -> 0.65 KB frames, the model sees the
whole context, 96% of the prompt read from the cache). The delta is used only when the backend model, system prompt,
tools, reasoning and tool choice are unchanged, the earlier items are byte for byte the same (item hashes) and the
history holds exactly what the backend produced in its last answer: the same assistant text, tool calls with the same id,
name and arguments (compared as JSON values), the same encrypted reasoning. Anything else (a compaction, a subagent
branch, an edited or dropped item) is sent in full on the same connection, with the reason in the log. Requests without
a cache key (`metadata.user_id`) use HTTP.

With `store: false` the backend keeps only the latest response of a connection, so a request that is not continued by
the next one costs the chain. Claude Code sends such background requests (a short fixed user message after the
conversation's history, with the same system prompt and tools); answered on the websocket they become the latest
response, and the conversation's next request, which holds neither the message nor its reply, would go out in full
(2-4 MiB). ccgp learns the message from that one divergence (the chain's last item is a user message of at most 8 KiB
and its reply is not in the new request; an edited message whose reply is still there is not learned) and from then on
sends requests that end in it over HTTP (zstd-compressed), so the websocket chain only holds the conversation.

A connection belongs to one endpoint, one account and one way out (proxy): two accounts never share one, the login is
checked on every request exactly as for HTTP (a refresh only when the token is due), and after a logout or an account
switch the old connections are closed. HTTP is the fallback. A request that finds its conversation's connection busy
(subagents share a key), or whose websocket failed before the request reached the backend (a refused upgrade, a dial or
TLS failure, a frame that could not be written: the backend acts only on a complete message), goes out over HTTP at once.
A connection that broke after the request went out and before any content is asked again, in full on a fresh connection,
the same rule as for an HTTP request broken before its reply; once content reached the client the stream ends with an
error and is never asked again. An endpoint that answers the upgrade with 400, 404, 405, 410, 426 or 501 (no websocket
there) gets none for 10 minutes; any other failed dial (401 or 403 after the token refresh, network, TLS, timeout) for 30
seconds; a 429 or a 5xx not at all (that request still goes over HTTP); the windows are per endpoint and account. A
connection is replaced after 25 minutes or 10 idle minutes; at most 8 are open. The backend ends a connection at a
fixed age (30 minutes seen, 60 documented), and replacing first keeps a request from meeting the cut: the next request
goes out in full on a fresh connection. If the backend ends connections earlier than planned (a break of a connection
5 minutes old or older, twice within an hour, per endpoint and account), the planned age is lowered by 5 minutes, down
to 5, and each further pair of early breaks lowers it again; after 6 hours without an early break it goes back to 25.
Breaks younger than 5 minutes are the network's, not an age limit, and count for nothing. Pings after
15 s of silence and a close after 30 s more are the same as for HTTP/2 connections. Tokens are refreshed exactly as for
HTTP (one refresh after a `401` handshake).

The model is rewritten by the profile's rules and the model-identity sentence in the system prompt is corrected
([providers](providers.md#the-model-identity)). Bodies up to `listen.max_body_mb` (default 256 MiB) are accepted;
larger ones answer `413`. If the provider has [peak hours](peak-hours.md) and the request arrives inside one (and no
`ccgp --peak <provider> pause` is running), ccgp answers `400` at once and sends nothing to the provider; this
route is the only one that does so, `count_tokens` and the others go on.

```sh
curl -s http://127.0.0.1:60093/call/$TOKEN/v1/messages \
  -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"sonnet","max_tokens":64,"messages":[{"role":"user","content":"Say hi"}]}'
```

Errors come in the Anthropic shape, `{"type":"error","error":{"type":"…","message":"…"}}`. Besides the backend's own
errors passed on with their status:

| Status | Meaning |
| --- | --- |
| `400` | the body is not a valid request (`invalid_request_error`); or the request arrived in the **peak hours** of the profile's provider: the message names the window and when requests resume, and the request is not sent to the provider ([peak hours](peak-hours.md)) |
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
