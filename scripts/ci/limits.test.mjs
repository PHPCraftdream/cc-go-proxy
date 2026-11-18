import assert from 'node:assert/strict';
import test from 'node:test';
import { check, countLines, format, isCode, MAX_FILES, MAX_LINES } from './limits.mjs';

test('countLines counts like wc -l and an unterminated last line', () => {
  assert.equal(countLines(''), 0);
  assert.equal(countLines('a'), 1);
  assert.equal(countLines('a\n'), 1);
  assert.equal(countLines('a\nb'), 2);
  assert.equal(countLines('a\nb\n'), 2);
  assert.equal(countLines('\n\n\n'), 3);
});

test('isCode takes source files and leaves docs, data and fixtures out', () => {
  for (const f of ['a.go', 'cmd/x/main.go', 'scripts/x.mjs', 'install.sh', 'install.ps1', 'scripts/deploy.cmd', 'a/b_test.go']) {
    assert.equal(isCode(f), true, f);
  }
  for (const f of ['README.md', 'go.mod', 'config.ktav', 'ci.yml', 'internal/x/testdata/a.go', 'vendor/y/z.go', 'a/node_modules/b.js']) {
    assert.equal(isCode(f), false, f);
  }
});

test('a file over the line limit is reported, one at the limit is not', () => {
  const v = check([
    { file: 'a/long.go', lines: MAX_LINES + 1 },
    { file: 'a/exact.go', lines: MAX_LINES },
  ]);
  assert.deepEqual(v, [{ kind: 'lines', path: 'a/long.go', count: MAX_LINES + 1 }]);
});

test('a directory over the file limit is reported with its files, one at the limit is not', () => {
  const names = (n, dir) => Array.from({ length: n }, (_, i) => ({ file: `${dir}/f${i}.go`, lines: 10 }));
  const v = check([...names(MAX_FILES, 'ok'), ...names(MAX_FILES + 1, 'full')]);
  assert.equal(v.length, 1);
  assert.equal(v[0].kind, 'files');
  assert.equal(v[0].path, 'full');
  assert.equal(v[0].count, MAX_FILES + 1);
  assert.equal(v[0].files.length, MAX_FILES + 1);
});

test('subdirectories do not count toward their parent, and the root is ".":', () => {
  const entries = [
    ...Array.from({ length: MAX_FILES }, (_, i) => ({ file: `p/f${i}.go`, lines: 1 })),
    ...Array.from({ length: MAX_FILES }, (_, i) => ({ file: `p/sub/f${i}.go`, lines: 1 })),
    ...Array.from({ length: MAX_FILES + 1 }, (_, i) => ({ file: `r${i}.sh`, lines: 1 })),
  ];
  const v = check(entries);
  assert.deepEqual(v.map((x) => x.path), ['.']);
});

test('limits can be set, and the report names both kinds', () => {
  const v = check(
    [
      { file: 'd/a.go', lines: 5 },
      { file: 'd/b.go', lines: 2 },
    ],
    { maxLines: 3, maxFiles: 1 },
  );
  const text = format(v, { maxLines: 3, maxFiles: 1 });
  assert.match(text, /d\/a\.go: 5 lines \(limit 3\)/);
  assert.match(text, /d\/: 2 code files \(limit 1\)/);
});
