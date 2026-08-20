#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 5 ]]; then
  echo "usage: render-probe-release.sh VERSION BROWSER_REVISION IMAGE IMAGE_DIGEST PUBLISHED_AT" >&2
  exit 2
fi

readonly version="$1"
readonly browser_revision="$2"
readonly image="$3"
readonly image_digest="$4"
readonly published_at="$5"

readonly numeric_identifier='(0|[1-9][0-9]*)'
readonly prerelease_identifier='(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)'
readonly semver_pattern="^${numeric_identifier}\\.${numeric_identifier}\\.${numeric_identifier}(-${prerelease_identifier}(\\.${prerelease_identifier})*)?(\\+[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?$"
if [[ ! "$version" =~ $semver_pattern ]]; then
  echo "VERSION must use semantic versioning without a v prefix" >&2
  exit 2
fi
if [[ ! "$browser_revision" =~ ^[0-9]+$ ]]; then
  echo "BROWSER_REVISION must be a nonnegative integer" >&2
  exit 2
fi
if ! jq --exit-status --null-input --arg publishedAt "$published_at" \
  '$publishedAt | fromdateiso8601 | type == "number"' >/dev/null; then
  echo "PUBLISHED_AT must be a valid RFC3339 UTC timestamp" >&2
  exit 2
fi

readonly image_final_segment="${image##*/}"
if [[ -z "$image" || "$image" =~ [[:space:]@] || "$image" == *://* || "$image" != */* ||
  "$image_final_segment" == *:* ]]; then
  echo "IMAGE must be an untagged, digest-free repository" >&2
  exit 2
fi
if [[ ! "$image_digest" =~ ^sha256:[[:xdigit:]]{64}$ ]]; then
  echo "IMAGE_DIGEST must be a sha256 OCI digest" >&2
  exit 2
fi

jq -cn \
  --arg version "$version" \
  --arg browserRevision "$browser_revision" \
  --arg image "$image" \
  --arg imageDigest "$image_digest" \
  --arg publishedAt "$published_at" \
  '{schemaVersion: 2, payload: {releases: [{
    channel: "stable",
    version: $version,
    protocol: 3,
    browserRevision: $browserRevision,
    image: $image,
    imageDigest: $imageDigest,
    publishedAt: $publishedAt
  }]}}'
