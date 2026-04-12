#!/usr/bin/env bash
set -euo pipefail

# Proxy URL is not stored in this repo (public). Your admin gives you the URL.
# Option A: export PROXY_URL or HTTPS_PROXY, then run this script.
# Option B: run this script; it will prompt if neither is set.

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "This script is for macOS."
  exit 1
fi

if [[ -z "${PROXY_URL:-}" && -n "${HTTPS_PROXY:-}" ]]; then
  PROXY_URL="$HTTPS_PROXY"
fi

if [[ -z "${PROXY_URL// }" ]]; then
  echo "Your admin should give you the proxy URL (e.g. http://host:8888 or http://user:pass@host:8888)."
  echo "Special characters in the password must be URL-encoded in the URL."
  read -r -p "Proxy URL: " PROXY_URL
fi

if [[ -z "${PROXY_URL// }" ]]; then
  echo "No proxy URL. Set PROXY_URL or HTTPS_PROXY, or enter one when prompted."
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
