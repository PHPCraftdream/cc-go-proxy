#!/usr/bin/env node
// Publishes the packages made by build.mjs: platform packages first (Windows
// ones leading), the main package last, so a user never gets a main package
// whose dependencies are missing.
//
//   node scripts/npm/publish.mjs <out-dir> [--dry-run]
//
// Authentication is npm Trusted Publishing (the OIDC identity of the job). The script
// refuses to publish while a token is set in the environment, so a token can never
// be what publishes a release (the actions/setup-node placeholder is not a token).
// A version that is already on the registry is skipped, so a failed run can be repeated.
import { spawnSync } from 'node:child_process';
import { readdirSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// alreadyPublished recognizes npm's refusal to publish a version twice. The
// registry answers `npm view` from replicas that lag behind a fresh publish, so
// the pre-check can miss a version that a re-run then hits.
export function alreadyPublished(output) {
  return /cannot publish over (the )?previously published version/i.test(output) || /\bEPUBLISHCONFLICT\b/.test(output);
}

// publishRank orders the packages: Windows platform packages first (npm's spam
// filter has refused those names before, so a refusal stops the run before the
// other platforms are out), then the other platforms, the main package last.
export function publishRank(pkg) {
  if (pkg.optionalDependencies) return 2;
  return (pkg.os ?? []).includes('win32') ? 0 : 1;
}

// storedToken names the environment variable that holds an npm token, or "".
// actions/setup-node exports NODE_AUTH_TOKEN as a placeholder of X and dashes.
export function storedToken(env) {
  const placeholder = /^X+(-X+)*$/;
  for (const name of ['NODE_AUTH_TOKEN', 'NPM_TOKEN']) {
    const v = (env[name] ?? '').trim();
    if (v && !placeholder.test(v)) return name;
  }
  return '';
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const dryRun = args.includes('--dry-run');
  const outDir = args.find((a) => !a.startsWith('--'));
  if (!outDir) {
    console.error('usage: publish.mjs <out-dir> [--dry-run]');
    process.exit(2);
  }

  const token = storedToken(process.env);
  if (token && !dryRun) {
    console.error(`::error::${token} is set: releases publish only through npm Trusted Publishing, never with a token`);
    process.exit(2);
  }

  const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
  const shell = process.platform === 'win32';
  const dirs = readdirSync(resolve(outDir), { withFileTypes: true })
    .filter((d) => d.isDirectory())
    .map((d) => join(resolve(outDir), d.name));
  const pkgOf = (dir) => JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'));
  dirs.sort((a, b) => publishRank(pkgOf(a)) - publishRank(pkgOf(b)) || a.localeCompare(b));

  const published = (name, version) => {
    const r = spawnSync(npm, ['view', `${name}@${version}`, 'version'], { encoding: 'utf8', shell });
    return r.status === 0 && r.stdout.trim() === version;
  };

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
    const r = spawnSync(npm, flags, { cwd: dir, encoding: 'utf8', shell });
    process.stdout.write(r.stdout || '');
    process.stderr.write(r.stderr || '');
    if (r.status === 0) continue;
    if (!dryRun && alreadyPublished(`${r.stdout}\n${r.stderr}`)) {
      console.log(`skip ${name}@${version}: already published (the registry view lagged behind)`);
      continue;
    }
    failed = true;
    console.error(`::error::npm publish failed for ${name}@${version}`);
    break; // later packages depend on this one
  }
  process.exit(failed ? 1 : 0);
}
