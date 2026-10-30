#!/usr/bin/env node
// Picks the Go packages a change can break: the packages with changed files and
// every package that imports them, directly or not (tests included).
//
//   node scripts/ci/affected.mjs <base-commit>      (COMMIT_MESSAGE: the pushed head commit's)
//
// Prints "./..." (everything), or one import path per line, or nothing (no Go
// package is touched: docs, npm scripts, other workflows). Anything it cannot
// place with certainty means everything: no base, go.mod/go.sum, the CI files,
// a Go file outside every listed package. So does the marker [ci full] in the
// commit message, the explicit way to ask for the whole suite.
import { execFileSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';

// Changes that decide how the whole suite is built or run.
const EVERYTHING = [/^go\.(mod|sum)$/, /^\.github\/workflows\/ci\.yml$/, /^scripts\/ci\//, /^\.golangci/];

// wantsFull reports whether a commit message asks for the whole suite: [ci full].
export function wantsFull(message) {
  return /\[ci[: ]+full\]/i.test(message ?? '');
}

// files: changed paths, '/'-separated from the module root.
// pkgs: [{ path, dir, imports }], dir is relative to the module root ('.' for the root).
// Returns { full: true, reason } or { full: false, packages }.
export function affected(files, pkgs) {
  const byDir = [...pkgs].sort((a, b) => b.dir.length - a.dir.length);
  const owner = (f) =>
    byDir.find((p) => (p.dir === '.' ? !f.includes('/') : f === p.dir || f.startsWith(p.dir + '/')));

  const changed = new Set();
  for (const f of files) {
    if (EVERYTHING.some((re) => re.test(f))) return { full: true, reason: `${f} changed` };
    const p = owner(f);
    if (p) changed.add(p.path);
    else if (f.endsWith('.go')) return { full: true, reason: `${f} belongs to no listed package` };
  }

  const importers = new Map();
  for (const p of pkgs) for (const i of p.imports) importers.set(i, [...(importers.get(i) ?? []), p.path]);
  const out = new Set(changed);
  const queue = [...changed];
  while (queue.length) {
    for (const up of importers.get(queue.pop()) ?? []) {
      if (!out.has(up)) {
        out.add(up);
        queue.push(up);
      }
    }
  }
  return { full: false, packages: [...out].sort() };
}

function git(...args) {
  return execFileSync('git', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], maxBuffer: 1 << 26 });
}

function goPackages() {
  const slash = (s) => s.trim().replace(/\\/g, '/');
  const root = slash(execFileSync('go', ['list', '-m', '-f', '{{.Dir}}'], { encoding: 'utf8' }));
  const tpl = '{{.ImportPath}}\t{{.Dir}}\t{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}';
  const lines = execFileSync('go', ['list', '-f', tpl, './...'], { encoding: 'utf8', maxBuffer: 1 << 26 })
    .split('\n')
    .filter(Boolean);
  return lines.map((line) => {
    const [path, dir, imports] = line.split('\t');
    const d = slash(dir);
    const rel = d === root ? '.' : d.startsWith(root + '/') ? d.slice(root.length + 1) : d;
    return { path, dir: rel, imports: imports.split(/\s+/).filter(Boolean) };
  });
}

function main() {
  const note = (m) => console.error(`::notice::${m}`);
  const base = process.argv[2] ?? '';
  if (wantsFull(process.env.COMMIT_MESSAGE)) {
    note('tests: everything (the commit message says [ci full])');
    return console.log('./...');
  }
  if (!/^[0-9a-f]{7,64}$/.test(base) || /^0+$/.test(base)) {
    note('tests: everything (no base commit to compare with)');
    return console.log('./...');
  }
  try {
    git('cat-file', '-e', `${base}^{commit}`);
  } catch {
    note('tests: everything (the base commit is not in the checkout)');
    return console.log('./...');
  }
  const files = git('diff', '--name-only', '--no-renames', '-z', base, 'HEAD').split('\0').filter(Boolean);
  const res = affected(files, goPackages());
  if (res.full) {
    note(`tests: everything (${res.reason})`);
    return console.log('./...');
  }
  note(
    res.packages.length
      ? `tests: ${res.packages.length} packages touched by ${files.length} changed files`
      : 'tests: no Go package is touched',
  );
  for (const p of res.packages) console.log(p);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
