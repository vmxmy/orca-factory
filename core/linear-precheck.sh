#!/usr/bin/env bash
# Orca automation precheck: exit 0 only when this repo's Linear project has a claimable ticket
# and fewer than MAX_IN_FLIGHT tickets are in flight. Linear projects map 1:1 to repos, so the
# project id is the repo fence. A ticket is claimable when every issue it is "blocked by" is
# completed or canceled. The coordinator claims the first id printed on `claimable=` (oldest first).
# Every orca call must succeed: the CLI talks to the local Orca app and drops connections under
# concurrent load (runtime_unavailable), so a failed read aborts the scan instead of reading as "none".
# Stale in-flight tickets (claimed + started, no Linear update for AGENT_STALE_HOURS) are a crashed
# coordinator: they are flipped to auto-failed, commented, and dropped from the in-flight count so they
# cannot hold a concurrency slot forever. Printed on the `stale=` line.
# ponytail: max in-flight is a flat count, not file-overlap aware; the coordinator skips overlapping tickets.
set -Eeuo pipefail

TEAM="${AGENT_LINEAR_TEAM:?set in agent-graph.env}"
PROJECT="${AGENT_LINEAR_PROJECT:?set in agent-graph.env}"
LABEL_READY="${AGENT_LABEL_READY:?set in agent-graph.env}"
LABEL_CLAIMED="${AGENT_LABEL_CLAIMED:?set in agent-graph.env}"
LABEL_FAILED="${AGENT_LABEL_FAILED:?set in agent-graph.env}"
MAX_IN_FLIGHT="${AGENT_MAX_IN_FLIGHT:?set in agent-graph.env}"
STALE_HOURS="${AGENT_STALE_HOURS:?set in agent-graph.env}"

ocall() {
  local out attempt
  for attempt in 1 2 3; do
    out="$(orca "$@" --json 2>/dev/null)" && jq -e '.ok == true' <<<"$out" >/dev/null && { printf '%s' "$out"; return 0; }
    sleep 2
  done
  echo "orca $* failed: $(jq -r '.error.code // "unknown"' <<<"$out" 2>/dev/null)" >&2
  return 1
}

ids() {
  local out
  out="$(ocall linear list-issues --team "$TEAM" --project "$PROJECT" --label "$1" --order-by createdAt)" || exit 3
  jq -r --arg type "$2" '.result.issues | sort_by(.createdAt)[] | select(.state.type == $type) | .identifier' <<<"$out"
}

unblocked() {
  local rel blocker state
  rel="$(ocall linear issue "$1" --relations)" || exit 3
  for blocker in $(jq -r '(.result.relations // [])[] | select(.relationship == "blockedBy") | .relatedIssue.identifier' <<<"$rel"); do
    state="$(ocall linear issue "$blocker")" || exit 3
    case "$(jq -r '.result.issue.state.type' <<<"$state")" in completed|canceled) ;; *) return 1 ;; esac
  done
}

# Parse Linear's ISO timestamp with jq (not `date -j -f`, which differs between macOS and Linux);
# Linear emits milliseconds, which fromdateiso8601 rejects, so strip them first.
stale() {
  local out verdict
  out="$(ocall linear issue "$1")" || exit 3
  verdict="$(jq -r --argjson h "$STALE_HOURS" \
    '.result.issue.updatedAt | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 | (now - .) >= ($h * 3600)' <<<"$out")" \
    || { echo "precheck: cannot parse updatedAt of $1" >&2; exit 3; }
  [[ $verdict == true ]]
}

mark_stale() {
  ocall linear label remove "$1" --label "$LABEL_CLAIMED" >/dev/null || exit 3
  ocall linear label add "$1" --label "$LABEL_FAILED" >/dev/null || exit 3
  ocall linear comment add "$1" --body "precheck: 在途超过 ${STALE_HOURS} 小时无更新，疑似 coordinator 中断，已转人工（worktree/PR 若存在均保留）" >/dev/null || exit 3
}

in_flight_ids="$(ids "$LABEL_CLAIMED" started)"
ready_ids="$(ids "$LABEL_READY" unstarted)"
live=()
stale_ids=()
for id in $in_flight_ids; do
  if stale "$id"; then mark_stale "$id"; stale_ids+=("$id"); else live+=("$id"); fi
done
in_flight=${#live[@]}
claimable=()
for id in $ready_ids; do
  if unblocked "$id"; then claimable+=("$id"); fi
done
echo "claimable=$(IFS=,; echo "${claimable[*]-}") in_flight=$in_flight max=$MAX_IN_FLIGHT"
echo "stale=$(IFS=,; echo "${stale_ids[*]-}")"
(( ${#claimable[@]} > 0 && in_flight < MAX_IN_FLIGHT ))
