#!/usr/bin/env bash
set -euo pipefail

readonly packages=(
  ./internal/egress
  ./internal/provider/cgv
  ./probe
)
readonly cover_packages=./internal/egress,./internal/provider/cgv,./probe
profile="$(mktemp "${TMPDIR:-/tmp}/cineko-unit-coverage.XXXXXX")"
trap 'rm -f "$profile"' EXIT

GOWORK=off go test -race \
  -covermode=atomic \
  -coverpkg="$cover_packages" \
  -coverprofile="$profile" \
  "${packages[@]}"

coverage="$(GOWORK=off go tool cover -func="$profile" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
printf 'Embedded scanner unit coverage: %s%%\n' "$coverage"
