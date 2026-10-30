#!/usr/bin/env bash
# Runs the Go tests a push or pull request can affect (scripts/ci/affected.mjs
# picks them from the diff against CI_BASE_SHA). No base (a scheduled or manual
# run) means the whole suite, with coverage.
# GITHUB_OUTPUT gets full=true|false: a partial run must not upload coverage.
set -euo pipefail

mapfile -t pkgs < <(node scripts/ci/affected.mjs "${CI_BASE_SHA:-}")

out() { if [ -n "${GITHUB_OUTPUT:-}" ]; then echo "$1" >>"$GITHUB_OUTPUT"; fi; }

if [ "${#pkgs[@]}" -eq 0 ]; then
  out "full=false"
  echo "no Go package is touched: no tests to run"
  exit 0
fi

if [ "${pkgs[0]}" = "./..." ]; then
  out "full=true"
  if [ "${RUNNER_OS:-}" = "Linux" ]; then
    exec go test -v -race -coverprofile=coverage.out ./...
  fi
  exec go test -v ./...
fi

out "full=false"
if [ "${RUNNER_OS:-}" = "Linux" ]; then
  exec go test -v -race "${pkgs[@]}"
fi
exec go test -v "${pkgs[@]}"
