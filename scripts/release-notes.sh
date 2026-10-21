#!/usr/bin/env bash
# Prints the GitHub release notes for a version: its CHANGELOG.md section (or a
# pointer to the changelog when there is none) followed by install instructions.
#
#   scripts/release-notes.sh 1.2.3 [owner/repo]
set -euo pipefail

version="${1:?usage: release-notes.sh <version> [owner/repo]}"
repo="${2:-${GITHUB_REPOSITORY:-PHPCraftdream/cc-go-proxy}}"
cd "$(dirname "$0")/.."

section=""
if [ -f CHANGELOG.md ]; then
  section="$(awk -v v="$version" '
    index($0, "## [" v "]") == 1 { found = 1; next }
    found && /^## \[/ { exit }
    found { print }
  ' CHANGELOG.md | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}')"
fi

if [ -n "$section" ]; then
  printf '%s\n\n' "$section"
else
  printf 'See [CHANGELOG.md](https://github.com/%s/blob/v%s/CHANGELOG.md).\n\n' "$repo" "$version"
fi

cat <<EOF
## Install

\`\`\`sh
# npm (any platform)
npm install -g ccgp@${version}

# Go
go install github.com/${repo}/cmd/ccgp@v${version}
\`\`\`

Or download the archive for your platform below, unpack it and put \`ccgp\` on your \`PATH\`.

### Verify the download

\`\`\`sh
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify ccgp_${version}_<os>_<arch>.<tar.gz|zip> --repo ${repo}
\`\`\`

The archives are built by GitHub Actions from this tag; the attestation proves it.
EOF
