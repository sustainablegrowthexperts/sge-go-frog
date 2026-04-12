#!/usr/bin/env bash
set -euo pipefail

# This repo is public — no proxy URL is committed here.
# Paste the URL your admin gave you between the quotes (once), save, then run this file.
# Example shapes: http://proxy.example.com:8888   or   http://USER:PASS@host:8888
# Password special chars: use a URL-encoded password, or use single quotes around the whole URL.
PROXY_URL=""

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "This script is for macOS."
  exit 1
fi

if [[ -z "${PROXY_URL// }" ]]; then
  echo "Edit run-with-proxy.sh: set PROXY_URL near the top of this file, save, then run again."
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
