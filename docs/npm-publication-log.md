# npm publication log: the first release

What was done to publish `ccgp` on npm, what npm refused, how the packages were renamed, published and deleted, and
where it stands. Written on 2026-10-23, updated on 2026-10-25. The general process is in [releasing.md](releasing.md);
this file is the history of the first attempts and what to do next.

## Summary

| | State |
| --- | --- |
| GitHub Releases `v0.1.0`, `v0.1.1`, `v0.2.0` | `v0.1.0`, `v0.1.1` to be deleted, `v0.2.0` released again from the current `main` (section 7) |
| npm `cc-go-proxy-linux-x64`, `-linux-arm64`, `-darwin-x64`, `-darwin-arm64` `@0.1.1` | deleted on 2026-10-25 (`unpublish`) |
| npm `cc-go-proxy-win32-x64`, `cc-go-proxy-win32-arm64` | never published: npm spam detection refuses `*-win32-<cpu>` names |
| npm `ccgp-<os>-<cpu>` (the first attempt) | deleted (`unpublish`), all six return 404 |
| Next | Windows packages renamed to `cc-go-proxy-w32-x64` / `-w32-arm64`; all seven published as `0.2.0` |

The npm package layout is the esbuild one: a main package (`cc-go-proxy`, a small JS launcher, commands `ccgp` and
`ccgpy`) with `optionalDependencies` on six platform packages (`cc-go-proxy-<os>-<cpu>`), each holding two prebuilt
binaries. The platform packages are published first, the main one last, so nobody gets a main package whose
dependencies are missing. That order is also why an incomplete run leaves **no installable package**: without the main
package nothing can be installed.

## Timeline (2026-10-22, UTC times from the workflow logs)

### 1. Preparation

- The release workflow had never run, so a dry run was started first (Actions → Release → Run workflow, run
  `39785599141`, version `0.0.0-dryrun`): tests on Linux and Windows, six binaries, third-party licenses and
  `npm publish --dry-run` for all seven packages passed; the GitHub Release job was skipped on purpose.
- The package names `ccgp`, `ccgpy`, `ccgp-<os>-<cpu>` answered 404 on `npm view` (looked free).
- Pre-release changes (commit `34e2152`): the changelog's *Unreleased* became *0.1.0*, the README "Pre-release" note was
  removed, dependencies were updated (this needs Go 1.26, so CI and the release use 1.26, golangci-lint v2.15.0), and
  `scripts/npm/trust.mjs` was added for the later switch to Trusted Publishing.
- The repository secret `NPM_TOKEN` (granular token, read and write, allowed to bypass 2FA) was set with
  `gh secret set NPM_TOKEN -R PHPCraftdream/cc-go-proxy` at 13:30.

### 2. `v0.1.0`: the name `ccgp` is refused

- Tag `v0.1.0` on `34e2152`, release run `39788895418`. GitHub Release created.
- npm job: the six platform packages `ccgp-<os>-<cpu>@0.1.0` were published. The main package failed:

  ```
  npm error 403 403 Forbidden - PUT https://registry.npmjs.org/ccgp - Package name too similar to existing package ncp;
  try renaming your package to '@phpcraftdream/ccgp' and publishing with 'npm publish --access=public' instead
  ```

  Some of the platform packages also carried a `0.0.0-stage` version next to `0.1.0` (placeholders that were already
  on the registry).
- Result: six orphaned platform packages, no installable package. The "too similar" rule is not visible in
  `npm view` (a 404 does not mean the name can be published), and `npm publish --dry-run` does not check it.

### 3. The decision: rename to `cc-go-proxy`

Options offered: the scope `@phpcraftdream/ccgp` (npm's own suggestion) or an unscoped name. Chosen:

- the unscoped package **`cc-go-proxy`** (the repository name), platform packages `cc-go-proxy-<os>-<cpu>`;
  the commands stay `ccgp` and `ccgpy`;
- a **new release `v0.1.1`** instead of moving the `v0.1.0` tag (the tag and its GitHub Release stay as they are);
- the six orphaned `ccgp-<os>-<cpu>` packages get **deleted**.

Commit `9ba5e3d`: `NPM_NAME: cc-go-proxy` in `release.yml`, `trust.mjs` and its test, README, `releasing.md`, a
`0.1.1` changelog entry. CI green, tag `v0.1.1`, release run `39790683645`.

### 4. `v0.1.1`: publishing the new names, and the spam block

- GitHub Release `v0.1.1` created (13:53).
- npm job: `cc-go-proxy-darwin-arm64`, `-darwin-x64`, `-linux-arm64`, `-linux-x64` @0.1.1 published, then:

  ```
  npm error 403 403 Forbidden - PUT https://registry.npmjs.org/cc-go-proxy-win32-arm64 -
  Package name triggered spam detection; if you believe this is in error, please contact support at https://npmjs.com/support
  ```

  `cc-go-proxy-win32-x64` and the main `cc-go-proxy` were never reached.
- **Re-run, attempt 2** (`gh run rerun 39790683645 --failed`): failed differently, on `cc-go-proxy-darwin-x64`:
  `You cannot publish over the previously published versions: 0.1.1`. The script's pre-check (`npm view name@version`)
  still did not see the version npm had just accepted (registry replicas lag), so it tried to publish it again.
  Fix, commit `5a32ef9`: `scripts/npm/publish.mjs` now treats that refusal (and `EPUBLISHCONFLICT`) as "already
  published" and goes on; `scripts/npm/publish.test.mjs` covers it. A re-run uses the workflow files of the tag, so
  this fix helps the next release, not the re-runs of `v0.1.1`.
- **Re-run, attempt 3** (after the old packages were deleted): the same spam block on `cc-go-proxy-win32-arm64`.
- **Re-run, attempt 4** (2026-10-24, two days later): the same block again; the block does not lift on its own so far.

### 5. What is known about the spam block

- It is a `403` on the `PUT` of a **new package name**; a real rate limit would be `429`. npm documents neither limit.
- Reports of the same error: the `litsea` project hit it on the first release of seven new platform packages
  (`litsea-win32-x64-msvc`, issue [mosuka/litsea#225](https://github.com/mosuka/litsea/issues/225)). A retry seven
  minutes later was rejected again; only a ticket to npm support lifted it, after about six days, with the names kept.
  Their analysis: it is not a per-account counter, because other new names were created right before and after.
- Our guess (unconfirmed): ten new package names in a short time from a fresh account (six `ccgp-*` that were then
  deleted, then the `cc-go-proxy*` set) looked like spam, which is a temporary heuristic. If so, it may lift on its own.
- Hourly retries were set up with a cron, then changed to every 6 hours, then **switched off** (the block was still
  there, and repeated attempts can only look worse). The last run attempt stopped at the same package.

### 6. Deleting the six old packages (`ccgp-<os>-<cpu>`)

`npm unpublish <name> --force` removes a package with all its versions (`0.1.0` and `0.0.0-stage`).

- It needs an npm login **and 2FA**: from the assistant's shell the first try ended with `EOTP — This operation
  requires a one-time password`, so the commands were run by the owner in their terminal (`npm login` first). In
  `cmd.exe`, where the bash `for` syntax does not work:

  ```bat
  for %p in (ccgp-linux-x64 ccgp-linux-arm64 ccgp-darwin-x64 ccgp-darwin-arm64 ccgp-win32-x64 ccgp-win32-arm64) do npm unpublish %p --force --otp=<code>
  ```

  (a one-time code lives about 30 seconds; the browser confirmation flow works too.)
- It is **irreversible**: the same name and version cannot be published again. npm also keeps the names reserved for
  a while, and allows unpublishing only for a limited time and while nothing depends on the package.
- Check after the deletion: `npm view ccgp-linux-x64 version` and the other five return `E404`
  (done on 2026-10-22: all six 404). The four published `cc-go-proxy-*` packages were deliberately left alone.

### 7. 2026-10-25: the block is the `win32` name, not the project

- Another project of the same owner (`netivri`, the same esbuild layout and workflow) published
  `netivri-linux-x64`, `-linux-arm64`, `-darwin-x64`, `-darwin-arm64` without trouble and then got exactly the same
  refusal on the first Windows package:
  `403 Forbidden - PUT https://registry.npmjs.org/netivri-win32-x64 - Package name triggered spam detection`.
  Two unrelated names, both blocked only on `<name>-win32-<cpu>`: the spam filter reacts to that pattern.
- Decision: rename the Windows platform packages to `<name>-w32-x64` / `<name>-w32-arm64` (`PLATFORMS` in
  `scripts/npm/build.mjs`; the launcher maps `process.platform === 'win32'` to `w32`; the `os` field stays `win32`).
- The four `cc-go-proxy-*@0.1.1` packages were deleted by the owner (`npm login --auth-type=web`, then
  `npm unpublish <name> --force` with 2FA). A granular token with "bypass 2FA" can no longer unpublish:
  `403 … Granular access tokens that bypass two-factor authentication may not perform this action`.
- The history is restarted as one release `0.2.0`: the GitHub Releases and tags `v0.1.0`, `v0.1.1`, `v0.2.0` are
  deleted and `v0.2.0` is tagged again on the current `main`. No `cc-go-proxy*` package ever had the npm version
  `0.2.0`, so it is free; `0.1.1` can never be used again for the four deleted names.
- npm blocks new versions of a fully unpublished package for 24 hours, so the four `cc-go-proxy-<linux|darwin>-*`
  names can be published again from about 2026-10-26 17:00 UTC.

## Where it stands, and what to do next

The items below were written on 2026-10-23, before section 7; what is still open after it:

- dry-run the release with the `w32` names (Actions → Release → Run workflow, version `0.2.0`);
- after the 24-hour wait: delete the old releases and tags, set `NPM_PUBLISH=true`, tag `v0.2.0`;
- if `cc-go-proxy-w32-*` is refused too, use the support ticket below with the new names.


1. **Wait or ask npm.** If the block does not lift within a day or two, write to https://npmjs.com/support from the
   `phpcraftdream` account. A text that can be pasted:

   > Publishing the first release of an open-source CLI is blocked by spam detection:
   > `403 Forbidden - PUT https://registry.npmjs.org/cc-go-proxy-win32-arm64 - Package name triggered spam detection`.
   > The project is https://github.com/PHPCraftdream/cc-go-proxy (MIT OR Apache-2.0), distributed in the esbuild
   > layout: a main package `cc-go-proxy` and six platform packages `cc-go-proxy-<os>-<cpu>`, published from GitHub
   > Actions with provenance (workflow `.github/workflows/release.yml`, release `v0.1.1`). Four platform packages are
   > already on the registry; `cc-go-proxy-win32-arm64`, `cc-go-proxy-win32-x64` and `cc-go-proxy` are blocked or not
   > reached. Please lift the block for these three names.

2. **Publish the rest.** The npm job of the release run is re-run with the failed jobs only; versions that are
   already there are skipped:

   ```sh
   gh run rerun 39790683645 -R PHPCraftdream/cc-go-proxy --failed
   ```

   If `v0.1.1` is too old to re-run by then, publish the remaining three from a new tag (`v0.1.2`), which also
   carries the `publish.mjs` fix.
3. **After all seven packages exist:** check `npm view cc-go-proxy@<version>`, add `npm install -g cc-go-proxy`
   (and the badge) back to the README, set up Trusted Publishing with `node scripts/npm/trust.mjs` (see
   [releasing.md](releasing.md)), then delete the `NPM_TOKEN` secret and revoke the token.

## Lessons for the next release

- A free name on `npm view` is not proof that npm accepts it: the similarity rule ("too similar to `ncp`") and the spam
  heuristics only show up on the real `PUT`. Check a new name early, not at the tag; a scope (`@phpcraftdream/...`)
  avoids the similarity rule.
- Publish the **main package first** under a throwaway pre-release version, or in a separate earlier step, if there is
  any doubt about the name: the platform-first order is right for users but leaves orphans when the last step fails.
- Do not create many new package names at once from a young account; spread the first publish of seven packages out.
- Keep the re-run safe: skip versions that exist, and trust the refusal message, not only `npm view` (fixed in
  `5a32ef9`).
- Deleting is possible only briefly and only with 2FA; avoid needing it.
