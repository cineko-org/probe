#!/usr/bin/env bash
set -euo pipefail

readonly targets=(
  windows/amd64
  windows/arm64
  darwin/amd64
  darwin/arm64
  linux/amd64
  linux/arm64
)
readonly version="${CINEKO_VERSION:-$(cat VERSION)}"
readonly browser_revision="${CINEKO_BROWSER_REVISION:-1228}"
output_dir="$(mktemp -d "${TMPDIR:-/tmp}/cineko-probe-platforms.XXXXXX")"
trap 'rm -rf -- "$output_dir"' EXIT

for target in "${targets[@]}"; do
  target_os="${target%/*}"
  target_arch="${target#*/}"
  output="$output_dir/cineko-probe-$target_os-$target_arch"
  if [[ "$target_os" == "windows" ]]; then
    output="${output}.exe"
  fi
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
	GOWORK=off go build -mod=vendor -trimpath \
      -ldflags "-s -w -X main.version=$version -X main.browserRevision=$browser_revision" \
      -o "$output" ./cmd/cineko-probe
  test -s "$output"
  printf 'probe platform build: %s\n' "$target"
done
