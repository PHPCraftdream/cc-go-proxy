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
| npm `@phpcraftdream/ccgp` + `@phpcraftdream/ccgp-<os>-<cpu>` | the launcher package and six platform packages (the esbuild layout: no install scripts), published with provenance |

Verify a download:

```sh
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify ccgp_1.2.3_linux_amd64.tar.gz --repo PHPCraftdream/cc-go-proxy
npm audit signatures          # after npm install: checks registry signatures and provenance
```

`go install github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp@vX.Y.Z` needs nothing from the workflow: the Go module
proxy serves the tag. Such a build reports the default version compiled into the source; use a release archive or the
npm package when the exact version matters.

## npm: publish with a token now, Trusted Publishing later

The npm job needs no edit to switch. It looks at one thing:

| Secret `NPM_TOKEN` | The job publishes with |
| --- | --- |
| exists | that token (`NODE_AUTH_TOKEN`), plus provenance |
| does not exist | **npm Trusted Publishing**: an OIDC identity of the job, no stored credential |

### 1. First release, with a token

npm lets you add a Trusted Publisher in the settings of a package that already exists, so the first publish of each
new package name needs a token.

1. [npmjs.com](https://www.npmjs.com) → *Access Tokens* → *Generate New Token* → **Granular access token**:
   permission *Read and write*, packages *All packages* (none exists yet), the shortest expiry that suits you.
   Publishing from CI needs a token that may bypass 2FA.
2. Store it as the secret **`NPM_TOKEN`**; `gh` asks for the value without echoing it or keeping it in the shell
   history:
   ```sh
   gh secret set NPM_TOKEN -R PHPCraftdream/cc-go-proxy             # repository secret
   gh secret set NPM_TOKEN -R PHPCraftdream/cc-go-proxy --env npm   # or: only the npm job sees it
   ```
   The environment variant needs the `npm` environment to exist (*Settings → Environments*); there you can also
   require a reviewer for every publish.
3. Push the tag. Seven packages appear, all in the author's scope `@phpcraftdream`: `@phpcraftdream/ccgp` and
   `@phpcraftdream/ccgp-linux-x64`, `-linux-arm64`, `-darwin-x64`, `-darwin-arm64`, `-w32-x64`, `-w32-arm64`. (The
   bare name `ccgp` is refused by npm as too similar to `ncp`, which a scope avoids; npm's spam filter refused names
   ending in `-win32-<cpu>`, hence `w32`; the commands are still `ccgp` and `ccgpy`.) The scope is the npm user
   `phpcraftdream`, so no organization has to be created, and scoped packages are published with `--access public`
   (`publish.mjs` passes it).

To publish under another name or scope, change `NPM_NAME` at the top of `release.yml` (for example
`@yourscope/ccgp`; the platform packages follow as `@yourscope/ccgp-<os>-<cpu>`, with `w32` for Windows). The build
puts a scoped package in the directory `yourscope__ccgp`, so the output stays flat. The unscoped
`cc-go-proxy-*` names of the first attempts are left alone (see [npm-publication-log.md](npm-publication-log.md)).

### 2. Switch to Trusted Publishing

One command does it for all seven packages (npm 11.10 or newer, logged in as the owner; npm asks for 2FA):

```sh
npm login
node scripts/npm/trust.mjs --dry-run   # what would be set
node scripts/npm/trust.mjs             # set it: npm trust github <package> --file release.yml --repo … --env npm
node scripts/npm/trust.mjs --list      # check
```

Or by hand, for **each of the seven packages**: npmjs.com → the package → *Settings* → *Trusted Publisher* →
*GitHub Actions*:

| Field | Value |
| --- | --- |
| Organization or user | `PHPCraftdream` |
| Repository | `cc-go-proxy` |
| Workflow filename | `release.yml` |
| Environment | `npm` (the job runs in it; leave empty only if you also remove `environment: npm`) |

Then delete the `NPM_TOKEN` secret (repository and environment) and revoke the token on npmjs.com:

```sh
gh secret delete NPM_TOKEN -R PHPCraftdream/cc-go-proxy
gh secret delete NPM_TOKEN -R PHPCraftdream/cc-go-proxy --env npm   # if it was put there
``` The next tag
publishes through OIDC. Optionally set each package to *Require two-factor authentication and disallow tokens*,
so a leaked token can no longer publish.

Needs npm 11.5.1 or newer (the job installs it) and GitHub-hosted runners.

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
