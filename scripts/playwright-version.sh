#!/usr/bin/env bash
set -euo pipefail

readonly module='github.com/mxschmitt/playwright-go'

case "${1:-}" in
  go)
    version="$(awk -v module="$module" '$1 == module { print $2; exit }' go.mod)"
    ;;
  driver)
    version="$(sed -n 's/^[[:space:]]*playwrightCliVersion = "\([^"]*\)"/\1/p' \
      vendor/github.com/mxschmitt/playwright-go/run.go)"
    ;;
  *)
    echo 'usage: playwright-version.sh go|driver' >&2
    exit 2
    ;;
esac

if [[ -z "$version" ]]; then
  echo "could not resolve Playwright ${1} version" >&2
  exit 1
fi

printf '%s\n' "$version"
