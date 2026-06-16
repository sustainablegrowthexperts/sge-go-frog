#!/usr/bin/env bash
# Cross-compile go-frog for all platforms from a Linux host.
#
# Targets:
#   linux/amd64     — native build (always works)
#   linux/arm64     — needs zig (https://ziglang.org/download/)
#   windows/amd64   — needs zig
#   darwin/amd64    — needs Docker + fyne-cross (see below)
#   darwin/arm64    — needs Docker + fyne-cross (see below)
#
# Prerequisites:
#   Go toolchain
#   zig >= 0.12  (for cross-compiling to linux/arm64 and windows/amd64)
#   Docker       (for macOS targets via fyne-cross — install separately)
#
# Usage:
#   chmod +x scripts/cross-build.sh
#   sudo apt install zig          # or download from ziglang.org
#   ./scripts/cross-build.sh
#
# For linux/arm64 with full OpenGL/X11, you may also need multi-arch system libs:
#   sudo dpkg --add-architecture arm64 && sudo apt update
#   sudo apt install libgl1-mesa-dev:arm64 libx11-dev:arm64 \
#                    libxrandr-dev:arm64 libxcursor-dev:arm64 \
#                    libxinerama-dev:arm64 libxi-dev:arm64

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

mkdir -p dist

# ── helpers ──────────────────────────────────────────────────────────────────
print_banner() {
  echo ""
  echo "================================================"
  echo " go-frog cross-platform build"
  echo "================================================"
}

build_ok()   { echo "    ✓  $1"; }
build_skip() { echo "    –  $1"; }
build_fail() { echo "    ✗  $1"; }

has_zig()   { command -v zig &>/dev/null; }
has_docker(){ command -v docker &>/dev/null; }

BUILD_OPTS="-trimpath -ldflags='-s -w'"

# ── main ─────────────────────────────────────────────────────────────────────
print_banner

n_ok=0
n_skip=0
n_fail=0

# ------------------------------------------------------------------
# 1  linux/amd64 — native build (always works)
# ------------------------------------------------------------------
echo ""
echo "▸ linux/amd64 (native)"
if CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
     go build -trimpath -ldflags='-s -w' -o dist/go-frog-linux-amd64 . 2>/tmp/gf-build.log; then
  build_ok "dist/go-frog-linux-amd64"
  ((n_ok++))
else
  build_fail "native build failed — see /tmp/gf-build.log"
  ((n_fail++))
fi

# ------------------------------------------------------------------
# 2  linux/arm64 — cross-compile with zig cc
# ------------------------------------------------------------------
echo ""
echo "▸ linux/arm64 (zig cc)"
if has_zig; then
  if CC="zig cc -target aarch64-linux-gnu" \
       CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
       go build -trimpath -ldflags='-s -w' -o dist/go-frog-linux-arm64 . 2>/tmp/gf-build.log; then
    build_ok "dist/go-frog-linux-arm64"
    ((n_ok++))
  else
    build_fail "missing arm64 system libraries (libGL, X11, etc.)"
    echo "       Install cross libs:"
    echo "       sudo dpkg --add-architecture arm64 && sudo apt update"
    echo "       sudo apt install libgl1-mesa-dev:arm64 libx11-dev:arm64 \\"
    echo "                        libxrandr-dev:arm64 libxcursor-dev:arm64 \\"
    echo "                        libxinerama-dev:arm64 libxi-dev:arm64"
    ((n_fail++))
  fi
else
  build_skip "install zig (https://ziglang.org/download/) for linux/arm64"
  ((n_skip++))
fi

# ------------------------------------------------------------------
# 3  windows/amd64 — cross-compile with zig cc
# ------------------------------------------------------------------
echo ""
echo "▸ windows/amd64 (zig cc)"
if has_zig; then
  if CC="zig cc -target x86_64-windows-gnu" \
       CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
       go build -trimpath -ldflags='-s -w' -o dist/go-frog-windows-amd64.exe . 2>/tmp/gf-build.log; then
    build_ok "dist/go-frog-windows-amd64.exe"
    ((n_ok++))
  else
    build_fail "zig cc Windows build failed — see /tmp/gf-build.log"
    ((n_fail++))
  fi
else
  build_skip "install zig (https://ziglang.org/download/) for windows/amd64"
  ((n_skip++))
fi

# ------------------------------------------------------------------
# 4  macOS — cross-compile with fyne-cross (needs Docker)
# ------------------------------------------------------------------
echo ""
echo "▸ darwin/amd64 + darwin/arm64 (fyne-cross + Docker)"

if has_docker; then
  # Install fyne-cross if not present
  if ! command -v fyne-cross &>/dev/null; then
    echo "       Installing fyne-cross..."
    go install github.com/fyne-io/fyne-cross@latest 2>/tmp/gf-fyne-install.log || true
  fi

  if command -v fyne-cross &>/dev/null; then
    # macOS ARM (Apple Silicon)
    echo "       → darwin/arm64 ..."
    if fyne-cross darwin -arch=arm64 -output go-frog-darwin-arm64 \
         -app-id com.gofrog -icon="" \
         -trimpath -ldflags="-s -w" . 2>/tmp/gf-build.log; then
      # fyne-cross puts output in fyne-cross/dist/darwin-arm64/
      if [ -f "fyne-cross/dist/darwin-arm64/go-frog-darwin-arm64.tar.gz" ]; then
        cp "fyne-cross/dist/darwin-arm64/go-frog-darwin-arm64.tar.gz" dist/
        # Extract the raw binary too
        cd dist && tar xzf go-frog-darwin-arm64.tar.gz 2>/dev/null; cd "$ROOT"
        build_ok "dist/go-frog-darwin-arm64 (via fyne-cross)"
        ((n_ok++))
      else
        build_ok "dist/go-frog-darwin-arm64.tar.gz"
        ((n_ok++))
      fi
    else
      build_fail "fyne-cross darwin/arm64 failed — see /tmp/gf-build.log"
      ((n_fail++))
    fi

    # macOS Intel
    echo "       → darwin/amd64 ..."
    if fyne-cross darwin -arch=amd64 -output go-frog-darwin-amd64 \
         -app-id com.gofrog -icon="" \
         -trimpath -ldflags="-s -w" . 2>/tmp/gf-build.log; then
      if [ -f "fyne-cross/dist/darwin-amd64/go-frog-darwin-amd64.tar.gz" ]; then
        cp "fyne-cross/dist/darwin-amd64/go-frog-darwin-amd64.tar.gz" dist/
        cd dist && tar xzf go-frog-darwin-amd64.tar.gz 2>/dev/null; cd "$ROOT"
        build_ok "dist/go-frog-darwin-amd64 (via fyne-cross)"
        ((n_ok++))
      else
        build_ok "dist/go-frog-darwin-amd64.tar.gz"
        ((n_ok++))
      fi
    else
      build_fail "fyne-cross darwin/amd64 failed — see /tmp/gf-build.log"
      ((n_fail++))
    fi
  else
    build_skip "install fyne-cross (go install github.com/fyne-io/fyne-cross@latest)"
    ((n_skip++))
    echo ""
    echo "       macOS builds require Apple frameworks (Cocoa, GLKit)."
    echo "       fyne-cross uses Docker + an embedded macOS SDK — the"
    echo "       official way to cross-compile Fyne apps from Linux."
    ((n_skip++))
  fi
else
  build_skip "Docker is required for macOS targets via fyne-cross"
  ((n_skip++))
  echo ""
  echo "       Alternative — build natively on a Mac:"
  echo "       (no Docker needed, just Xcode Command Line Tools)"
  echo ""
  echo "       # On a Mac with Xcode Command Line Tools:"
  echo "       go build -trimpath -ldflags='-s -w' -o dist/go-frog-darwin-arm64 ."
  echo "       GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/go-frog-darwin-amd64 ."
  ((n_skip++))
fi

# ── summary ──────────────────────────────────────────────────────────────────
echo ""
echo "──────────────────────────────────────────────────"
echo "  Results:  ${n_ok} built  |  ${n_skip} skipped  |  ${n_fail} failed"
echo "──────────────────────────────────────────────────"
echo ""
ls -lh dist/ 2>/dev/null || echo "(empty)"
echo ""

# Exit with failure if any builds failed
[ "$n_fail" -eq 0 ]
