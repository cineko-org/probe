#!/usr/bin/env bash
set -euo pipefail

readonly image="${1:?usage: check-container-runtime.sh IMAGE}"

docker run --rm --entrypoint /usr/local/bin/cineko-probe "$image" browser-preflight
printf 'Probe container browser preflight: %s\n' "$image"
