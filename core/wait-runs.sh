#!/usr/bin/env bash
# Wait for worker_done / escalation / question on any of several orchestration runs.
# A coordinator terminal is bound to one Run at a time, so each Run is bound with `run-use` before
# it is checked. `check --json` prints keepalive objects before the result, so the whole output is
# parsed and the last non-keepalive object is used.
# Usage: wait-runs.sh <units-file> <state-dir> [budget-seconds]
#   units-file: one line per unit: "<ticket> <run-id> [anything]"
#   state-dir:  <state-dir>/<ticket>/settled marks a finished ticket (skipped);
#               the raw result of a run with messages is written to <state-dir>/<ticket>/msg.json
# Output: for every run with messages, "### <ticket> <run> delivery=<id>" and one compact JSON line per message.
# Exit: 0 at least one run has messages (ack them with `check --ack <delivery>`), 2 budget used, 64 bad usage.
set -u
units="${1:-}"; state="${2:-}"; budget="${3:-540}"
{ [[ -f "$units" ]] && [[ -d "$state" ]]; } || { echo "usage: wait-runs.sh <units-file> <state-dir> [budget-seconds]" >&2; exit 64; }
wait_ms="${AGENT_WAIT_PER_RUN_MS:-5000}"
end=$((SECONDS + budget))
while :; do
  found=0
  while read -r ticket run _; do
    [[ -n "$ticket" && -n "$run" ]] || continue
    [[ -f "$state/$ticket/settled" ]] && continue
    if ! orca orchestration run-use --id "$run" --json >/dev/null 2>&1; then
      echo "### $ticket $run run-use failed" >&2; continue
    fi
    raw="$(orca orchestration check --run "$run" --types worker_done,escalation,question --wait --timeout-ms "$wait_ms" --json 2>/dev/null)" || true
    out="$(jq -s 'map(select(._keepalive | not)) | last' <<<"$raw" 2>/dev/null)" || out=""
    count="$(jq -r '.result.count // "none"' <<<"$out" 2>/dev/null)" || count=none
    if [[ "$count" == none ]]; then echo "### $ticket $run unreadable result" >&2; continue; fi
    if [[ "$count" != 0 ]]; then
      mkdir -p "$state/$ticket"; printf '%s\n' "$out" > "$state/$ticket/msg.json"
      echo "### $ticket $run delivery=$(jq -r '.result.deliveryId // ""' <<<"$out")"
      jq -c '.result.messages[]? | {id, type, subject, taskId, dispatchId, outcome}' <<<"$out"
      found=1
    fi
  done < "$units"
  (( found )) && exit 0
  (( SECONDS < end )) || break
  sleep 2
done
echo TIMEOUT
exit 2
