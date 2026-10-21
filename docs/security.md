# Security model

To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## Access to the proxy

- Every model request must carry a profile token in the path: `/call/<token>/v1/messages`. An unknown token
  gets `404`. Only `/health` is open, and it reveals the version and process id, nothing else.
- Each profile has its own token (256 random bits, hex, stored in the `creds` directory), so profiles are
  isolated from each other and each routes only to its own provider. Tokens are masked in logs.
- The server listens on `127.0.0.1` by default. Do not expose it to a network: the token is the only gate.
- No CORS headers: browsers cannot call the proxy.

## Secrets at rest

- Everything secret lives in `<config dir>/cc-go-proxy/creds/`: profile tokens, API keys, rotating OAuth
  tokens. Files are written atomically and are readable only by the current user (Windows: an explicit
  owner-only DACL; Unix: mode `0600`).
- `config.ktav` holds no secrets and is never rewritten by ccgp, apart from the marked model-mapping blocks.
- A rotated OAuth refresh token is persisted **before** it is used.

## Official Claude credentials never reach a ccgp session

`ccgp <profile>` runs `claude` with its own config directory (`<config dir>/cc-go-proxy/claude/<profile>`,
passed as `CLAUDE_CONFIG_DIR`), so your claude.ai login and Anthropic API keys are not available to it:

- there is no `.credentials.json`; one left behind (for example by `/login`) is deleted at the next launch;
- parent environment variables that select an official endpoint or credential (`ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `CLAUDE_CODE_USE_BEDROCK` / `VERTEX` / `FOUNDRY`) are dropped;
- `settings.json` is mirrored without `apiKeyHelper`, `forceLogin*` and those variables in `env`;
- `.claude.json` is seeded once with onboarding state, theme and MCP servers only: no account data.

ccgp does not read, forward or store a Claude subscription's credentials, and does not route a Claude
subscription through the proxy.

To keep the working conditions familiar, everything except credentials is copied from your own Claude Code
directory at every launch, one way:

- `CLAUDE.md`, `settings.json` and `keybindings.json` are overwritten from the source;
- `agents`, `commands`, `skills`, `output-styles` and `plugins` are mirrored incrementally: only changed files
  are copied, files you delete at the source are removed from the copy, files created inside the isolated
  directory are kept, credential files are never copied. Absolute paths in `plugins/*.json` are rewritten to
  point at the copy. Links and junctions are never followed, so a session cannot write into your own directory.

History, sessions and projects stay separate; move chosen ones with [`ccgp import` / `export`](sessions.md).
The only command that writes into your own Claude Code directory is `ccgp export`, under `projects/`.

## Third-party terms

Using a ChatGPT subscription through a third-party client is subject to OpenAI's terms. Anthropic's terms
restrict third parties from routing a Claude subscription's credentials, which is why ccgp never touches them.
ccgp is not affiliated with or endorsed by Anthropic, OpenAI or z.ai.
