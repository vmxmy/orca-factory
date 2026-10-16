#!/usr/bin/env bash
set -Eeuo pipefail

case "${AGENT_ISSUE_BACKEND:-}" in
  github|feishu) exec "${AGENT_DRIVER_BIN:-orca-factory-driver}" ledger "$@" ;;
  *) echo 'ledger: AGENT_ISSUE_BACKEND must be github or feishu' >&2; exit 2 ;;
esac
