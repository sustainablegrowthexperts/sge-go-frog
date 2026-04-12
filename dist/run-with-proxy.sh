#!/usr/bin/env bash
# Run go-frog on macOS with HTTP(S) traffic sent through a proxy (e.g. allowlisted droplet).
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "This script is for macOS. On Linux, set HTTPS_PROXY and run the appropriate binary yourself."
  exit 1
fi

if [[ $# -ge 1 ]]; then
  PROXY_URL="$1"
else
  read -r -p "Proxy URL (e.g. http://203.0.113.10:3128): " PROXY_URL
fi

if [[ -z "${PROXY_URL// }" ]]; then
  echo "Usage: $0 http://HOST:PORT"
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
