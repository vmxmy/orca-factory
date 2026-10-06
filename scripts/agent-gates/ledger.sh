#!/usr/bin/env bash
# Linear run ledger: one JSON object per coordinator run, stored as a comment
# on the dedicated [ledger] agent-graph issue in AGENT_LINEAR_PROJECT.
# Row fields: ticket, lane, tier(fast|default|strong; the tier resolved at claim time), outcome(merged|escalated|no_ticket),
# first_gate(green|red), corrections, ci_reds, pr, minutes, started_at, finished_at. Rows without tier (older runs)
# are reported as tier=unknown. Run via agent-graph.sh, which exports AGENT_LINEAR_TEAM / AGENT_LINEAR_PROJECT.
# Usage: ledger.sh append '<json>' | ledger.sh stats (Linear comment storage)
set -Eeuo pipefail
case "${AGENT_ISSUE_BACKEND:-linear}" in
  github) exec orca-factory-driver ledger "$@" ;;
  linear) ;;
  *) echo 'ledger: unsupported AGENT_ISSUE_BACKEND' >&2; exit 2 ;;
esac

: "${AGENT_LINEAR_TEAM:?run via agent-graph.sh: AGENT_LINEAR_TEAM required}"
: "${AGENT_LINEAR_PROJECT:?run via agent-graph.sh: AGENT_LINEAR_PROJECT required}"

linear() {
  local attempt response status reason
  for attempt in 1 2 3; do
    status=0
    # Lossy, out-of-band tally shared with the swarm board (writes ignored on failure).
    { printf '%s ledger\n' "$(date +%s)" \
      >> "${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}"; } 2>/dev/null || true
    response="$(orca linear "$@")" || status=$?
    if [[ "$status" == 0 ]] && jq -e '.ok == true' <<<"$response" >/dev/null 2>&1; then
      printf '%s\n' "$response"
      return 0
    fi
    reason="$(jq -c '.error // .message // .' <<<"$response" 2>/dev/null)" || reason="$response"
    printf 'ledger: orca linear %s failed (attempt %s/3, exit %s): %s\n' \
      "$1" "$attempt" "$status" "${reason:-empty response}" >&2
    [[ "$attempt" == 3 ]] || sleep 2
  done
  echo "ledger: orca linear $1 failed after 3 attempts; ledger operation aborted" >&2
  return 1
}

find_issue() {
  local response
  response="$(linear list-issues --team "$AGENT_LINEAR_TEAM" --project "$AGENT_LINEAR_PROJECT" \
    --query "[ledger] agent-graph" --json)" || return 1
  # --query is fuzzy; only the exact title denotes the ledger issue.
  jq -r '[.result.issues[] | select(.title == "[ledger] agent-graph") | (.identifier // .id)][0] // empty' <<<"$response"
}

case "${1:-}" in
  append)
    row="$(jq -c . <<<"${2:-}")" || { echo "ledger: invalid json row" >&2; exit 2; }
    [[ -n "$row" ]] || { echo "ledger: invalid json row" >&2; exit 2; }
    issue="$(find_issue)"
    if [[ -z "$issue" ]]; then
      response="$(linear save-issue --team "$AGENT_LINEAR_TEAM" --project "$AGENT_LINEAR_PROJECT" \
        --title "[ledger] agent-graph" --state Backlog --json)"
      issue="$(jq -er '.result.issue | .identifier // .id // empty' <<<"$response")" || {
        echo "ledger: save-issue returned no ledger issue identifier" >&2; exit 1;
      }
    fi
    linear comment add "$issue" --body "ledger $row" --json >/dev/null
    ;;
  stats)
    issue="$(find_issue)"
    [[ -n "$issue" ]] || { echo "runs=0"; exit 0; }
    response="$(linear issue "$issue" --comments --json)"
    # Orca currently caps issue comments at 500 (.result.meta.sections.comments.cap).
    # There is no comment cursor here; warn that totals may be incomplete.
    if jq -e '.result.meta.sections.comments | .hasMore == true or .capReached == true' <<<"$response" >/dev/null; then
      echo "ledger: warning: comments reached the 500-comment read cap or have more pages; stats may be incomplete" >&2
    fi
    rows="$(jq -c '
      [.result.comments[].body | select(type == "string") | select(startswith("ledger "))
        | ltrimstr("ledger ")
        | try (fromjson | if type == "object" then . else error("row is not an object") end)
          catch ("ledger: warning: invalid ledger comment skipped" | . + "\n" | stderr | empty)]
      | unique_by([.ticket, .started_at])
    ' <<<"$response")"
    [[ "$rows" != '[]' ]] || { echo "runs=0"; exit 0; }
    jq -r '
      def avg: if length == 0 then 0 else (add / length * 100 | round) / 100 end;
      def pct: if length == 0 then 0 else (map(select(. == "green")) | length) * 100 / length | round end;
      "runs=\(length) merged=\(map(select(.outcome == "merged")) | length) escalated=\(map(select(.outcome == "escalated")) | length) no_ticket=\(map(select(.outcome == "no_ticket")) | length)",
      "avg_corrections=\(map(.corrections // empty) | avg) ci_reds=\(map(.ci_reds // 0) | add // 0) avg_minutes=\(map(.minutes // empty) | avg)",
      (map(select(.lane != null)) | group_by(.lane)[]
        | "lane=\(.[0].lane) runs=\(length) first_gate_green_pct=\(map(.first_gate // empty) | pct)"),
      (map(select(.outcome != "no_ticket")) | group_by(.tier // "unknown")[]
        | "tier=\(.[0].tier // "unknown") runs=\(length) first_gate_green_pct=\(map(.first_gate // empty) | pct)")
    ' <<<"$rows"
    ;;
  *)
    echo "usage: ledger.sh append '<json>' | ledger.sh stats (Linear comment storage)" >&2
    exit 2
    ;;
esac
