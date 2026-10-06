#!/usr/bin/env bash
# Install explicit tool versions without editing driver/go.mod or go.sum.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
lock="$root/scripts/dev-tools.lock"
bin="$root/.omc/tools/bin"
export GOTOOLCHAIN
GOTOOLCHAIN="$(awk '$1 == "toolchain" { print $2 }' "$root/driver/go.mod")"
[[ -n "$GOTOOLCHAIN" ]] || { echo 'driver/go.mod needs an explicit toolchain' >&2; exit 1; }
mkdir -p "$bin"
if [[ $# -eq 0 ]]; then
  mapfile -t requested < <(awk 'NF && $1 !~ /^#/ { print $1 }' "$lock")
else
  requested=("$@")
fi
for tool in "${requested[@]}"; do
  entry="$(awk -v tool="$tool" '$1 == tool { print $2, $3 }' "$lock")"
  [[ -n "$entry" ]] || { echo "unknown development tool: $tool" >&2; exit 2; }
  read -r package version <<< "$entry"
  # Verify the actual binary, not a version stamp left by a different compiler.
  if [[ -x "$bin/$tool" ]] && go version -m "$bin/$tool" | awk \
    -v package="$package" -v version="$version" -v compiler="$GOTOOLCHAIN" '
      NR == 1 { built = ($NF == compiler) }
      $1 == "path" { path = ($2 == package) }
      $1 == "mod" { pinned = ($3 == version) }
      END { exit !(built && path && pinned) }
    '; then
    continue
  fi
  printf 'Installing %s@%s with %s\n' "$package" "$version" "$GOTOOLCHAIN" >&2
  GOBIN="$bin" go install "$package@$version"
done
