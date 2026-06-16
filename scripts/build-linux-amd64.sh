#!/usr/bin/env bash
# Build a linux/amd64 wacli binary (glibc, Debian-based) with the vendored, patched
# whatsmeow that fixes PN<->LID poll-vote decryption. Intended for dropping into the
# gateway container at /home/node/.local/bin/wacli without touching the brew install.
#
# Usage (from the repo root):
#   scripts/build-linux-amd64.sh                # -> dist/wacli-linux-amd64
#   OUT=dist/wacli-patched scripts/build-linux-amd64.sh
#
# Requires Docker. The whole repo (including third_party/whatsmeow) is mounted, so the
# go.mod `replace go.mau.fi/whatsmeow => ./third_party/whatsmeow` resolves with no clone.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
out="${OUT:-dist/wacli-linux-amd64}"
go_image="${GO_IMAGE:-golang:1.25}"   # Debian (glibc) toolchain, matches the gateway
# Distinct version so `wacli --version` confirms the patched binary is the one running.
version="${VERSION:-0.11.1-pollvote-lid-v3}"

mkdir -p "$repo_root/$(dirname "$out")"

# --platform linux/amd64 runs an amd64-native (emulated) container so CGO uses the
# native amd64 gcc. A native arm64 gcc cannot cross-compile go-sqlite3 to amd64.
docker run --rm \
  --platform linux/amd64 \
  -v "$repo_root":/src \
  -w /src \
  -e CGO_ENABLED=1 \
  -e GOOS=linux \
  -e GOARCH=amd64 \
  -e CGO_CFLAGS="-Wno-error=missing-braces" \
  -e WACLI_VERSION="$version" \
  "$go_image" \
  sh -ceu '
    apt-get update >/dev/null && apt-get install -y --no-install-recommends gcc libc6-dev >/dev/null
    go build -tags sqlite_fts5 -trimpath -ldflags="-s -w -X main.version=${WACLI_VERSION}" -o "'"$out"'" ./cmd/wacli
  '

echo "Built: $repo_root/$out"
file "$repo_root/$out" 2>/dev/null || true
