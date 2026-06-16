# Build go-frog for the current platform (GUI requires CGo for Fyne).
#
# Cross-compiled dist/ binaries are not produced here because Fyne uses CGo,
# which requires a platform-specific C toolchain. To distribute for multiple
# platforms, build natively on each target, or use a CGo cross-compiler
# solution (e.g. zig cc via CC=zig cc).
#
# Run from repo root:  powershell -ExecutionPolicy Bypass -File scripts/build-all.ps1
# Or:                 .\scripts\build-all.ps1

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

New-Item -ItemType Directory -Force -Path "dist" | Out-Null

Write-Host "Building go-frog (native) ..."
go build -trimpath -ldflags="-s -w" -o (Join-Path "dist" "go-frog.exe") .

Write-Host "Done. Binary at dist/go-frog.exe"
