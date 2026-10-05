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
WORKER_MAX_MINUTES="${AGENT_WORKER_MAX_MINUTES:-90}"
# Batch claim: 1 = legacy single claim; >1 = per-run cap; 0 = no per-run cap (only MAX_IN_FLIGHT caps).
BATCH_SIZE="${AGENT_BATCH_SIZE:-1}"
# SCOPE avoidance switch: 1 = enforce candidate↔in-flight overlap and (when batching) candidate↔candidate;
# 0 = skip every SCOPE check (worktree + merge rebase resolve conflicts instead).
SCOPE_AVOID="${AGENT_SCOPE_AVOID:-1}"

# Lossy, out-of-band tally that lets the swarm board see who spends Linear quota.
# Writes are append-only, one line per call: "<epoch> <source>". Failures ignored.
# The two lines live in-script because this file may be run from stdin via `git show | bash`.
_linear_count_file() { printf '%s' "${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}"; }
_count_linear_call() { { printf '%s precheck\n' "$(date +%s)" >> "$(_linear_count_file)"; } 2>/dev/null || true; }

# One Linear reader per run: precheck writes a snapshot that prune, the swarm board and other
# consumers read instead of hitting Linear themselves. The file is a cache, not state: a failed
# write or a stale/missing snapshot is harmless, so writes never change the precheck exit code.
SNAPSHOT_FILE="${AGENT_SNAPSHOT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-snapshot.json}"
SNAPSHOT_COMMENT_TTL_SECONDS="${AGENT_SNAPSHOT_COMMENT_TTL_SECONDS:-600}"
# Cheap tally rotation: keep the tail so the file never grows without bound.
{
  _lcf="$(_linear_count_file)"
  if [[ -f "$_lcf" ]] && (( $(wc -l < "$_lcf" 2>/dev/null || echo 0) > 5000 )); then
    tail -n 3000 "$_lcf" > "$_lcf.tmp" && mv "$_lcf.tmp" "$_lcf"
  fi
  unset _lcf
} 2>/dev/null || true

# Files survive command substitutions and work with macOS bash 3.2.
precheck_tmp="$(mktemp -d "${TMPDIR:-/tmp}/linear-precheck.XXXXXX")"
: > "$precheck_tmp/calls"
finish_precheck() {
  local status=$?
  if [[ -f "$precheck_tmp/rate-limited" ]]; then
    echo '不确定: linear rate limited' >&2
    status=3
  fi
  { write_linear_snapshot; } 2>/dev/null || true
  echo "linear_calls=$(wc -l < "$precheck_tmp/calls" | tr -d ' ')" >&2
  rm -rf "$precheck_tmp"
  exit "$status"
}
trap finish_precheck EXIT

rate_guard() {
  [[ ! -f "$precheck_tmp/rate-limited" ]] || exit 3
}

ocall() {
  local out attempt attempts=3 cache="" status
  rate_guard
  if [[ "$1 $2" == 'linear issue' ]]; then
    cache="$precheck_tmp/issue-$(jq -rn --arg id "$3" '$id | @uri')-${4:-detail}"
    if [[ -f "$cache.status" ]]; then
      status="$(cat "$cache.status")"
      cat "$cache"
      return "$status"
    fi
    # Even failed issue reads are cached: each section is requested at most once.
    attempts=1
  fi
  [[ "${claim_bounded:-0}" != 1 ]] || attempts=1
  for ((attempt=1; attempt<=attempts; attempt++)); do
    rate_guard
    [[ "$1" != linear ]] || { echo call >> "$precheck_tmp/calls"; _count_linear_call; }
    status=0
    if [[ "${claim_bounded:-0}" == 1 ]]; then
      out="$(perl -e 'alarm 20; exec @ARGV' -- orca "$@" --json 2> "$precheck_tmp/stderr")" || status=$?
    else
      out="$(orca "$@" --json 2> "$precheck_tmp/stderr")" || status=$?
    fi
    if [[ "$status" == 0 ]] && jq -e '.ok == true' <<<"$out" >/dev/null 2>&1; then
      if [[ -n "$cache" ]]; then
        printf '%s' "$out" > "$cache"
        echo 0 > "$cache.status"
      fi
      printf '%s' "$out"
      return 0
    fi
    if [[ "$1" == linear ]] && { [[ "$out" == *'Rate limit'* ]] || grep -q 'Rate limit' "$precheck_tmp/stderr"; }; then
      touch "$precheck_tmp/rate-limited"
      return 3
    fi
    (( attempt >= attempts )) || sleep 2
  done
  if [[ -n "$cache" ]]; then
    printf '%s' "$out" > "$cache"
    echo 1 > "$cache.status"
  fi
  echo "orca $* failed: $(jq -r '.error.code // "unknown"' <<<"$out" 2>/dev/null)" >&2
  printf '%s' "$out"
  return 1
}

ids() {
  local out
  if [[ ! -f "$precheck_tmp/issues" ]]; then
    out="$(ocall linear list-issues --team "$TEAM" --project "$PROJECT" --order-by createdAt)" || exit 3
    printf '%s' "$out" > "$precheck_tmp/issues"
  fi
  # priority: 1 urgent … 4 low; 0/null = none -> 9 so it sorts after every explicit priority.
  jq -r --arg label "$1" --arg type "$2" '
    .result.issues
    | sort_by(((.priority // 0) | if . == 0 then 9 else . end), .createdAt)[]
    | select(any(.labels[]?; .id == $label or .name == $label))
    | select(.state.type == $type) | .identifier' "$precheck_tmp/issues"
}

# Cross-run cache of a candidate ticket's blocked-by identifiers. Relations only change when
# tickets are created or merged, so a short TTL is safe and skips one Linear call per candidate
# per run. The cache is a pure optimization: write failures are ignored, failed reads never
# populate it, and nothing downstream reads it except the relations fetch itself.
PRECHECK_CACHE_DIR="${AGENT_PRECHECK_CACHE_DIR:-${TMPDIR:-/tmp}/orca-factory-precheck-cache}"
RELATION_TTL_SECONDS="${AGENT_RELATION_TTL_SECONDS:-600}"
mkdir -p "$PRECHECK_CACHE_DIR" 2>/dev/null || true
mkdir -p "$precheck_tmp/relations-ok" 2>/dev/null || true

relations_cache_file() {
  printf '%s/relations-%s' "$PRECHECK_CACHE_DIR" "$(jq -rn --arg id "$1" '$id | @uri')"
}

# Prints a space-separated list of blocker identifiers (empty when there are none, which is the
# common case and still worth caching). Returns 1 only when the live read failed. On every success
# path (cache hit or fresh fetch) touches `relations-ok/<id>` so the snapshot writer knows the
# relations for that candidate are good this run; a failed read leaves the flag absent.
relations_of() {
  local id="$1" cache now ts="" rel blockers
  cache="$(relations_cache_file "$id")"
  now="$(date +%s)"
  if [[ -f "$cache" ]]; then
    IFS= read -r ts < "$cache" 2>/dev/null || ts=""
    if [[ -n "$ts" && "$ts" =~ ^[0-9]+$ ]] && (( now - ts < RELATION_TTL_SECONDS )); then
      { : > "$precheck_tmp/relations-ok/$id"; } 2>/dev/null || true
      sed -n '2p' "$cache"
      return 0
    fi
  fi
  rel="$(ocall linear issue "$id" --relations)" || return 1
  blockers="$(jq -r '
    [(.result.relations // [])[] | select(.relationship == "blockedBy") | .relatedIssue.identifier]
    | join(" ")' <<<"$rel")"
  { printf '%s\n%s\n' "$now" "$blockers" > "$cache"; } 2>/dev/null || true
  { : > "$precheck_tmp/relations-ok/$id"; } 2>/dev/null || true
  printf '%s\n' "$blockers"
}

# Reads a blocker's state.type from the full-project snapshot already fetched by ids(); only
# falls back to a per-ticket Linear call if the blocker is missing from the snapshot (eg. it
# belongs to a different project). Blocker state itself is never cached across runs: it decides
# whether a candidate can be claimed and must stay fresh.
blocker_state() {
  local id="$1" out state
  state="$(jq -r --arg id "$id" '
    .result.issues | map(select(.identifier == $id)) | .[0] | .state.type // empty
  ' "$precheck_tmp/issues" 2>/dev/null)"
  if [[ -n "$state" ]]; then printf '%s' "$state"; return 0; fi
  out="$(ocall linear issue "$id")" || return 1
  jq -r '.result.issue.state.type' <<<"$out"
}

# Candidate-local tolerance for a transient relations read failure: skip just that candidate,
# keep evaluating the rest, and only stop the whole scan once three candidates have failed in one
# run or the error names a rate limit. Blocker-state failures still abort because they decide
# claimability and have their own retry inside ocall.
unblocked() {
  local blockers blocker state err_cache err_msg
  if ! blockers="$(relations_of "$1")"; then
    err_cache="$precheck_tmp/issue-$(jq -rn --arg id "$1" '$id | @uri')---relations"
    err_msg=""
    if [[ -f "$err_cache" ]]; then
      err_msg="$(jq -r '.error.message // .error.code // "unknown"' "$err_cache" 2>/dev/null)" || err_msg=""
    fi
    [[ -n "$err_msg" ]] || err_msg="unknown"
    if [[ -f "$precheck_tmp/rate-limited" ]] || printf '%s' "$err_msg" | grep -iq 'rate limit'; then
      touch "$precheck_tmp/rate-limited" 2>/dev/null || true
      exit 3
    fi
    echo "不确定: $1 依赖读取失败（$err_msg），本轮跳过该票" >&2
    rel_fail_count=$((rel_fail_count + 1))
    (( rel_fail_count >= 3 )) && exit 3
    return 2
  fi
  for blocker in $blockers; do
    state="$(blocker_state "$blocker")" || exit 3
    case "$state" in completed|canceled) ;; *) return 1 ;; esac
  done
}

# Snapshot comment cache: lives on disk under `${AGENT_PRECHECK_CACHE_DIR}/comments-<id>` with the
# epoch seconds on line 1 and the trimmed JSON array on the rest. A fresh (<10 min by default)
# cache replays without a Linear call. On a miss we prefer the in-run ocall cache (populated by
# the stale/orphan/overrun/stuck checks when the ticket is in-flight), and only fall back to a
# direct `orca linear issue --comments` call when neither cache helps. A failed or rate-limited
# fetch sticks with the previous cached body — or an empty array — so a snapshot read never aborts
# the scan nor changes the precheck exit code.
snapshot_comments_of() {
  local id="$1" uri cache now ts out comments run_cache run_status
  uri="$(jq -rn --arg id "$id" '$id | @uri' 2>/dev/null)" || uri="$id"
  cache="$PRECHECK_CACHE_DIR/comments-$uri"
  now="$(date +%s)"
  if [[ -f "$cache" ]]; then
    IFS= read -r ts < "$cache" 2>/dev/null || ts=""
    if [[ -n "$ts" && "$ts" =~ ^[0-9]+$ ]] && (( now - ts < SNAPSHOT_COMMENT_TTL_SECONDS )); then
      tail -n +2 "$cache"
      return 0
    fi
  fi
  out=""
  run_cache="$precheck_tmp/issue-$uri---comments"
  if [[ -f "$run_cache.status" && -f "$run_cache" ]]; then
    run_status="$(cat "$run_cache.status" 2>/dev/null)" || run_status=""
    if [[ "$run_status" == 0 ]]; then
      out="$(cat "$run_cache")"
    fi
  fi
  # Rate-limited runs still write a snapshot, but strictly from already-cached bodies: a direct
  # read here would land another Linear call after the main loop decided to stop.
  if [[ -z "$out" && ! -f "$precheck_tmp/rate-limited" ]]; then
    { echo call >> "$precheck_tmp/calls"; } 2>/dev/null || true
    _count_linear_call
    out="$(perl -e 'alarm 20; exec @ARGV' -- orca linear issue "$id" --comments --json 2>/dev/null)" || out=""
  fi
  if [[ -n "$out" ]] && comments="$(jq -ce '
    if .ok != true then error("not ok") else . end
    | (.result.comments // [])
    | sort_by(.createdAt // "") | reverse | .[0:20]
    | map({createdAt: (.createdAt // ""), body: ((.body // "")[0:2000])})
  ' <<<"$out" 2>/dev/null)"; then
    { printf '%s\n%s\n' "$now" "$comments" > "$cache"; } 2>/dev/null || true
    printf '%s\n' "$comments"
    return 0
  fi
  if [[ -f "$cache" ]]; then
    tail -n +2 "$cache"
    return 0
  fi
  printf '[]\n'
}

# Write a single-run snapshot that consumers (prune, swarm board) read instead of hitting Linear.
# Only runs when this run's project-wide list-issues call succeeded (precheck_tmp/issues exists);
# a failed list leaves the previous snapshot in place. Atomic via tempfile + rename in the same
# directory so partial writes never surface. The whole function is best effort: any error returns
# without writing.
write_linear_snapshot() {
  [[ -f "$precheck_tmp/issues" ]] || return 0
  local snapshot_dir snapshot_tmp ids_to_fetch id comments_obj new_obj entry \
        candidate_ids blocked_by_obj cache blockers_line blockers_json
  snapshot_dir="$(dirname "$SNAPSHOT_FILE")"
  mkdir -p "$snapshot_dir" 2>/dev/null || return 0
  snapshot_tmp="$(mktemp "$snapshot_dir/.linear-snapshot.XXXXXX" 2>/dev/null)" || return 0
  if ! ids_to_fetch="$(jq -r --arg lbl "$LABEL_FAILED" '
    .result.issues
    | map(select(.identifier != null))
    | map(select(
        (.state.type // "") == "started"
        or (any(.labels[]?; .id == $lbl or .name == $lbl)
            and (.state.type // "") != "completed"
            and (.state.type // "") != "canceled")
      ))
    | .[].identifier
  ' "$precheck_tmp/issues" 2>/dev/null)"; then
    ids_to_fetch=""
  fi
  comments_obj="{}"
  for id in $ids_to_fetch; do
    [[ -n "$id" ]] || continue
    entry="$(snapshot_comments_of "$id" 2>/dev/null)" || entry="[]"
    [[ -n "$entry" ]] || entry="[]"
    if new_obj="$(jq -c --arg id "$id" --argjson entry "$entry" '. + {($id): $entry}' <<<"$comments_obj" 2>/dev/null)"; then
      comments_obj="$new_obj"
    fi
  done
  # blockedBy: only ready candidates whose relations_of succeeded this run and whose blocker list
  # is non-empty. A failed/rate-limited read leaves no entry — never write an empty array that
  # would misrepresent an unknown state as "no blockers". No new Linear call: reuse the cache that
  # relations_of already populated.
  blocked_by_obj="{}"
  if ! candidate_ids="$(jq -r --arg lbl "$LABEL_READY" '
    .result.issues
    | map(select((.state.type // "") == "unstarted"
      and any(.labels[]?; .id == $lbl or .name == $lbl)))
    | .[].identifier // empty
  ' "$precheck_tmp/issues" 2>/dev/null)"; then
    candidate_ids=""
  fi
  for id in $candidate_ids; do
    [[ -n "$id" ]] || continue
    [[ -e "$precheck_tmp/relations-ok/$id" ]] || continue
    cache="$(relations_cache_file "$id")"
    [[ -f "$cache" ]] || continue
    blockers_line="$(sed -n '2p' "$cache" 2>/dev/null)" || continue
    [[ -n "$blockers_line" ]] || continue
    if ! blockers_json="$(printf '%s\n' "$blockers_line" | jq -Rc 'split(" ") | map(select(length > 0))' 2>/dev/null)"; then
      continue
    fi
    [[ "$blockers_json" != '[]' && -n "$blockers_json" ]] || continue
    if new_obj="$(jq -c --arg id "$id" --argjson arr "$blockers_json" '. + {($id): $arr}' <<<"$blocked_by_obj" 2>/dev/null)"; then
      blocked_by_obj="$new_obj"
    fi
  done
  if jq -nc --slurpfile src "$precheck_tmp/issues" \
    --argjson comments "$comments_obj" --argjson blockedBy "$blocked_by_obj" \
    --argjson gen "$(date +%s)" '
    {generatedAt: $gen, issues: ($src[0].result.issues // []),
     comments: $comments, blockedBy: $blockedBy}
  ' > "$snapshot_tmp" 2>/dev/null; then
    mv "$snapshot_tmp" "$SNAPSHOT_FILE" 2>/dev/null || rm -f "$snapshot_tmp" 2>/dev/null || true
  else
    rm -f "$snapshot_tmp" 2>/dev/null || true
  fi
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
  if out="$(claim_bounded=1 ocall "$@")"; then
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

# One-shot cache of open PRs for the whole run. State lives on disk so a
# subshell (command substitution) that calls open_pr_count_for sees the same
# cache the parent populated. Failure or timeout means "uncertain": every
# per-ticket PR check that depends on this returns 1 (claim stays live).
load_open_prs() {
  [[ ! -e "$precheck_tmp/open-prs.json" && ! -e "$precheck_tmp/open-prs.failed" ]] || return 0
  local out
  if out="$(perl -e 'alarm 30; exec @ARGV' -- gh pr list --repo "$GH_REPO" --state open --json number,title --limit 100 2>/dev/null)" \
    && jq -e 'type == "array"' <<<"$out" >/dev/null 2>&1; then
    printf '%s' "$out" > "$precheck_tmp/open-prs.json"
  else
    : > "$precheck_tmp/open-prs.failed"
    echo 'precheck: 开放 PR 列表获取失败，相关认领保留' >&2
  fi
}
open_pr_count_for() {
  load_open_prs
  [[ -f "$precheck_tmp/open-prs.json" ]] || return 1
  jq --arg id "$1" '[.[] | select((.title // "") | contains($id))] | length' "$precheck_tmp/open-prs.json"
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
  local claim_id="$1" out verdict pr_count
  claim_reason=""
  claim_failures=0
  if ! out="$(claim_read linear issue "$claim_id")"; then return 1; fi
  if ! verdict="$(jq -r --argjson m "$ORPHAN_MINUTES" \
    '.result.issue.updatedAt | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601 | (now - .) >= ($m * 60)' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse updatedAt for orphan check'; return 1
  fi
  [[ "$verdict" == true ]] || return 1
  if ! pr_count="$(open_pr_count_for "$claim_id")"; then
    claim_uncertain "gh pr list cache unavailable for $claim_id"; return 1
  fi
  (( pr_count == 0 )) || return 1
  if ! out="$(claim_read linear issue "$claim_id" --comments)"; then return 1; fi
  if ! claim_failures="$(jq '[.result.comments[] | select((.body // "") | contains("认领失效"))] | length' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot count claim failures for orphan check'; return 1
  fi
  claim_reason="在途超过 ${ORPHAN_MINUTES} 分钟无更新且无 PR，coordinator 疑似已结束"
}

# Bound total time since the newest claim even when worker activity refreshes updatedAt.
# Require a complete comment snapshot, a still-started issue and no open PR; uncertain
# reads keep the ticket live, using the same bounded reads as orphan_claim.
overrun_claim() {
  local claim_id="$1" out verdict pr_count
  claim_reason=""
  claim_failures=0
  if ! out="$(claim_read linear issue "$claim_id")"; then return 1; fi
  if ! verdict="$(jq -er '
    .result.issue.state.type
    | if type == "string" and length > 0 then . else error("state missing") end
    ' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse state for overrun check'; return 1
  fi
  [[ "$verdict" == started ]] || return 1
  if ! out="$(claim_read linear issue "$claim_id" --comments)"; then return 1; fi
  if jq -e '.result.meta.sections.comments | .capReached == true or .hasMore == true' <<<"$out" >/dev/null; then
    claim_uncertain 'comments truncated for overrun check'; return 1
  fi
  if ! verdict="$(jq -er --argjson m "$WORKER_MAX_MINUTES" '
    .result.comments | if type != "array" then error("comments missing") else . end
    | map(select((.body // "") | startswith("已领取"))
      | .createdAt | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601)
    | if length == 0 then "live"
      elif (now - max) > ($m * 60) then "overrun" else "live" end
    ' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot parse claim comments for overrun check'; return 1
  fi
  [[ "$verdict" == overrun ]] || return 1
  if ! claim_failures="$(jq '[.result.comments[] | select((.body // "") | contains("认领失效"))] | length' <<<"$out" 2>/dev/null)"; then
    claim_uncertain 'cannot count claim failures for overrun check'; return 1
  fi
  if ! pr_count="$(open_pr_count_for "$claim_id")"; then
    claim_uncertain "gh pr list cache unavailable for $claim_id"; return 1
  fi
  (( pr_count == 0 )) || return 1
  claim_reason="认领后超过 ${WORKER_MAX_MINUTES} 分钟未产出 PR"
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
scope_overlap() (
  # Split scope tokens without expanding globs against this checkout.
  set -f
  local a="$1" b="$2" x y
  for x in $a; do
    for y in $b; do
      # shellcheck disable=SC2053
      [[ "$x" == $y || "$y" == $x ]] && return 0
    done
  done
  return 1
)

# Only a complete, freshly fetched set of unit diffs can replace the declared scope.
branch_scope_of() {
  local refs ref files
  refs="$(git for-each-ref --format='%(refname)' "refs/remotes/origin/agent/$1/")" || return 1
  [[ -n "$refs" ]] || return 1
  : > "$precheck_tmp/branch-scope"
  while IFS= read -r ref; do
    files="$(git diff --name-only --no-renames "origin/main...$ref" --)" || return 1
    [[ -z "$files" ]] || printf '%s\n' "$files" >> "$precheck_tmp/branch-scope"
  done <<<"$refs"
  sort -u "$precheck_tmp/branch-scope"
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
  elif overrun_claim "$id"; then
    release_claim "$id"
  elif stuck_worker "$id"; then
    release_claim "$id"
  else
    live+=("$id")
  fi
done
in_flight=${#live[@]}
# Fetch once per scan, pruning deleted unit refs. Failures retain declared SCOPE;
# AGENT_SCOPE_AVOID=0 neither fetches nor populates the conflict cache.
mkdir "$precheck_tmp/live-scope"
if (( SCOPE_AVOID == 1 && ${#live[@]} > 0 )); then
  branch_fetch_ok=0
  if command -v timeout >/dev/null 2>&1; then
    if timeout 20 git fetch --quiet --prune origin '+refs/heads/agent/*:refs/remotes/origin/agent/*' 2>/dev/null; then branch_fetch_ok=1; fi
  else
    if perl -e 'alarm 20; exec @ARGV' -- git fetch --quiet --prune origin '+refs/heads/agent/*:refs/remotes/origin/agent/*' 2>/dev/null; then branch_fetch_ok=1; fi
  fi
  for id in "${live[@]}"; do
    if (( branch_fetch_ok == 1 )) && s="$(branch_scope_of "$id" 2>/dev/null)"; then
      printf '%s' "$s" > "$precheck_tmp/live-scope/$id"
    elif s="$(scope_of "$id")" && [[ -n "$s" ]]; then
      printf '%s' "$s" > "$precheck_tmp/live-scope/$id"
    fi
  done
fi
# How many candidates this run may accept: capped by free MAX_IN_FLIGHT slots, then by AGENT_BATCH_SIZE
# (0 means no per-run cap). BATCH_SIZE=1 preserves the legacy single-claim behavior.
slots=$(( MAX_IN_FLIGHT - in_flight ))
(( slots < 0 )) && slots=0
(( BATCH_SIZE > 0 && slots > BATCH_SIZE )) && slots=$BATCH_SIZE
# Candidate↔candidate SCOPE avoidance kicks in only when SCOPE_AVOID=1 and this run may accept more than one.
cand_avoid=0
if (( SCOPE_AVOID == 1 )) && (( BATCH_SIZE != 1 )); then cand_avoid=1; fi
claimable=()
declare -A accepted_scope=()
rel_fail_count=0
for id in $ready_ids; do
  (( ${#claimable[@]} >= slots )) && break
  if ! unblocked "$id"; then continue; fi
  cand_scope=""
  if (( SCOPE_AVOID == 1 )); then
    if ! cand_scope="$(scope_of "$id")"; then cand_scope=""; fi
  fi
  overlap=""
  if (( SCOPE_AVOID == 1 )) && [[ -n "$cand_scope" ]]; then
    if (( ${#live[@]} > 0 )); then
      for lid in "${live[@]}"; do
        [[ -f "$precheck_tmp/live-scope/$lid" ]] || continue
        if scope_overlap "$cand_scope" "$(cat "$precheck_tmp/live-scope/$lid")"; then overlap="$lid"; break; fi
      done
    fi
    if [[ -z "$overlap" ]] && (( cand_avoid == 1 && ${#accepted_scope[@]} > 0 )); then
      for aid in "${!accepted_scope[@]}"; do
        if scope_overlap "$cand_scope" "${accepted_scope[$aid]}"; then overlap="$aid"; break; fi
      done
    fi
  fi
  if [[ -n "$overlap" ]]; then
    echo "skip $id: scope overlaps $overlap" >&2
  else
    claimable+=("$id")
    if (( cand_avoid == 1 )) && [[ -n "$cand_scope" ]]; then accepted_scope["$id"]="$cand_scope"; fi
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
rate_guard
echo "claimable=$(IFS=,; echo "${claimable[*]-}") in_flight=$in_flight max=$MAX_IN_FLIGHT"
echo "stale=$(IFS=,; echo "${stale_ids[*]-}")"
echo "health=live=$in_flight max=$MAX_IN_FLIGHT released=$released escalated=$escalated failed_backlog=$failed_backlog"
(( ${#claimable[@]} > 0 && in_flight < MAX_IN_FLIGHT ))
