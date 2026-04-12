#!/usr/bin/env bash
# Cross-compile go-frog for Windows (amd64) and macOS (Intel + Apple Silicon).
# Usage: chmod +x scripts/build-all.sh && ./scripts/build-all.sh

set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

mkdir -p dist
export CGO_ENABLED=0

build_one() {
  local goos=$1 goarch=$2 out=$3
  echo "Building dist/${out} ..."
  GOOS="${goos}" GOARCH="${goarch}" go build -trimpath -ldflags='-s -w' -o "dist/${out}" .
}

build_one windows amd64 go-frog-windows-amd64.exe
build_one darwin arm64 go-frog-darwin-arm64
build_one darwin amd64 go-frog-darwin-amd64

echo "Done. Outputs in dist/"
