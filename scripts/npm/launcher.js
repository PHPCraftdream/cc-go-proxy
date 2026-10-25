#!/usr/bin/env node
// Starts the prebuilt ccgp binary that npm installed for this platform as an
// optional dependency (__PREFIX__-<platform>-<arch>, w32 for win32), and passes
// arguments, stdio, signals and the exit code through.
'use strict';

const { spawn } = require('node:child_process');
const { chmodSync } = require('node:fs');
const { basename } = require('node:path');

// Windows packages are named w32-<arch> (see PLATFORMS in build.mjs).
const key = `${process.platform === 'win32' ? 'w32' : process.platform}-${process.arch}`;
const pkg = `__PREFIX__-${key}`;
// bin/ccgpy.js starts the ccgpy binary (ccgp with permission prompts off), bin/ccgp.js starts ccgp.
const name = basename(__filename, '.js') === 'ccgpy' ? 'ccgpy' : 'ccgp';
const exe = process.platform === 'win32' ? `${name}.exe` : name;

let bin;
try {
  bin = require.resolve(`${pkg}/bin/${exe}`);
} catch {
  console.error(`${name}: there is no prebuilt binary for ${key} (${pkg} is not installed).`);
  console.error('If npm skipped optional dependencies (--omit=optional), install again without that flag.');
  console.error('Otherwise use a release archive: https://github.com/PHPCraftdream/cc-go-proxy/releases');
  console.error('or: go install github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp@latest');
  process.exit(1);
}

if (process.platform !== 'win32') {
  try {
    chmodSync(bin, 0o755); // npm does not keep the executable bit of non-bin files
  } catch {
    // read-only install: the bit may already be set
  }
}

const child = spawn(bin, process.argv.slice(2), { stdio: 'inherit' });

// Ctrl+C reaches the child through the terminal already; do not deliver it twice.
process.on('SIGINT', () => {});
for (const sig of ['SIGTERM', 'SIGHUP']) {
  process.on(sig, () => {
    try {
      child.kill(sig);
    } catch {
      // already gone
    }
  });
}

child.on('error', (err) => {
  console.error(`${name}: cannot start ${bin}: ${err.message}`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal) {
    process.removeAllListeners(signal);
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code === null ? 1 : code);
});
