import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { PLATFORMS } from './build.mjs';
import { packageNames, trustCommands } from './trust.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');

test('covers the main package and every platform package', () => {
  const names = packageNames();
  assert.equal(names.length, 1 + PLATFORMS.length);
  assert.equal(names[0], 'cc-go-proxy');
  assert.ok(names.includes('cc-go-proxy-w32-x64') && names.includes('cc-go-proxy-darwin-arm64'));
  assert.deepEqual(packageNames('@me/x').slice(0, 2), ['@me/x', '@me/x-linux-x64']);
});

test('binds each package to release.yml in the npm environment', () => {
  for (const args of trustCommands()) {
    assert.deepEqual(args.slice(0, 2), ['trust', 'github']);
    const opt = (k) => args[args.indexOf(k) + 1];
    assert.equal(opt('--file'), 'release.yml');
    assert.equal(opt('--repo'), 'PHPCraftdream/cc-go-proxy');
    assert.equal(opt('--env'), 'npm');
    assert.ok(args.includes('--yes') && !args.includes('--dry-run'));
  }
  assert.ok(trustCommands({ dryRun: true }).every((a) => a.includes('--dry-run')));
  assert.deepEqual(trustCommands({ list: true })[0], ['trust', 'list', 'cc-go-proxy']);
});

// The values must match what release.yml really does, or npm refuses the OIDC token.
test('matches the workflow: file name, environment, package name', () => {
  const wf = readFileSync(join(ROOT, '.github', 'workflows', 'release.yml'), 'utf8');
  assert.match(wf, /^\s+environment: npm$/m);
  assert.match(wf, /^\s+NPM_NAME: cc-go-proxy\b/m);
  assert.match(wf, /id-token: write\s+# npm provenance and Trusted Publishing/);
});
