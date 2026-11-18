#!/usr/bin/env node
// Size limits of the source tree: at most MAX_LINES lines in a file and at most
// MAX_FILES code files in a directory (tests included, subdirectories not). A file
// or directory over a limit is split; in Go a subdirectory is a package of its own.
//
//   node scripts/ci/limits.mjs
//
// Checks the files tracked by git. Exits 1 and lists every violation.
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

export const MAX_LINES = 1000;
export const MAX_FILES = 7;

const CODE = /\.(go|mjs|cjs|js|ts|sh|cmd|bat|ps1|py)$/i;
// Fixtures and third-party trees are not ours to split.
const SKIPPED = /(^|\/)(testdata|vendor|node_modules)\//;

// isCode reports whether a '/'-separated path counts toward the limits.
export function isCode(file) {
  return CODE.test(file) && !SKIPPED.test(file);
}

// countLines counts lines as wc -l does, plus an unterminated last line.
export function countLines(text) {
  if (text === '') return 0;
  const breaks = text.split('\n').length - 1;
  return text.endsWith('\n') ? breaks : breaks + 1;
}

// check takes [{ file, lines }] of code files and returns the violations as
// { kind: 'lines' | 'files', path, count, files? }, longest files and fullest
// directories first.
export function check(entries, { maxLines = MAX_LINES, maxFiles = MAX_FILES } = {}) {
  const out = [];
  const dirs = new Map();
  for (const { file, lines } of entries) {
    if (lines > maxLines) out.push({ kind: 'lines', path: file, count: lines });
    const dir = file.includes('/') ? file.slice(0, file.lastIndexOf('/')) : '.';
    dirs.set(dir, [...(dirs.get(dir) ?? []), file]);
  }
  for (const [dir, files] of dirs) {
    if (files.length > maxFiles) out.push({ kind: 'files', path: dir, count: files.length, files: files.sort() });
  }
  return out.sort((a, b) => a.kind.localeCompare(b.kind) || b.count - a.count || a.path.localeCompare(b.path));
}

export function format(violations, { maxLines = MAX_LINES, maxFiles = MAX_FILES } = {}) {
  return violations
    .map((v) =>
      v.kind === 'lines'
        ? `${v.path}: ${v.count} lines (limit ${maxLines})`
        : `${v.path}/: ${v.count} code files (limit ${maxFiles})`,
    )
    .join('\n');
}

function tracked() {
  const out = execFileSync('git', ['ls-files', '-z'], { encoding: 'utf8', maxBuffer: 64 << 20 });
  return out.split('\0').filter((f) => f && isCode(f));
}

function main() {
  const entries = tracked().map((file) => ({ file, lines: countLines(readFileSync(file, 'utf8')) }));
  const violations = check(entries);
  if (violations.length === 0) {
    console.log(`ok: ${entries.length} code files, none over ${MAX_LINES} lines, no directory over ${MAX_FILES} files`);
    return;
  }
  console.error(format(violations));
  console.error(
    `\n${violations.length} violation(s). Split the file, or move part of the directory into a subdirectory (a package of its own in Go).`,
  );
  process.exit(1);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) main();
