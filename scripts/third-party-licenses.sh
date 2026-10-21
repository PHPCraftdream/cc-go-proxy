#!/usr/bin/env bash
# Writes THIRD_PARTY_LICENSES.md: every library linked into ccgp with its
# license and the full license text, as MIT/BSD/Apache require for binaries.
#
#   scripts/third-party-licenses.sh [output-file]
set -euo pipefail

out="${1:-THIRD_PARTY_LICENSES.md}"
cd "$(dirname "$0")/.."
tpl="$(pwd)/scripts/licenses.tpl"

rows="$(go run github.com/google/go-licenses/v2@v2.0.1 report ./cmd/ccgp --ignore github.com/PHPCraftdream/cc-go-proxy --template="$tpl" 2>/dev/null | sort -u | grep -v '^$')"

{
  echo "# Third-party licenses"
  echo
  echo "ccgp links the libraries below. Each is distributed under the license shown."
  echo
  echo "| Library | Version | License |"
  echo "| --- | --- | --- |"
  while IFS='|' read -r name version license _; do
    echo "| \`$name\` | $version | $license |"
  done <<<"$rows"
  echo
  while IFS='|' read -r name version license path; do
    echo "## $name $version — $license"
    echo
    echo '```text'
    if [ -f "$path" ]; then tr -d '\r' <"$path"; else echo "(license file not found: $path)"; fi
    echo '```'
    echo
  done <<<"$rows"
} >"$out"

echo "wrote $out ($(grep -c '^## ' "$out") libraries)"
