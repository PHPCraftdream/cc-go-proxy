# Releasing

A release is a tag `vMAJOR.MINOR.PATCH` (or `-rc.1`-style pre-release) on `main`. The
[Release workflow](../.github/workflows/release.yml) does the rest:

```
tag ─▶ tests (Linux, Windows) ─▶ 10 binaries ─▶ GitHub Release ─▶ npm packages
                                  + THIRD_PARTY_LICENSES        │
                                                                └ archives, SHA256SUMS, build-provenance attestations
```

The version inside the binary comes from the tag (`-ldflags -X …/internal/server.ProxyVersion=`), so there is
no version file to edit. Versions are never bumped automatically.

## Steps

1. In [`CHANGELOG.md`](../CHANGELOG.md) rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD` (keep a fresh empty
   `## [Unreleased]` above it). The section becomes the release notes.
2. Merge to `main` and wait for CI (tests on Linux and Windows, lint, license check, cross-compile vet).
3. Optional dry run: **Actions → Release → Run workflow**. It builds and packs everything and runs
   `npm publish --dry-run`; it creates no release and publishes nothing.
4. Tag and push:
   ```sh
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```
5. Watch the run. It can be re-run after a failure: the GitHub Release step overwrites its assets, and the npm step
   skips versions that are already published.

The workflow refuses a tag that is not on `main` or is not a semantic version.

npm publishing is off by default: on a tag the npm job only builds the packages, and publishes them only when the
repository variable `NPM_PUBLISH` is `true` (`gh variable set NPM_PUBLISH --body true -R PHPCraftdream/cc-go-proxy`).
Turn it on for a release that should reach npm, or turn it on later and re-run the release run's npm job; the history
of the first attempts is in [npm-publication-log.md](npm-publication-log.md). `install.sh` / `install.ps1` install
from the GitHub release and always take the newest one; CI checks that.

The packages go out in this order: the two Windows ones (`-w32-*`, the names npm refused before), the other four
platforms, the main package last. A refusal stops the run there, so a refused Windows name leaves nothing published.

## What a release contains

| Asset | |
| --- | --- |
| `ccgp_<version>_<os>_<arch>.tar.gz` / `.zip` | linux, darwin, windows × amd64, arm64: the binary, `LICENSE-MIT`, `LICENSE-APACHE`, `NOTICE`, `README.md`, `THIRD_PARTY_LICENSES.md` |
| `SHA256SUMS` | checksums of all archives |
| attestations | signed SLSA build provenance for every file, from GitHub Actions |
| npm `@cc-go-proxy/ccgp` + `@cc-go-proxy/ccgp-<os>-<cpu>` | the launcher package and six platform packages (the esbuild layout: no install scripts), published with provenance |

Verify a download:

```sh
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify ccgp_1.2.3_linux_amd64.tar.gz --repo PHPCraftdream/cc-go-proxy
npm audit signatures          # after npm install: checks registry signatures and provenance
```

`go install github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp@vX.Y.Z` needs nothing from the workflow: the Go module
proxy serves the tag. Such a build reports the default version compiled into the source; use a release archive or the
npm package when the exact version matters.

## npm: Trusted Publishing only

Releases publish to npm through **npm Trusted Publishing**: the npm job exchanges its GitHub OIDC identity for a
short-lived publish credential. A stored token is never used:

- the workflow has no token step, and a release that finds an `NPM_TOKEN` secret stops before publishing
  (`Refuse a stored npm token`);
- `scripts/npm/publish.mjs` refuses to run while `NODE_AUTH_TOKEN` (anything but the setup-node placeholder) or
  `NPM_TOKEN` is set.

### Set it up once per package

A package must exist before a Trusted Publisher can be added; the first version of a new package name is published by
hand with your own npm login (2FA), never with a stored token. Then, logged in as an owner of the organization
(npm 11.10 or newer; npm asks for 2FA):

```sh
npm login
node scripts/npm/trust.mjs --dry-run   # what would be set
node scripts/npm/trust.mjs             # set it: npm trust github <package> --file release.yml --repo … --env npm
node scripts/npm/trust.mjs --list      # check
```

Or by hand, for **each of the seven packages** (`@cc-go-proxy/ccgp` and `-linux-x64`, `-linux-arm64`, `-darwin-x64`,
`-darwin-arm64`, `-w32-x64`, `-w32-arm64`): npmjs.com → the package → *Settings* → *Trusted Publisher* →
*GitHub Actions*:

| Field | Value |
| --- | --- |
| Organization or user | `PHPCraftdream` |
| Repository | `cc-go-proxy` |
| Workflow filename | `release.yml` |
| Environment | `npm` (the job runs in it; leave empty only if you also remove `environment: npm`) |

npm does not check these values when they are saved: a wrong one shows only when a release is refused.

**A new configuration expires** unless a publish through it succeeds soon after it was created (npm's documented
window is two days): that first publish validates it and binds it to the repository's identity. An expired entry
cannot publish or be edited; delete it and create it again. So set it up right before a release, and make sure the
release really publishes through it. All seven packages are published by one release run, so one run validates all
seven; if that run fails half way, the packages it did not reach keep their unvalidated configuration. See
[npm's documentation](https://docs.npmjs.com/trusted-publishers/).

Needs npm 11.5.1 or newer (the job installs it) and GitHub-hosted runners. Optionally set each package to *Require
two-factor authentication and disallow tokens*, so a token can no longer publish at all.

## Licenses

ccgp is `MIT OR Apache-2.0`. Dependencies are checked in CI ([`licenses.yml`](../.github/workflows/licenses.yml)):

- `go-licenses check` allows only `MIT`, `Apache-2.0`, `BSD-2-Clause`, `BSD-3-Clause`, `ISC`, `0BSD` for libraries
  linked into the binary; copyleft and unknown licenses fail the build. Widen `ALLOWED_LICENSES` only on purpose.
- On pull requests GitHub's dependency review applies the same list to dependency changes and fails on high-severity
  advisories.
- `scripts/third-party-licenses.sh` writes `THIRD_PARTY_LICENSES.md` (every linked library with its license text).
  It goes into every archive and npm package, as MIT, BSD and Apache require for binary distribution.

Parts derived from [nielspeter/claude-code-proxy](https://github.com/nielspeter/claude-code-proxy) stay available under
the MIT license with its copyright notice; see [`NOTICE`](../NOTICE).
