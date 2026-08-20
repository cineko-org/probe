#!/usr/bin/env bash
set -euo pipefail

readonly actual="$(bash scripts/render-probe-release.sh \
  2.2.0 \
  1228 \
  registry.example/cineko/probe \
  sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  2026-08-19T10:00:00Z)"

jq --exit-status \
  --argjson actual "$actual" \
  --slurpfile expected testdata/wire/probe-release-set.json \
  '$actual == $expected[0]' \
  >/dev/null

readonly digest=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
readonly invalid_images=(
  registry.example/cineko/probe:sha-0123456789ab
  registry.example/cineko/probe@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  https://registry.example/cineko/probe
  probe
)
for image in "${invalid_images[@]}"; do
  if bash scripts/render-probe-release.sh \
    2.2.0 1228 "$image" "$digest" 2026-08-19T10:00:00Z \
    >/dev/null 2>&1; then
    printf 'invalid Probe repository was accepted: %s\n' "$image" >&2
    exit 1
  fi
done

if bash scripts/render-probe-release.sh \
  2.2.0 1228 registry.example/cineko/probe sha256:not-a-digest 2026-08-19T10:00:00Z \
  >/dev/null 2>&1; then
  echo "invalid Probe digest was accepted" >&2
  exit 1
fi

for version in 2.2 v2.2.0 02.2.0 2.2.0-01; do
  if bash scripts/render-probe-release.sh \
    "$version" 1228 registry.example/cineko/probe "$digest" 2026-08-19T10:00:00Z \
    >/dev/null 2>&1; then
    printf 'invalid Probe version was accepted: %s\n' "$version" >&2
    exit 1
  fi
done

for browser_revision in -1 1228.0 current; do
  if bash scripts/render-probe-release.sh \
    2.2.0 "$browser_revision" registry.example/cineko/probe "$digest" 2026-08-19T10:00:00Z \
    >/dev/null 2>&1; then
    printf 'invalid browser revision was accepted: %s\n' "$browser_revision" >&2
    exit 1
  fi
done

for published_at in 2026-08-19 2026-13-19T10:00:00Z not-a-time; do
  if bash scripts/render-probe-release.sh \
    2.2.0 1228 registry.example/cineko/probe "$digest" "$published_at" \
    >/dev/null 2>&1; then
    printf 'invalid publication time was accepted: %s\n' "$published_at" >&2
    exit 1
  fi
done
