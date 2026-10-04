#!/usr/bin/env bash
# Local run ledger: one JSON object per coordinator run, appended to
# $AGENT_REPO_PATH/.omc/agent-graph/runs.jsonl (.omc is gitignored; this is machine-local data).
# Row fields: ticket, lane, tier(fast|default|strong; the tier resolved at claim time), outcome(merged|escalated|no_ticket),
# first_gate(green|red), corrections, ci_reds, pr, minutes, started_at, finished_at. Rows without tier (older runs)
# are reported as tier=unknown. Run via agent-graph.sh, which exports AGENT_REPO_PATH.
# Usage: ledger.sh append '<json>' | ledger.sh stats
set -Eeuo pipefail

LEDGER="${AGENT_REPO_PATH:?run via agent-graph.sh}/.omc/agent-graph/runs.jsonl"

case "${1:-}" in
  append)
    row="$(jq -c . <<<"${2:?json row}")" || { echo "ledger: invalid json row" >&2; exit 2; }
    mkdir -p "$(dirname "$LEDGER")"
    printf '%s\n' "$row" >>"$LEDGER"
    ;;
  stats)
    [[ -s $LEDGER ]] || { echo "runs=0"; exit 0; }
    jq -rs '
      def avg: if length == 0 then 0 else (add / length * 100 | round) / 100 end;
      def pct: if length == 0 then 0 else (map(select(. == "green")) | length) * 100 / length | round end;
      "runs=\(length) merged=\(map(select(.outcome == "merged")) | length) escalated=\(map(select(.outcome == "escalated")) | length) no_ticket=\(map(select(.outcome == "no_ticket")) | length)",
      "avg_corrections=\(map(.corrections // empty) | avg) ci_reds=\(map(.ci_reds // 0) | add // 0) avg_minutes=\(map(.minutes // empty) | avg)",
      (map(select(.lane != null)) | group_by(.lane)[]
        | "lane=\(.[0].lane) runs=\(length) first_gate_green_pct=\(map(.first_gate // empty) | pct)"),
      (map(select(.outcome != "no_ticket")) | group_by(.tier // "unknown")[]
        | "tier=\(.[0].tier // "unknown") runs=\(length) first_gate_green_pct=\(map(.first_gate // empty) | pct)")
    ' "$LEDGER"
    ;;
  *)
    echo "usage: ledger.sh append '<json>' | ledger.sh stats" >&2
    exit 2
    ;;
esac
