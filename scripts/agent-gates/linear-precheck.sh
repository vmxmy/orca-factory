#!/usr/bin/env bash
# Orca automation precheck: exit 0 only when this repo's Linear project has a claimable ticket
# and fewer than MAX_IN_FLIGHT tickets are in flight. Linear projects map 1:1 to repos, so the
# project id is the repo fence. A ticket is claimable when every issue it is "blocked by" is
# completed or canceled. The coordinator claims the first id printed on `claimable=` (oldest first).
# Within one label/state scan the ids are ordered by Linear priority ascending (1=urgent, 2=high,
# 3=medium, 4=low; 0=none sorts last as 9), then createdAt ascending, so an urgent ticket created
# today is claimed before a no-priority ticket from last week.
# Existing scan calls must succeed: the CLI talks to the local Orca app and drops connections under
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
  # priority: 1 urgent … 4 low; 0/null = none -> 9 so it sorts after every explicit priority.
  jq -r --arg type "$2" '
    .result.issues
    | sort_by(((.priority // 0) | if . == 0 then 9 else . end), .createdAt)[]
    | select(.state.type == $type) | .identifier' <<<"$out"
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
  ocall linear comment add "$1" --body "转人: 在途超过 ${STALE_HOURS} 小时无更新，疑似 coordinator 中断（worktree/PR 若存在均保留）
证据: ${BASH_SOURCE[0]}:61 的 stale 判定为 true，票据 $1 已至少 ${STALE_HOURS} 小时无更新
需要你: 检查票据 $1 对应的 coordinator 运行状态" >/dev/null || exit 3
}

# Recovery reads are bounded and fail closed without aborting the existing scan.
claim_read() {
  local out
  if out="$(perl -e 'alarm 20; exec @ARGV' -- orca "$@" --json 2>/dev/null)" &&
    jq -e '.ok == true' <<<"$out" >/dev/null 2>&1; then
    printf '%s' "$out"
  else
    echo "precheck: $claim_id 认领判定不确定：orca $* failed ($(jq -r '.error.code // "command error/timeout/invalid response"' <<<"$out" 2>/dev/null))" >&2
    return 1
  fi
}

claim_uncertain() {
  echo "precheck: $claim_id 认领判定不确定：$*" >&2
  return 1
}

# Return 0 for expired, 1 for live, 2 for uncertain (do not attempt further recovery).
expired_claim() {
  local claim_id="$1" out age runs run tasks cards cursor="" matches=""
  claim_reason=""
  claim_failures=0
  if ! out="$(claim_read linear issue "$claim_id" --comments)"; then return 2; fi
  if jq -e '.result.meta.sections.comments | .capReached == true or .hasMore == true' <<<"$out" >/dev/null; then
    claim_uncertain 'comments truncated'; return 2
  fi
  if ! age="$(jq -r '
    .result.comments | if type != "array" then error("comments missing") else . end
    | map(select((.body // "") | startswith("已领取"))
      | .createdAt | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601)
    | if length == 0 then false else (now - max) > 1200 end' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse claim comments'; return 2
  fi
  [[ "$age" == true ]] || return 1
  claim_failures="$(jq '[.result.comments[] | select((.body // "") | contains("认领失效"))] | length' <<<"$out")" || {
    claim_uncertain 'cannot count claim failures'; return 2;
  }
  # Follow pagination so a run on a later page is never treated as absent.
  while :; do
    if [[ -n "$cursor" ]]; then
      if ! out="$(claim_read orchestration run-list --cursor "$cursor")"; then return 2; fi
    else
      if ! out="$(claim_read orchestration run-list)"; then return 2; fi
    fi
    if ! runs="$(jq -r --arg id "$claim_id" '
      .result.runs | if type != "array" then error("runs missing") else . end
      | .[] | select(.objective | startswith($id)) | .id
      | if type != "string" or length == 0 then error("run id missing") else . end' <<<"$out" 2>/dev/null)"; then
      claim_uncertain 'cannot parse runs'; return 2
    fi
    matches="${matches}${matches:+ }$runs"
    cursor="$(jq -r '.result.nextCursor // empty' <<<"$out")"
    [[ -n "$cursor" ]] || break
  done
  if [[ -z "$matches" ]]; then
    claim_reason='没有匹配的编排运行'
    return 0
  fi
  for run in $matches; do
    if ! out="$(claim_read orchestration task-list --run "$run")"; then return 2; fi
    if ! tasks="$(jq -r '.result.tasks | if type == "array" then length else error("tasks missing") end' <<<"$out" 2>/dev/null)"; then
      claim_uncertain 'cannot parse tasks'; return 2
    fi
    [[ "$tasks" == 0 ]] || return 1
  done
  if ! out="$(claim_read worktree ps)"; then return 2; fi
  if jq -e '.result.hostScope.omittedHostIds | type == "array" and length > 0' <<<"$out" >/dev/null; then
    claim_uncertain 'worktree hosts omitted'; return 2
  fi
  if ! cards="$(jq -r --arg id "$claim_id" '
    if .result.truncated == true then error("worktrees truncated") else .result.worktrees end
    | if type != "array" then error("worktrees missing") else . end
    | map(select(.linkedLinearIssue == $id)) | length' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse complete worktree list'; return 2
  fi
  [[ "$cards" == 0 ]] || return 1
  claim_reason='编排运行无任务且没有关联卡片'
}

# Read the whole snapshot before deciding: partial/invalid evidence must never mutate a ticket.
stuck_worker() {
  local claim_id="$1" out worktrees
  worker_evidence=""
  if ! out="$(claim_read worktree ps)"; then return 1; fi
  if ! worktrees="$(jq -ce --arg id "$claim_id" '
    .result
    | if .truncated == true or ((.hostScope.omittedHostIds // []) | length > 0)
      then error("incomplete worktree list") else .worktrees end
    | if type != "array" then error("worktrees missing") else . end
    | map(select(.linkedLinearIssue == $id) | .worktreeId
      | if type != "string" or length == 0 then error("worktree id missing") else . end)
    ' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse complete worker worktree list'; return 1
  fi
  [[ "$worktrees" != '[]' ]] || return 1
  if ! out="$(claim_read terminal list)"; then return 1; fi
  if ! worker_evidence="$(jq -r --argjson worktrees "$worktrees" \
    --argjson minutes "${AGENT_WORKER_IDLE_MINUTES:-15}" '
    .result
    | if .truncated == true or ((.hostScope.omittedHostIds // []) | length > 0)
      then error("incomplete terminal list") else . end
    | if (.terminals | type) != "array" then error("terminals missing") else . end
    | if .totalCount != null and .totalCount != (.terminals | length)
      then error("terminal count mismatch") else .terminals end
    | map(
        if (.worktreeId | type) != "string" then error("terminal worktree id missing") else . end
        | select(.worktreeId as $id | $worktrees | index($id))
        | if (.lastOutputAt | type) != "number" or (.preview | type) != "string"
          then error("terminal output missing") else . end
        | . + {idle: ((now - (.lastOutputAt / 1000)) / 60)})
    | map(select(.idle > $minutes and (.preview | test("Sign in|ask timeout|worker_done|failed|登录"))))
    | if length == 0 then empty else .[0]
      | "空闲 \(.idle | floor) 分钟，preview 末尾: \(.preview[-300:] | gsub("[\\r\\n]"; " "))" end
    ' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse complete terminal list/worker output'; return 1
  fi
  [[ -n "$worker_evidence" ]]
}

mark_stuck_worker() {
  ocall linear label remove "$1" --label "$LABEL_CLAIMED" >/dev/null || exit 3
  ocall linear label add "$1" --label "$LABEL_FAILED" >/dev/null || exit 3
  ocall linear comment add "$1" --body "转人: 工人长时间无输出且命中卡点特征（worktree/PR 若存在均保留）
证据: 票据 $1 的终端${worker_evidence}
需要你: 检查票据 $1 对应工人的登录或运行状态" >/dev/null || exit 3
}

release_claim() {
  ocall linear label remove "$1" --label "$LABEL_CLAIMED" >/dev/null || exit 3
  if (( claim_failures >= 2 )); then
    ocall linear label add "$1" --label "$LABEL_FAILED" >/dev/null || exit 3
    ocall linear comment add "$1" --body "转人: 已多次认领失效（worktree/PR 若存在均保留）
证据: orca linear issue $1 --comments 的评论中已有 ${claim_failures} 次认领失效；${claim_reason}
需要你: 排查票据 $1 的认领失效原因" >/dev/null || exit 3
    stale_ids+=("$1")
  else
    ocall linear status set "$1" --to Todo >/dev/null || exit 3
    ocall linear comment add "$1" --body "precheck: 认领失效（${claim_reason}），已释放，下一轮重新领取" >/dev/null || exit 3
  fi
}

in_flight_ids="$(ids "$LABEL_CLAIMED" started)"
ready_ids="$(ids "$LABEL_READY" unstarted)"
live=()
stale_ids=()
for id in $in_flight_ids; do
  if stale "$id"; then
    mark_stale "$id"; stale_ids+=("$id")
  elif expired_claim "$id"; then
    release_claim "$id"
  else
    claim_status=$?
    if [[ "$claim_status" == 1 ]] && stuck_worker "$id"; then
      mark_stuck_worker "$id"; stale_ids+=("$id")
    else
      live+=("$id")
    fi
  fi
done
in_flight=${#live[@]}
claimable=()
for id in $ready_ids; do
  if unblocked "$id"; then claimable+=("$id"); fi
done
echo "claimable=$(IFS=,; echo "${claimable[*]-}") in_flight=$in_flight max=$MAX_IN_FLIGHT"
echo "stale=$(IFS=,; echo "${stale_ids[*]-}")"
(( ${#claimable[@]} > 0 && in_flight < MAX_IN_FLIGHT ))
