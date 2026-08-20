#!/usr/bin/env bash
set -euo pipefail

readonly directory="internal/adapters/cgv/third_party/playwright_go_stealth"
readonly upstream_sha="375dd3a300f31a6e95a429e16ba1920dc2b7645a454662e851e74ab1f157a557"
readonly actual_sha="$(shasum -a 256 "$directory/stealth.min.js" | awk '{print $1}')"

if [[ "$actual_sha" != "$upstream_sha" ]]; then
  echo "stealth.min.js no longer matches the reviewed upstream checksum" >&2
  exit 1
fi

if rg --line-number \
  'challenges\.cloudflare\.com|isTrusted\s*=\s*true|console\.(log|debug)\s*=|\.parentElement\.click\(' \
  "$directory/chrome_stealth.js"; then
  echo "chrome_stealth.js contains forbidden active challenge or event manipulation" >&2
  exit 1
fi
