# Cross-compile go-frog for Windows (amd64) and macOS (Intel + Apple Silicon).
# Run from repo root:  powershell -ExecutionPolicy Bypass -File scripts/build-all.ps1
# Or:                 .\scripts\build-all.ps1

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

New-Item -ItemType Directory -Force -Path "dist" | Out-Null
$env:CGO_ENABLED = "0"

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Out = "go-frog-windows-amd64.exe" },
    @{ GOOS = "darwin";  GOARCH = "arm64"; Out = "go-frog-darwin-arm64" },
    @{ GOOS = "darwin";  GOARCH = "amd64"; Out = "go-frog-darwin-amd64" }
)

foreach ($t in $targets) {
    $env:GOOS = $t.GOOS
    $env:GOARCH = $t.GOARCH
    $out = Join-Path "dist" $t.Out
    Write-Host "Building $out ..."
    go build -trimpath -ldflags="-s -w" -o $out .
}

Remove-Item Env:GOOS -ErrorAction SilentlyContinue
Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
Write-Host "Done. Outputs in dist/"
