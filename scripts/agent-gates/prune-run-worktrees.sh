#!/usr/bin/env bash
set -Eeuo pipefail

driver="${AGENT_DRIVER_BIN:-orca-factory-driver}"
contract="$("$driver" prune-worktrees --contract-version)" || {
  echo "prune: driver does not support prune-worktrees-v1: $driver" >&2
  exit 1
}
[[ "$contract" == prune-worktrees-v1 ]] || {
  echo "prune: incompatible prune-worktrees contract: $contract" >&2
  exit 1
}
exec "$driver" prune-worktrees "$@"
