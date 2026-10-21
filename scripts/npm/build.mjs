#!/usr/bin/env node
// Builds the npm packages of a release from the compiled binaries.
//
//   node scripts/npm/build.mjs --version 1.2.3 --bin <dir> --out <dir> [--name ccgp]
//
// <dir> holds one sub-directory per platform, "bin-<id>/ccgp[.exe]" (what
// actions/download-artifact produces for the build matrix). Output:
//
//   <out>/<name>/            main package: launcher + optionalDependencies
//   <out>/<name>-<id>/       one package per platform with the binary
//
// The launcher resolves the platform package at run time (the esbuild layout),
// so installs work with --ignore-scripts and without a download step.
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

export const PLATFORMS = [
  { id: 'linux-x64', os: 'linux', cpu: 'x64', exe: 'ccgp', label: 'Linux x64' },
  { id: 'linux-arm64', os: 'linux', cpu: 'arm64', exe: 'ccgp', label: 'Linux arm64' },
  { id: 'darwin-x64', os: 'darwin', cpu: 'x64', exe: 'ccgp', label: 'macOS x64' },
  { id: 'darwin-arm64', os: 'darwin', cpu: 'arm64', exe: 'ccgp', label: 'macOS arm64' },
  { id: 'win32-x64', os: 'win32', cpu: 'x64', exe: 'ccgp.exe', label: 'Windows x64' },
  { id: 'win32-arm64', os: 'win32', cpu: 'arm64', exe: 'ccgp.exe', label: 'Windows arm64' },
];

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, '..', '..');
const LICENSE_FILES = ['LICENSE-MIT', 'LICENSE-APACHE', 'NOTICE'];

/** @returns {string[]} package directories in publish order: platforms first, main last. */
export function build({ version, bin, out, name = 'ccgp', repo = 'PHPCraftdream/cc-go-proxy', root = ROOT, licenses }) {
  if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version ?? '')) {
    throw new Error(`invalid version: ${version}`);
  }
  const common = {
    version,
    license: '(MIT OR Apache-2.0)',
    author: 'PHPCraftdream',
    homepage: `https://github.com/${repo}#readme`,
    bugs: { url: `https://github.com/${repo}/issues` },
    repository: { type: 'git', url: `git+https://github.com/${repo}.git` },
  };
  const thirdParty = licenses ?? join(root, 'THIRD_PARTY_LICENSES.md');

  const copyLegal = (dir) => {
    for (const f of LICENSE_FILES) copyFileSync(join(root, f), join(dir, f));
    if (existsSync(thirdParty)) copyFileSync(thirdParty, join(dir, 'THIRD_PARTY_LICENSES.md'));
  };
  const write = (dir, pkg) => writeFileSync(join(dir, 'package.json'), JSON.stringify(pkg, null, 2) + '\n');

  rmSync(out, { recursive: true, force: true });
  const dirs = [];

  for (const p of PLATFORMS) {
    const src = join(bin, `bin-${p.id}`, p.exe);
    if (!existsSync(src)) throw new Error(`missing binary for ${p.id}: ${src}`);
    const pkgName = `${name}-${p.id}`;
    const dir = join(out, pkgName);
    mkdirSync(join(dir, 'bin'), { recursive: true });
    copyFileSync(src, join(dir, 'bin', p.exe));
    copyLegal(dir);
    writeFileSync(
      join(dir, 'README.md'),
      `# ${pkgName}\n\nThe ${p.label} binary of [ccgp](https://github.com/${repo}). ` +
        `Do not install this package directly: install \`${name}\`, which selects the right one.\n`,
    );
    write(dir, {
      name: pkgName,
      ...common,
      description: `ccgp binary for ${p.label}`,
      os: [p.os],
      cpu: [p.cpu],
      files: ['bin', ...LICENSE_FILES, 'THIRD_PARTY_LICENSES.md'],
    });
    dirs.push(dir);
  }

  const main = join(out, name);
  mkdirSync(join(main, 'bin'), { recursive: true });
  const launcher = readFileSync(join(HERE, 'launcher.js'), 'utf8').replaceAll('__PREFIX__', name);
  writeFileSync(join(main, 'bin', 'ccgp.js'), launcher, { mode: 0o755 });
  copyLegal(main);
  copyFileSync(join(root, 'README.md'), join(main, 'README.md'));
  write(main, {
    name,
    ...common,
    description: 'Run Claude Code on ChatGPT Codex, OpenAI-compatible APIs and z.ai GLM: a headless Go bridge with token-in-URL auth and per-profile model mapping',
    keywords: ['claude-code', 'anthropic', 'openai', 'codex', 'proxy', 'llm', 'gateway', 'glm', 'z.ai', 'oauth', 'cli'],
    bin: { ccgp: 'bin/ccgp.js' },
    files: ['bin', ...LICENSE_FILES, 'THIRD_PARTY_LICENSES.md'],
    engines: { node: '>=18' },
    optionalDependencies: Object.fromEntries(PLATFORMS.map((p) => [`${name}-${p.id}`, version])),
  });
  dirs.push(main);
  return dirs;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const { values } = parseArgs({
    options: {
      version: { type: 'string' },
      bin: { type: 'string' },
      out: { type: 'string' },
      name: { type: 'string', default: 'ccgp' },
      repo: { type: 'string', default: 'PHPCraftdream/cc-go-proxy' },
    },
  });
  for (const k of ['version', 'bin', 'out']) {
    if (!values[k]) {
      console.error(`usage: build.mjs --version X.Y.Z --bin <dir> --out <dir> [--name ccgp] [--repo owner/name]  (missing --${k})`);
      process.exit(2);
    }
  }
  const dirs = build({ ...values, bin: resolve(values.bin), out: resolve(values.out) });
  for (const d of dirs) console.log(d);
}
