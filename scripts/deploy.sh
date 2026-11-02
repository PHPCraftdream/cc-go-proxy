#!/usr/bin/env sh
# Build ccgp and ccgpy, install them and restart the background server.
# Run it from a plain terminal, not under wnice / capc: the restarted server
# would stay in that job (Idle priority, CPU and memory caps).
#   scripts/deploy.sh
# Installs into $CCGP_BIN, default $HOME/bin.
set -eu
cd "$(dirname "$0")/.."

BIN="${CCGP_BIN:-$HOME/bin}"
EXT=""
case "$(uname -s)" in MINGW* | MSYS* | CYGWIN*) EXT=".exe" ;; esac

mkdir -p dist "$BIN"
echo "== build"
go build -ldflags "-s -w" -o "dist/ccgp$EXT" ./cmd/ccgp
go build -ldflags "-s -w" -o "dist/ccgpy$EXT" ./cmd/ccgpy

echo "== install into $BIN"
ts=$(date +%H%M%S)
for n in ccgp ccgpy; do
  if [ -n "$EXT" ]; then
    # Windows: a running exe cannot be overwritten, only renamed.
    if [ -e "$BIN/$n$EXT" ]; then mv "$BIN/$n$EXT" "$BIN/$n.old-$ts$EXT"; fi
    cp "dist/$n$EXT" "$BIN/$n$EXT"
  else
    # Unix: replace atomically, a running binary keeps its old inode.
    cp "dist/$n" "$BIN/$n.new" && mv -f "$BIN/$n.new" "$BIN/$n"
  fi
done

echo "== restart"
"$BIN/ccgp$EXT" --restart
"$BIN/ccgp$EXT" --status
