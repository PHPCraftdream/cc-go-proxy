# Contributing

Thanks for helping. Bug reports, fixes, provider presets and documentation are all welcome.

## Before you start

- For anything bigger than a fix, open an issue first so we can agree on the shape.
- Security problems: do **not** open an issue, see [SECURITY.md](SECURITY.md).
- Read [docs/architecture.md](docs/architecture.md): it lists the layout and the invariants a change must keep
  (every model endpoint under `/call/:token/`, secrets only through `securefile`, OAuth refresh tokens persisted
  before use, official Claude credentials never reaching a session).

## Development

You need Go 1.26 or newer (and Node 24 for the npm packaging scripts).

```sh
git clone https://github.com/PHPCraftdream/cc-go-proxy
cd cc-go-proxy
make build                              # ./ccgp
go vet ./...
go test ./...
golangci-lint run                       # the CI version is pinned in .github/workflows/ci.yml
node --test "scripts/npm/*.test.mjs"    # only if you touch scripts/npm
```

CI runs the tests on Linux and Windows. ccgp targets Windows first: its ACL, junction and detached-process code only
runs there, so keep platform-specific code behind build tags and covered by a test.

## Pull requests

- Keep a change focused; do not mix a refactor with a behaviour change.
- Add or update tests with the code. A bug fix starts with a test that fails without it.
- Run `gofmt` and `go vet`; CI also runs `golangci-lint`.
- Update the docs (`README.md`, `docs/`) and add a line under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md)
  for anything a user can notice.
- Write comments that say *why*, briefly. Commit messages: a short imperative subject (`fix:`, `feat:`, `docs:`,
  `ci:` prefixes are used), the reason in the body.
- Never commit secrets, tokens, real account data or personal paths. Logs mask tokens, but check what you paste
  into an issue or a test fixture.

## Dependencies and licenses

ccgp is `MIT OR Apache-2.0`, and CI allows only permissive dependency licenses (MIT, Apache-2.0, BSD-2/3-Clause, ISC,
0BSD). A new dependency needs a reason in the pull request; copyleft and unlicensed ones are rejected by CI.

## Licensing of contributions

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in ccgp by you, as
defined in the Apache-2.0 license, shall be dual licensed as `MIT OR Apache-2.0`, without any additional terms or
conditions. Code derived from [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy) keeps its
MIT notice (see [NOTICE](NOTICE)).

## Conduct

Be kind and assume good faith. See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
