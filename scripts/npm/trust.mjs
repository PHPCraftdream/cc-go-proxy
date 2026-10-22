#!/usr/bin/env node
// Sets up npm Trusted Publishing for every package of a release: the main one
// and the six platform ones, all bound to release.yml in the npm environment.
// Run once after the first (token) release, logged in to npm as the owner:
//
//   npm login
//   node scripts/npm/trust.mjs --dry-run     # show what would be set
//   node scripts/npm/trust.mjs               # set it (npm asks for 2FA)
//   node scripts/npm/trust.mjs --list        # show what is set
//
// Then delete the NPM_TOKEN secret and revoke the token (docs/releasing.md).
import { spawnSync } from 'node:child_process';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

import { PLATFORMS } from './build.mjs';

/** @returns {string[]} the main package and the platform packages. */
export function packageNames(name = 'cc-go-proxy') {
  return [name, ...PLATFORMS.map((p) => `${name}-${p.id}`)];
}

/** @returns {string[][]} one npm argument list per package. */
export function trustCommands({ name = 'cc-go-proxy', repo = 'PHPCraftdream/cc-go-proxy', file = 'release.yml', env = 'npm', dryRun = false, list = false } = {}) {
  return packageNames(name).map((pkg) => {
    if (list) return ['trust', 'list', pkg];
    const args = ['trust', 'github', pkg, '--file', file, '--repo', repo, '--env', env, '--yes'];
    if (dryRun) args.push('--dry-run');
    return args;
  });
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      name: { type: 'string', default: 'cc-go-proxy' },
      repo: { type: 'string', default: 'PHPCraftdream/cc-go-proxy' },
      'dry-run': { type: 'boolean', default: false },
      list: { type: 'boolean', default: false },
    },
  });
  const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
  let failed = 0;
  for (const args of trustCommands({ name: values.name, repo: values.repo, dryRun: values['dry-run'], list: values.list })) {
    console.log(`> npm ${args.join(' ')}`);
    const r = spawnSync(npm, args, { stdio: 'inherit', shell: process.platform === 'win32' });
    if (r.status !== 0) failed++;
  }
  if (failed) {
    console.error(`${failed} package(s) failed; fix and run again (setting an existing trust again is harmless to retry).`);
    process.exit(1);
  }
}
