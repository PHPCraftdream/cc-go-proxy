#!/usr/bin/env node
// Publishes the packages made by build.mjs: platform packages first, the main
// package last, so a user never gets a main package whose dependencies are missing.
//
//   node scripts/npm/publish.mjs <out-dir> [--dry-run]
//
// Authentication is not handled here, so the same script serves both modes:
//   * NODE_AUTH_TOKEN set to a real token -> token publishing;
//   * otherwise npm uses the OIDC identity of the job (Trusted Publishing).
// A version that is already on the registry is skipped, so a failed run can be repeated.
import { execFileSync, spawnSync } from 'node:child_process';
import { readdirSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

const args = process.argv.slice(2);
const dryRun = args.includes('--dry-run');
const outDir = args.find((a) => !a.startsWith('--'));
if (!outDir) {
  console.error('usage: publish.mjs <out-dir> [--dry-run]');
  process.exit(2);
}

const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const dirs = readdirSync(resolve(outDir), { withFileTypes: true })
  .filter((d) => d.isDirectory())
  .map((d) => join(resolve(outDir), d.name));
const pkgOf = (dir) => JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'));
// Packages without optionalDependencies (the platform ones) go first.
dirs.sort((a, b) => Number(!!pkgOf(a).optionalDependencies) - Number(!!pkgOf(b).optionalDependencies) || a.localeCompare(b));

function published(name, version) {
  const r = spawnSync(npm, ['view', `${name}@${version}`, 'version'], { encoding: 'utf8', shell: process.platform === 'win32' });
  return r.status === 0 && r.stdout.trim() === version;
}

let failed = false;
for (const dir of dirs) {
  const { name, version } = pkgOf(dir);
  if (!dryRun && published(name, version)) {
    console.log(`skip ${name}@${version}: already published`);
    continue;
  }
  // Pre-releases must not take over the "latest" tag.
  const tag = version.includes('-') ? 'next' : 'latest';
  const flags = ['publish', '--access', 'public', '--tag', tag];
  flags.push(dryRun ? '--dry-run' : '--provenance');
  console.log(`${dryRun ? 'dry-run' : 'publish'} ${name}@${version} (tag ${tag})`);
  try {
    execFileSync(npm, flags, { cwd: dir, stdio: 'inherit', shell: process.platform === 'win32' });
  } catch {
    failed = true;
    console.error(`::error::npm publish failed for ${name}@${version}`);
    break; // later packages depend on this one
  }
}
process.exit(failed ? 1 : 0);
