#!/bin/bash
set -euo pipefail
source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root=$(git -C "$source_dir" rev-parse --show-toplevel)
output=${1:?usage: build.sh IMAGE_ID_FILE}
go_version=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2 }' "$root/driver/go.mod")
archive="go$go_version.linux-amd64.tar.gz"
# The checksum comes from go.dev; the bytes come from the mirror the runner network can reach.
go_sha256=$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' \
  | jq -er --arg file "$archive" 'first(.[].files[] | select(.filename == $file) | .sha256)')
context=$(mktemp -d)
trap 'rm -rf -- "$context"' EXIT
cp -- "$source_dir/Dockerfile" "$source_dir/entrypoint.sh" "$context/"
mkdir -p -- "$context/prewarm/driver" "$context/prewarm/scripts"
cp -- "$root/driver/go.mod" "$root/driver/go.sum" "$context/prewarm/driver/"
cp -- "$root/scripts/dev-tools.lock" "$root/scripts/install-dev-tools.sh" "$context/prewarm/scripts/"
mkdir -p -- "$(dirname -- "$output")"
/usr/bin/docker build --progress plain --build-arg GO_VERSION="$go_version" --build-arg GO_SHA256="$go_sha256" \
  --iidfile "$output.next" "$context"
mv -- "$output.next" "$output"
