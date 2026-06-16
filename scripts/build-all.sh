#!/usr/bin/env bash
# Build go-frog for the current platform (GUI requires CGo for Fyne).
#
# Cross-compiled dist/ binaries are not produced here because Fyne uses CGo,
# which requires a platform-specific C toolchain. To distribute for multiple
# platforms, build natively on each target, or use a CGo cross-compiler
# solution (e.g. zig cc via CC=zig cc).
#
# Usage: chmod +x scripts/build-all.sh && ./scripts/build-all.sh

set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

mkdir -p dist

echo "Building go-frog (native) ..."
go build -trimpath -ldflags='-s -w' -o "dist/go-frog" .

echo "Done. Binary at dist/go-frog"
