import assert from 'node:assert/strict';
import { test } from 'node:test';

import { affected, wantsFull } from './affected.mjs';

const M = 'github.com/x/ccgp';
// config <- server <- cmd/ccgp; connpool <- server; oauth <- codex <- server
const pkgs = [
  { path: `${M}/cmd/ccgp`, dir: 'cmd/ccgp', imports: [`${M}/internal/server`, `${M}/internal/config`] },
  {
    path: `${M}/internal/server`,
    dir: 'internal/server',
    imports: [`${M}/internal/config`, `${M}/internal/codex`, `${M}/internal/connpool`],
  },
  { path: `${M}/internal/codex`, dir: 'internal/codex', imports: [`${M}/internal/oauth`] },
  { path: `${M}/internal/oauth`, dir: 'internal/oauth', imports: [] },
  { path: `${M}/internal/config`, dir: 'internal/config', imports: [] },
  { path: `${M}/internal/connpool`, dir: 'internal/connpool', imports: [] },
  { path: `${M}/internal/sessions`, dir: 'internal/sessions', imports: [] },
];
const short = (r) => r.packages.map((p) => p.slice(M.length + 1));

test('a changed package brings its importers, not its siblings', () => {
  assert.deepEqual(short(affected(['internal/connpool/connpool.go'], pkgs)), [
    'cmd/ccgp',
    'internal/connpool',
    'internal/server',
  ]);
  assert.deepEqual(short(affected(['internal/sessions/a_test.go'], pkgs)), ['internal/sessions']);
});

test('importers are followed through several levels', () => {
  assert.deepEqual(short(affected(['internal/oauth/session.go'], pkgs)), [
    'cmd/ccgp',
    'internal/codex',
    'internal/oauth',
    'internal/server',
  ]);
});

test('a leaf package changes only itself', () => {
  assert.deepEqual(short(affected(['cmd/ccgp/main.go', 'cmd/ccgp/testdata/x.txt'], pkgs)), ['cmd/ccgp']);
});

test('files that no Go test reads select nothing', () => {
  const r = affected(
    ['README.md', 'docs/cli.md', 'scripts/npm/build.mjs', '.github/workflows/release.yml', 'CHANGELOG.md'],
    pkgs,
  );
  assert.equal(r.full, false);
  assert.deepEqual(r.packages, []);
});

test('files of the build or of CI mean everything', () => {
  for (const f of ['go.mod', 'go.sum', '.github/workflows/ci.yml', 'scripts/ci/tests.sh', '.golangci.yml']) {
    assert.equal(affected([f], pkgs).full, true, f);
  }
});

test('a Go file outside every listed package means everything', () => {
  assert.equal(affected(['tools/gen/main.go'], pkgs).full, true);
  assert.equal(affected(['tools/gen/notes.txt'], pkgs).full, false);
});

test('a root package owns only the files in the root', () => {
  const withRoot = [...pkgs, { path: M, dir: '.', imports: [] }];
  assert.deepEqual(affected(['main.go'], withRoot).packages, [M]);
  assert.deepEqual(short(affected(['internal/sessions/a.go'], withRoot)), ['internal/sessions']);
});

test('a package is listed once however many files reach it', () => {
  const r = affected(['internal/config/a.go', 'internal/config/b.go', 'internal/server/c.go'], pkgs);
  assert.deepEqual(short(r), ['cmd/ccgp', 'internal/config', 'internal/server']);
});

test('the commit message asks for the whole suite with [ci full]', () => {
  for (const m of ['x [ci full]', 'x\n\n[CI: full] done', 'x [ci:full]']) assert.equal(wantsFull(m), true, m);
  for (const m of ['', undefined, 'full run', 'x [ci fast]', 'x [tests: full]', 'ci full']) {
    assert.equal(wantsFull(m), false, String(m));
  }
});
