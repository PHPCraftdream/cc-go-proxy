import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { build, dirName, PLATFORMS } from './build.mjs';

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

// npm's spam filter refuses <name>-win32-<cpu>; the launcher must look up the same ids.
test('platform ids use w32 for Windows, as the launcher does', () => {
  for (const p of PLATFORMS) {
    assert.equal(p.id, `${p.os === 'win32' ? 'w32' : p.os}-${p.cpu}`);
    assert.ok(!p.id.includes('win32'), p.id);
  }
  const src = readFileSync(join(HERE, 'launcher.js'), 'utf8');
  assert.ok(src.includes("process.platform === 'win32' ? 'w32' : process.platform"));
});

test('the package name is configurable', () => {
  const bin = join(work, 'bin2');
  fakeBinaries(bin);
  const out = join(work, 'out2');
  const dirs = build({ version: '0.1.0-rc.1', bin, out, name: '@acme/ccgp' });
  const main = json(out, 'acme__ccgp', 'package.json');
  assert.equal(main.name, '@acme/ccgp');
  assert.ok('@acme/ccgp-w32-x64' in main.optionalDependencies);
  assert.equal(dirs.at(-1), join(out, 'acme__ccgp'));
  assert.equal(json(out, 'acme__ccgp-w32-x64', 'package.json').name, '@acme/ccgp-w32-x64');
});

// publish.mjs walks the output directory one level deep: a scoped build must not nest.
test('a scoped build keeps the output flat', () => {
  const bin = join(work, 'bin2b');
  fakeBinaries(bin);
  const out = join(work, 'out2b');
  build({ version: '1.0.0', bin, out, name: '@acme/ccgp' });
  const entries = readdirSync(out);
  assert.equal(entries.length, PLATFORMS.length + 1);
  for (const e of entries) {
    assert.ok(!e.startsWith('@'), `${e} nests packages`);
    assert.ok(existsSync(join(out, e, 'package.json')), `${e} has no package.json`);
  }
  assert.equal(dirName('@acme/ccgp'), 'acme__ccgp');
  assert.equal(dirName('ccgp'), 'ccgp');
});

// A scoped package lives in node_modules/@scope/: the launcher must find it there.
test('the launcher finds a scoped platform package', { skip: process.platform === 'win32' }, () => {
  const dir = join(work, 'runscoped');
  const pkgDir = join(dir, 'node_modules', '@acme', `ccgp-${process.platform}-${process.arch}`, 'bin');
  mkdirSync(pkgDir, { recursive: true });
  symlinkSync(process.execPath, join(pkgDir, 'ccgp'));
  const src = readFileSync(join(HERE, 'launcher.js'), 'utf8').replaceAll('__PREFIX__', '@acme/ccgp');
  writeFileSync(join(dir, 'ccgp.js'), src);
  const r = spawnSync(process.execPath, [join(dir, 'ccgp.js'), '-e', 'console.log("scoped"); process.exit(5)'], { encoding: 'utf8' });
  assert.equal(r.status, 5, r.stderr);
  assert.match(r.stdout, /scoped/);
});

test('rejects a bad version and a missing binary', () => {
  const bin = join(work, 'bin3');
  fakeBinaries(bin);
  assert.throws(() => build({ version: 'v1.2.3', bin, out: join(work, 'o3') }), /invalid version/);
  assert.throws(() => build({ version: '1.2', bin, out: join(work, 'o3') }), /invalid version/);
  rmSync(join(bin, 'bin-w32-arm64'), { recursive: true });
  assert.throws(() => build({ version: '1.2.3', bin, out: join(work, 'o3') }), /missing binary for w32-arm64/);

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
