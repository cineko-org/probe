#!/usr/bin/env bash
set -euo pipefail

readonly digest=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
readonly actual="$(bash scripts/render-probe-release.sh \
  2.2.0 1228 registry.example/cineko/probe "$digest" 2026-08-19T10:00:00Z)"

jq --exit-status --argjson actual "$actual" '
  $actual.schemaVersion == 2 and
  $actual.payload.releases[0].channel == "stable" and
  $actual.payload.releases[0].version == "2.2.0" and
  $actual.payload.releases[0].protocol == 3 and
  $actual.payload.releases[0].browserRevision == "1228" and
  $actual.payload.releases[0].image == "registry.example/cineko/probe" and
  $actual.payload.releases[0].imageDigest == "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" and
  $actual.payload.releases[0].publishedAt == "2026-08-19T10:00:00Z"
' >/dev/null

for image in \
  registry.example/cineko/probe:sha-0123456789ab \
  registry.example/cineko/probe@"$digest" \
  https://registry.example/cineko/probe \
  probe; do
  if bash scripts/render-probe-release.sh 2.2.0 1228 "$image" "$digest" 2026-08-19T10:00:00Z >/dev/null 2>&1; then
    echo "invalid Probe repository was accepted: $image" >&2
    exit 1
  fi
done

if bash scripts/render-probe-release.sh 2.2.0 1228 registry.example/cineko/probe sha256:not-a-digest 2026-08-19T10:00:00Z >/dev/null 2>&1; then
  echo "invalid Probe digest was accepted" >&2
  exit 1
fi

for version in 2.2 v2.2.0 02.2.0 2.2.0-01; do
  if bash scripts/render-probe-release.sh "$version" 1228 registry.example/cineko/probe "$digest" 2026-08-19T10:00:00Z >/dev/null 2>&1; then
    echo "invalid Probe version was accepted: $version" >&2
    exit 1
  fi
done

for published_at in 2026-08-19 2026-13-19T10:00:00Z not-a-time; do
  if bash scripts/render-probe-release.sh 2.2.0 1228 registry.example/cineko/probe "$digest" "$published_at" >/dev/null 2>&1; then
    echo "invalid publication time was accepted: $published_at" >&2
    exit 1
  fi
done
