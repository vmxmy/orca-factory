#!/usr/bin/env bash
set -Eeuo pipefail

: "${AGENT_LINEAR_TEAM:?run via agent-graph.sh: AGENT_LINEAR_TEAM required}"
: "${AGENT_LINEAR_PROJECT:?run via agent-graph.sh: AGENT_LINEAR_PROJECT required}"

driver="${AGENT_DRIVER_BIN:-orca-factory-driver}"
contract="$("$driver" ledger-shell --contract-version)" || {
  echo "ledger: driver does not support ledger-shell-v1: $driver" >&2
  exit 1
}
[[ "$contract" == ledger-shell-v1 ]] || {
  echo "ledger: incompatible ledger-shell contract: $contract" >&2
  exit 1
}
exec "$driver" ledger-shell "$@"
