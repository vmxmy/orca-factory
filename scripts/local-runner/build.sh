#!/bin/bash
set -euo pipefail
source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
output=${1:?usage: build.sh IMAGE_ID_FILE}
mkdir -p -- "$(dirname -- "$output")"
/usr/bin/docker build --progress plain --iidfile "$output.next" "$source_dir"
mv -- "$output.next" "$output"
