#!/usr/bin/env bash
set -euo pipefail

# Allowlisted proxy (edit this line before you commit / ship)
PROXY_URL="http://sgeadmin:g94sLhsqA5ecnfNOH6WgWde@128.199.217.199:7437"

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "This script is for macOS."
  exit 1
fi

if [[ -z "${PROXY_URL// }" ]]; then
  echo "PROXY_URL is empty in run-with-proxy.sh"
  exit 1
fi

export HTTPS_PROXY="$PROXY_URL"
export HTTP_PROXY="$PROXY_URL"

case "$(uname -m)" in
  arm64) exec "$DIR/go-frog-darwin-arm64" ;;
  x86_64) exec "$DIR/go-frog-darwin-amd64" ;;
  *)
    echo "Unsupported Mac architecture: $(uname -m)"
    exit 1
    ;;
esac
