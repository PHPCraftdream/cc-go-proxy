# Security policy

## Reporting a vulnerability

Please report privately through GitHub: **Security → Report a vulnerability** on
[PHPCraftdream/cc-go-proxy](https://github.com/PHPCraftdream/cc-go-proxy/security/advisories/new).
Do not open a public issue or pull request for a vulnerability, and do not post tokens or logs that contain secrets.

Include what you found, how to reproduce it, the ccgp --version (`ccgp --version`) and your OS. You can expect an
acknowledgement within a few days; fixes are released as soon as they are ready and credited if you wish.

## Supported versions

Only the latest release receives security fixes.

## What counts

In scope, for example:

- reaching a model endpoint without a valid profile token, or learning a token from the server (responses, logs, `/health`);
- reading or writing credentials outside the owner-only `creds` directory, or leaking OAuth or API keys;
- an official Claude credential (login, API key, OAuth token) reaching a ccgp session, or `ccgp --export` /
  `--import` copying something it must not;
- a request that makes the server write or read files outside its directories;
- vulnerabilities in the release pipeline: unsigned or tampered artifacts, a way to publish without the release workflow.

Out of scope: problems that need local administrator or same-user access to the machine, the server being deliberately
bound to a non-loopback address, and the terms of service of third-party backends.

See [docs/security.md](docs/security.md) for the model ccgp aims to enforce.
