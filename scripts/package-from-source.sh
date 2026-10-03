#!/usr/bin/env bash
# Builds OneCamp from this repository and lays it out exactly as the official
# release does, so `make install` works on it the same way.
#
#   scripts/package-from-source.sh [output-dir]     (default: ./onecamp-install)
#
# Then copy the directory to your server (or run this there) and follow
# https://onemana.dev/docs/installation from step 2: `make install ...`.
#
# Needs Go 1.25 or newer. The binary is static (CGO off), because the image it
# runs in is Alpine. A build from source has no seat limit and no licence key;
# it is AGPL-3.0, like the code.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT/onecamp-install}"
cd "$ROOT"

VERSION="$(git describe --tags --abbrev=0 2>/dev/null || true)"
ENV_TEMPLATE=".sample.env"
[ -f "$ENV_TEMPLATE" ] || ENV_TEMPLATE="vars/.env.prod"
[ -f "$ENV_TEMPLATE" ] || { echo "no environment template (.sample.env) in this checkout" >&2; exit 1; }

if [ -e "$OUT" ] && [ -n "$(ls -A "$OUT" 2>/dev/null)" ]; then
  echo "$OUT is not empty; choose another directory or remove it first" >&2
  exit 1
fi
mkdir -p "$OUT"

echo "building the server ${VERSION:-from source}…"
LDFLAGS="-s -w"
[ -n "$VERSION" ] && LDFLAGS="$LDFLAGS -X github.com/akashc777/OneCamp/helpers.ReleaseVersion=$VERSION"
CGO_ENABLED=0 GOOS=linux go build -ldflags "$LDFLAGS" -o "$OUT/app" ./cmd/server

# The same files, under the same names, as the official release archive.
cp distribute-Dockerfile "$OUT/Dockerfile"
cp distribute-compose.yml "$OUT/sample-compose.yml"
cp Makefile-distribute "$OUT/Makefile"
cp .dockerignore "$OUT/.dockerignore"
cp "$ENV_TEMPLATE" "$OUT/.sample.env"
cp livekit.yaml.sample "$OUT/livekit.yaml.sample"
for d in migrations ch-docker emqx livekit-agent other-services; do
  cp -R "$d" "$OUT/$d"
done
echo "${VERSION:-source}" > "$OUT/version.txt"

echo "ready: $OUT"
echo "next, on your server in that directory:"
echo "  make install EMAIL=you@example.com            # free address, no DNS needed"
echo "  make install EMAIL=you@example.com DOMAIN=example.com   # your own domain"
