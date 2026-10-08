#!/usr/bin/env bash
set -Eeuo pipefail

case "${AGENT_ISSUE_BACKEND:-linear}" in
  github) exec "${AGENT_DRIVER_BIN:-orca-factory-driver}" ledger "$@" ;;
  linear)
    : "${AGENT_LINEAR_TEAM:?run via agent-graph.sh: AGENT_LINEAR_TEAM required}"
    : "${AGENT_LINEAR_PROJECT:?run via agent-graph.sh: AGENT_LINEAR_PROJECT required}" ;;
  *) echo 'ledger: 未知账本后端' >&2; exit 2 ;;
esac

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
