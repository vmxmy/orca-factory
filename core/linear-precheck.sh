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
GH_REPO="${AGENT_GH_REPO:?set in agent-graph.env}"
ORPHAN_MINUTES="${AGENT_ORPHAN_MINUTES:-45}"
WORKER_IDLE_MINUTES="${AGENT_WORKER_IDLE_MINUTES:-15}"

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
  escalated=$((escalated + 1))
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

# Success means positively expired; a live or uncertain claim returns false.
expired_claim() {
  local claim_id="$1" out age runs run tasks cards cursor="" matches=""
  claim_reason=""
  claim_failures=0
  if ! out="$(claim_read linear issue "$claim_id" --comments)"; then return 1; fi
  if jq -e '.result.meta.sections.comments | .capReached == true or .hasMore == true' <<<"$out" >/dev/null; then
    claim_uncertain 'comments truncated'; return 1
  fi
  if ! age="$(jq -r '
    .result.comments | if type != "array" then error("comments missing") else . end
    | map(select((.body // "") | startswith("已领取"))
      | .createdAt | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601)
    | if length == 0 then false else (now - max) > 1200 end' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse claim comments'; return 1
  fi
  [[ "$age" == true ]] || return 1
  claim_failures="$(jq '[.result.comments[] | select((.body // "") | contains("认领失效"))] | length' <<<"$out")" || {
    claim_uncertain 'cannot count claim failures'; return 1;
  }
  # Follow pagination so a run on a later page is never treated as absent.
  while :; do
    if [[ -n "$cursor" ]]; then
      if ! out="$(claim_read orchestration run-list --cursor "$cursor")"; then return 1; fi
    else
      if ! out="$(claim_read orchestration run-list)"; then return 1; fi
    fi
    if ! runs="$(jq -r --arg id "$claim_id" '
      .result.runs | if type != "array" then error("runs missing") else . end
      | .[] | select(.objective | startswith($id)) | .id
      | if type != "string" or length == 0 then error("run id missing") else . end' <<<"$out" 2>/dev/null)"; then
      claim_uncertain 'cannot parse runs'; return 1
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
    if ! out="$(claim_read orchestration task-list --run "$run")"; then return 1; fi
    if ! tasks="$(jq -r '.result.tasks | if type == "array" then length else error("tasks missing") end' <<<"$out" 2>/dev/null)"; then
      claim_uncertain 'cannot parse tasks'; return 1
    fi
    [[ "$tasks" == 0 ]] || return 1
  done
  if ! out="$(claim_read worktree ps)"; then return 1; fi
  if jq -e '.result.hostScope.omittedHostIds | type == "array" and length > 0' <<<"$out" >/dev/null; then
    claim_uncertain 'worktree hosts omitted'; return 1
  fi
  if ! cards="$(jq -r --arg id "$claim_id" '
    if .result.truncated == true then error("worktrees truncated") else .result.worktrees end
    | if type != "array" then error("worktrees missing") else . end
    | map(select(.linkedLinearIssue == $id)) | length' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse complete worktree list'; return 1
  fi
  [[ "$cards" == 0 ]] || return 1
  claim_reason='编排运行无任务且没有关联卡片'
}

# Catches an in-flight ticket whose coordinator ended without PR: Linear update older than
# ORPHAN_MINUTES and no open PR with the ticket id in its title. Any read failure or unparsable
# response is uncertain and keeps the ticket live. Sets claim_reason/claim_failures for release_claim.
orphan_claim() {
  local claim_id="$1" out verdict pr_out pr_count
  claim_reason=""
  claim_failures=0
  if ! out="$(claim_read linear issue "$claim_id")"; then return 1; fi
  if ! verdict="$(jq -r --argjson m "$ORPHAN_MINUTES" \
    '.result.issue.updatedAt | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 | (now - .) >= ($m * 60)' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse updatedAt for orphan check'; return 1
  fi
  [[ "$verdict" == true ]] || return 1
  if ! pr_out="$(perl -e 'alarm 20; exec @ARGV' -- \
    gh pr list --repo "$GH_REPO" --state open --search "$claim_id in:title" --json number 2>/dev/null)"; then
    claim_uncertain "gh pr list failed for $claim_id"; return 1
  fi
  if ! pr_count="$(jq 'if type == "array" then length else error("pr list not array") end' <<<"$pr_out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse gh pr list output'; return 1
  fi
  (( pr_count == 0 )) || return 1
  if ! out="$(claim_read linear issue "$claim_id" --comments)"; then return 1; fi
  if ! claim_failures="$(jq '[.result.comments[] | select((.body // "") | contains("认领失效"))] | length' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot count claim failures for orphan check'; return 1
  fi
  claim_reason="在途超过 ${ORPHAN_MINUTES} 分钟无更新且无 PR，coordinator 疑似已结束"
}

release_claim() {
  ocall linear label remove "$1" --label "$LABEL_CLAIMED" >/dev/null || exit 3
  if (( claim_failures >= 2 )); then
    ocall linear label add "$1" --label "$LABEL_FAILED" >/dev/null || exit 3
    ocall linear comment add "$1" --body "转人: 已多次认领失效（worktree/PR 若存在均保留）
证据: orca linear issue $1 --comments 的评论中已有 ${claim_failures} 次认领失效；${claim_reason}
需要你: 排查票据 $1 的认领失效原因" >/dev/null || exit 3
    stale_ids+=("$1")
    escalated=$((escalated + 1))
  else
    ocall linear status set "$1" --to Todo >/dev/null || exit 3
    ocall linear comment add "$1" --body "precheck: 认领失效（${claim_reason}），已释放，下一轮重新领取" >/dev/null || exit 3
    released=$((released + 1))
  fi
}

# Worker health check: judge the whole worktree, not a single terminal. Idle minutes come from the
# most recent agent terminal in the worktree, and the stuck pattern is matched only against that
# newest terminal's preview, so a live worker alongside an idle old terminal is never escalated.
# Any read failure, truncation, parsing error, or partial snapshot keeps the ticket live. On a
# positive match, populates claim_reason/claim_failures so release_claim decides between retry and
# escalation on the shared 认领失效 count.
stuck_worker() {
  local claim_id="$1" out worktrees worker_evidence=""
  claim_reason=""
  claim_failures=0
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
    --argjson minutes "$WORKER_IDLE_MINUTES" '
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
          then error("terminal output missing") else . end)
    | if length == 0 then empty
      else max_by(.lastOutputAt)
        | . + {idle: ((now - (.lastOutputAt / 1000)) / 60)}
        | select(.idle > $minutes and (.preview | test("Sign in|ask timeout|worker_done|failed|登录")))
        | "空闲 \(.idle | floor) 分钟，preview 末尾: \(.preview[-300:] | gsub("[\\r\\n]"; " "))"
      end
    ' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse complete terminal list/worker output'; return 1
  fi
  [[ -n "$worker_evidence" ]] || return 1
  if ! out="$(claim_read linear issue "$claim_id" --comments)"; then return 1; fi
  if jq -e '.result.meta.sections.comments | .capReached == true or .hasMore == true' <<<"$out" >/dev/null; then
    claim_uncertain 'comments truncated'; return 1
  fi
  if ! claim_failures="$(jq '[.result.comments[] | select((.body // "") | contains("认领失效"))] | length' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot count claim failures for stuck worker'; return 1
  fi
  claim_reason="工人卡死：${worker_evidence}"
}

# Parse the ticket's `## SCOPE` section: the first non-empty line after the heading, split by whitespace
# into globs. A missing/empty section prints nothing (unknown); any read failure returns 1 (uncertain).
# Both outcomes make the caller not block the candidate.
scope_of() {
  local out
  if ! out="$(ocall linear issue "$1")"; then return 1; fi
  jq -r '.result.issue.description // ""' <<<"$out" | awk '
    found && NF > 0 { print; exit }
    /^## SCOPE([[:space:]]|$)/ { found = 1 }
  '
}

# True iff any glob in A matches any token in B, either direction, same pattern rule as gate.sh match_any
# (`*` crosses `/`). A literal pair also matches.
scope_overlap() {
  local a="$1" b="$2" x y
  for x in $a; do
    for y in $b; do
      # shellcheck disable=SC2053
      [[ "$x" == $y || "$y" == $x ]] && return 0
    done
  done
  return 1
}

in_flight_ids="$(ids "$LABEL_CLAIMED" started)"
ready_ids="$(ids "$LABEL_READY" unstarted)"
released=0
escalated=0
live=()
stale_ids=()
for id in $in_flight_ids; do
  if stale "$id"; then
    mark_stale "$id"; stale_ids+=("$id")
  elif expired_claim "$id"; then
    release_claim "$id"
  elif orphan_claim "$id"; then
    release_claim "$id"
  elif stuck_worker "$id"; then
    release_claim "$id"
  else
    live+=("$id")
  fi
done
in_flight=${#live[@]}
# Cache every live ticket's SCOPE once; a ticket whose SCOPE is unknown or unreadable contributes nothing
# and never blocks a candidate.
declare -A live_scope=()
if (( ${#live[@]} > 0 )); then
  for id in "${live[@]}"; do
    if s="$(scope_of "$id")" && [[ -n "$s" ]]; then live_scope["$id"]="$s"; fi
  done
fi
claimable=()
for id in $ready_ids; do
  if ! unblocked "$id"; then continue; fi
  cand_scope=""
  if ! cand_scope="$(scope_of "$id")"; then cand_scope=""; fi
  overlap=""
  if [[ -n "$cand_scope" && ${#live_scope[@]} -gt 0 ]]; then
    for lid in "${!live_scope[@]}"; do
      if scope_overlap "$cand_scope" "${live_scope[$lid]}"; then overlap="$lid"; break; fi
    done
  fi
  if [[ -n "$overlap" ]]; then
    echo "skip $id: scope overlaps $overlap" >&2
  else
    claimable+=("$id")
  fi
done
# This advisory read must never change claimability or abort the scan.
failed_backlog='?'
if failed_out="$(ocall linear list-issues --team "$TEAM" --project "$PROJECT" --label "$LABEL_FAILED" --order-by createdAt)"; then
  failed_backlog="$(jq -er '
    .result.issues | if type != "array" then error("issues missing") else . end
    | map(select(.state.type != "completed" and .state.type != "canceled")) | length
  ' <<<"$failed_out" 2>/dev/null)" || failed_backlog='?'
fi
echo "claimable=$(IFS=,; echo "${claimable[*]-}") in_flight=$in_flight max=$MAX_IN_FLIGHT"
echo "stale=$(IFS=,; echo "${stale_ids[*]-}")"
echo "health=live=$in_flight max=$MAX_IN_FLIGHT released=$released escalated=$escalated failed_backlog=$failed_backlog"
(( ${#claimable[@]} > 0 && in_flight < MAX_IN_FLIGHT ))
