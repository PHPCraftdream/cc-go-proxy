import assert from 'node:assert/strict';
import { test } from 'node:test';

import { PLATFORMS } from './build.mjs';
import { alreadyPublished, publishRank } from './publish.mjs';

test('Windows packages go first, the main package last', () => {
  const platform = PLATFORMS.map((p) => ({ name: `x-${p.id}`, os: [p.os], cpu: [p.cpu] }));
  const main = { name: 'x', optionalDependencies: {} };
  const order = [main, ...platform]
    .sort((a, b) => publishRank(a) - publishRank(b) || a.name.localeCompare(b.name))
    .map((p) => p.name);
  assert.deepEqual(order, [
    'x-w32-arm64', 'x-w32-x64',
    'x-darwin-arm64', 'x-darwin-x64', 'x-linux-arm64', 'x-linux-x64',
    'x',
  ]);
});

test('a version npm already has counts as published', () => {
  // Seen on a re-run of the v0.1.1 release, when `npm view` still missed the version.
  assert.ok(alreadyPublished('npm error 403 403 Forbidden - PUT https://registry.npmjs.org/cc-go-proxy-darwin-x64 - You cannot publish over the previously published versions: 0.1.1.'));
  assert.ok(alreadyPublished('npm ERR! code EPUBLISHCONFLICT'));
});

test('other refusals are failures', () => {
  assert.ok(!alreadyPublished('npm error 403 403 Forbidden - PUT https://registry.npmjs.org/x - Package name triggered spam detection'));
  assert.ok(!alreadyPublished('npm error 403 403 Forbidden - PUT https://registry.npmjs.org/ccgp - Package name too similar to existing package ncp'));
  assert.ok(!alreadyPublished('npm error code E401'));
  assert.ok(!alreadyPublished(''));
});
