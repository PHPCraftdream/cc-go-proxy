import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { build, PLATFORMS } from './build.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const work = mkdtempSync(join(tmpdir(), 'ccgp-npm-'));
after(() => rmSync(work, { recursive: true, force: true }));

function fakeBinaries(dir) {
  for (const p of PLATFORMS) {
    mkdirSync(join(dir, `bin-${p.id}`), { recursive: true });
    writeFileSync(join(dir, `bin-${p.id}`, p.exe), `binary ${p.id}`);
    writeFileSync(join(dir, `bin-${p.id}`, p.yexe), `ccgpy ${p.id}`);
  }
}
const json = (...p) => JSON.parse(readFileSync(join(...p), 'utf8'));

test('builds one package per platform plus the main one, main last', () => {
  const bin = join(work, 'bin1');
  fakeBinaries(bin);
  const out = join(work, 'out1');
  const dirs = build({ version: '1.2.3', bin, out });

  assert.equal(dirs.length, PLATFORMS.length + 1);
  assert.equal(dirs.at(-1), join(out, 'ccgp'));

  const main = json(out, 'ccgp', 'package.json');
  assert.equal(main.name, 'ccgp');
  assert.equal(main.version, '1.2.3');
  assert.equal(main.license, '(MIT OR Apache-2.0)');
  assert.deepEqual(main.bin, { ccgp: 'bin/ccgp.js', ccgpy: 'bin/ccgpy.js' });
  assert.deepEqual(
    main.optionalDependencies,
    Object.fromEntries(PLATFORMS.map((p) => [`ccgp-${p.id}`, '1.2.3'])),
  );

  for (const p of PLATFORMS) {
    const pkg = json(out, `ccgp-${p.id}`, 'package.json');
    assert.deepEqual([pkg.os, pkg.cpu], [[p.os], [p.cpu]]);
    assert.equal(pkg.version, '1.2.3');
    assert.equal(readFileSync(join(out, `ccgp-${p.id}`, 'bin', p.exe), 'utf8'), `binary ${p.id}`);
    assert.equal(readFileSync(join(out, `ccgp-${p.id}`, 'bin', p.yexe), 'utf8'), `ccgpy ${p.id}`);
    for (const f of ['LICENSE-MIT', 'LICENSE-APACHE', 'NOTICE']) {
      assert.ok(existsSync(join(out, `ccgp-${p.id}`, f)), `${p.id} lacks ${f}`);
    }
  }

  const launcher = readFileSync(join(out, 'ccgp', 'bin', 'ccgp.js'), 'utf8');
  assert.ok(!launcher.includes('__PREFIX__'));
  assert.ok(launcher.includes('`ccgp-${key}`'));
  // The same launcher is installed under both bin names.
  assert.equal(readFileSync(join(out, 'ccgp', 'bin', 'ccgpy.js'), 'utf8'), launcher);
});

test('the package name is configurable', () => {
  const bin = join(work, 'bin2');
  fakeBinaries(bin);
  const out = join(work, 'out2');
  build({ version: '0.1.0-rc.1', bin, out, name: '@acme/ccgp' });
  const main = json(out, '@acme/ccgp', 'package.json');
  assert.equal(main.name, '@acme/ccgp');
  assert.ok('@acme/ccgp-win32-x64' in main.optionalDependencies);
});

test('rejects a bad version and a missing binary', () => {
  const bin = join(work, 'bin3');
  fakeBinaries(bin);
  assert.throws(() => build({ version: 'v1.2.3', bin, out: join(work, 'o3') }), /invalid version/);
  assert.throws(() => build({ version: '1.2', bin, out: join(work, 'o3') }), /invalid version/);
  rmSync(join(bin, 'bin-win32-arm64'), { recursive: true });
  assert.throws(() => build({ version: '1.2.3', bin, out: join(work, 'o3') }), /missing binary for win32-arm64/);

  const bin4 = join(work, 'bin4');
  fakeBinaries(bin4);
  const lacking = PLATFORMS.find((p) => p.id === 'linux-arm64');
  rmSync(join(bin4, 'bin-linux-arm64', lacking.yexe));
  assert.throws(() => build({ version: '1.2.3', bin: bin4, out: join(work, 'o4') }), /missing ccgpy binary for linux-arm64/);
});

test('launcher explains a missing platform package', () => {
  const dir = join(work, 'bare');
  mkdirSync(dir, { recursive: true });
  const src = readFileSync(join(HERE, 'launcher.js'), 'utf8').replaceAll('__PREFIX__', 'ccgp');
  writeFileSync(join(dir, 'ccgp.js'), src);
  const r = spawnSync(process.execPath, [join(dir, 'ccgp.js'), 'version'], { encoding: 'utf8' });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /no prebuilt binary for/);
  assert.match(r.stderr, /go install github\.com\/PHPCraftdream\/cc-go-proxy\/cmd\/ccgp@latest/);
});

test('launcher runs the platform binary and passes arguments and the exit code through', { skip: process.platform === 'win32' }, () => {
  const dir = join(work, 'run');
  const pkgDir = join(dir, 'node_modules', `ccgp-${process.platform}-${process.arch}`, 'bin');
  mkdirSync(pkgDir, { recursive: true });
  symlinkSync(process.execPath, join(pkgDir, 'ccgp')); // node itself stands in for the binary
  const src = readFileSync(join(HERE, 'launcher.js'), 'utf8').replaceAll('__PREFIX__', 'ccgp');
  writeFileSync(join(dir, 'ccgp.js'), src);
  const r = spawnSync(process.execPath, [join(dir, 'ccgp.js'), '-e', 'console.log("hi"); process.exit(7)'], { encoding: 'utf8' });
  assert.equal(r.stdout.trim(), 'hi');
  assert.equal(r.status, 7);
});

test('the launcher started as ccgpy.js runs the ccgpy binary', { skip: process.platform === 'win32' }, () => {
  const dir = join(work, 'runy');
  const pkgDir = join(dir, 'node_modules', `ccgp-${process.platform}-${process.arch}`, 'bin');
  mkdirSync(pkgDir, { recursive: true });
  symlinkSync(process.execPath, join(pkgDir, 'ccgpy')); // only ccgpy exists: ccgp.js would fail
  const src = readFileSync(join(HERE, 'launcher.js'), 'utf8').replaceAll('__PREFIX__', 'ccgp');
  writeFileSync(join(dir, 'ccgpy.js'), src);
  const r = spawnSync(process.execPath, [join(dir, 'ccgpy.js'), '-e', 'console.log("yolo"); process.exit(3)'], { encoding: 'utf8' });
  assert.equal(r.stdout.trim(), 'yolo');
  assert.equal(r.status, 3);
});

test('the ccgpy launcher names itself in the missing-binary message', () => {
  const dir = join(work, 'barey');
  mkdirSync(dir, { recursive: true });
  const src = readFileSync(join(HERE, 'launcher.js'), 'utf8').replaceAll('__PREFIX__', 'ccgp');
  writeFileSync(join(dir, 'ccgpy.js'), src);
  const r = spawnSync(process.execPath, [join(dir, 'ccgpy.js')], { encoding: 'utf8' });
  assert.equal(r.status, 1);
  assert.match(r.stderr, /^ccgpy: there is no prebuilt binary for/);
});
