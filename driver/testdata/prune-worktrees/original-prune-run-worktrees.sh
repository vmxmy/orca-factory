#!/usr/bin/env bash
# Remove worktrees left by finished automation runs. Each run gets a fresh worktree and a coordinator
# cannot delete its own, so every new run prunes its predecessors. A run is finished once its
# coordinator wrote .omc/agent-run-finished; runs older than MAX_AGE_HOURS are treated as crashed.
# Terminal liveness is no signal here: every Orca worktree keeps a shell open and an idle agent TUI stays up.
set -Eeuo pipefail

REPO="path:${AGENT_REPO_PATH:?main checkout path}"
PREFIX="${AGENT_RUN_WORKTREE_PREFIX:?set in agent-graph.env}"
LINEAR_TEAM="${AGENT_LINEAR_TEAM:?set in agent-graph.env}"
LINEAR_PROJECT="${AGENT_LINEAR_PROJECT:?set in agent-graph.env}"
MAX_AGE_HOURS="${AGENT_RUN_MAX_AGE_HOURS:-24}"
PRUNE_BUDGET_SECONDS="${AGENT_PRUNE_BUDGET_SECONDS:-60}"
PRUNE_MAX_PER_RUN="${AGENT_PRUNE_MAX_PER_RUN:-5}"
self="$(pwd -P)"
cutoff_ms=$(( ($(date +%s) - MAX_AGE_HOURS * 3600) * 1000 ))
prune_started_at=$(date +%s)
prune_removed_count=0
prune_budget_tripped=0
prune_quota_tripped=0
prune_linear_calls=0
prune_linear_ok=false
prune_linear_data='[]'
prune_report_linear_calls() { echo "prune_linear_calls=$prune_linear_calls" >&2; }
trap prune_report_linear_calls EXIT
# Lossy, out-of-band tally shared with the swarm board (writes ignored on failure).
_prune_count_linear_call() {
  { printf '%s prune\n' "$(date +%s)" \
    >> "${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}"; } 2>/dev/null || true
}
prune_budget_exceeded() {
  (( prune_budget_tripped == 1 )) && return 0
  local now=$(date +%s)
  if (( now - prune_started_at >= PRUNE_BUDGET_SECONDS )); then
    echo 'prune: 超过时间预算，剩余对象留到下次' >&2
    prune_budget_tripped=1
    return 0
  fi
  return 1
}
prune_quota_reached() {
  (( prune_quota_tripped == 1 )) && return 0
  if (( prune_removed_count >= PRUNE_MAX_PER_RUN )); then
    echo 'prune: 达到单次回收上限，剩余对象留到下次' >&2
    prune_quota_tripped=1
    return 0
  fi
  return 1
}
prune_remote_heads=''
prune_remote_heads_ok=false
prune_load_remote_heads() {
  [[ "$prune_remote_heads_ok" == false ]] || return 0
  if prune_remote_heads=$(git -C "$AGENT_REPO_PATH" ls-remote --heads origin 2>/dev/null); then
    prune_remote_heads_ok=true
  fi
}
# Lookup a cached remote head sha for a given short branch name.
prune_remote_sha_of() {
  [[ "$prune_remote_heads_ok" == true ]] || return 1
  awk -v ref="refs/heads/$1" '$2 == ref { print $1; found=1; exit } END { if (!found) exit 1 }' <<<"$prune_remote_heads"
}

# Compensate for merged workers that never received a coordinator marker. Keep the
# original inventories for issue associations even after a worktree is removed.
prune_keep() { printf 'skip %s: %s\n' "$1" "${2//$'\n'/ }" >&2; }
prune_git_worktrees() {
  local out
  out=$(git -C "$AGENT_REPO_PATH" -c core.quotePath=false worktree list --porcelain) || return 1
  jq -Rsc 'split("\n\n") | map(select(startswith("worktree ")) |
    split("\n") | {path:(.[0] | ltrimstr("worktree ")),
    branch:([.[] | select(startswith("branch ")) | ltrimstr("branch refs/heads/")][0] // ""),
    locked:any(.[]; . == "locked" or startswith("locked "))})' <<<"$out"
}
# One Linear snapshot per run covers every ticket we inspect. Prefer the shared snapshot
# precheck writes (within 900 s of now, same shape as list-issues' .result.issues); only call
# Linear ourselves if the file is missing, stale or malformed. prune_linear_calls therefore
# ends up 0 when the snapshot carries us, or 1 when we had to fall back to a live read.
prune_load_linear_issues() {
  [[ "$prune_linear_ok" == true ]] && return 0
  local snapshot now generated snapshot_issues
  snapshot="${AGENT_SNAPSHOT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-snapshot.json}"
  if [[ -f "$snapshot" ]]; then
    now=$(date +%s)
    if generated=$(jq -er '.generatedAt | if type == "number" then . else error("missing") end' "$snapshot" 2>/dev/null) \
      && (( now - generated <= 900 )) \
      && snapshot_issues=$(jq -ce '.issues | if type == "array" then . else error("missing issues") end' "$snapshot" 2>/dev/null); then
      prune_linear_data="$snapshot_issues"
      prune_linear_ok=true
      return 0
    fi
  fi
  local out stderr status=0
  stderr=$(mktemp)
  prune_linear_calls=$((prune_linear_calls + 1))
  _prune_count_linear_call
  out=$(orca linear list-issues --team "$LINEAR_TEAM" --project "$LINEAR_PROJECT" --json 2>"$stderr") || status=$?
  if (( status == 0 )) && jq -e '.ok == true and (.result.issues | type == "array")' <<<"$out" >/dev/null 2>&1; then
    prune_linear_data=$(jq -c '.result.issues' <<<"$out")
    prune_linear_ok=true
    rm -f "$stderr"
    return 0
  fi
  if [[ "$out" == *'Rate limit'* ]] || grep -q 'Rate limit' "$stderr" 2>/dev/null; then
    echo 'prune: 不确定: linear rate limited' >&2
  fi
  rm -f "$stderr"
  return 1
}
prune_ticket_check() {
  local mode="${3:-completed}"
  [[ "$prune_linear_ok" == true ]] || {
    prune_keep "$2" "linear snapshot unavailable, cannot verify ticket $1"
    return 1
  }
  if ! jq -e --arg id "$1" --arg mode "$mode" '
    map(select(.identifier == $id)) | .[0] as $issue |
    if $issue == null then false
    elif ($issue.state.type | type != "string") then false
    elif $mode == "completed" then $issue.state.type == "completed"
    else $issue.state.type != "started"
    end' <<<"$prune_linear_data" >/dev/null 2>&1; then
    prune_keep "$2" "ticket $1 is not confirmed $mode (in flight or missing from Linear snapshot)"
    return 1
  fi
}
# Check both explicit Orca links and identifiers embedded in branch/path names.
# Keep started issues (including auto-impl) and failed lookups out of every path.
# Only Orca workers and remote backups additionally require completed tickets.
prune_issue_safe() {
  local wt="$1" branch="$2" ids id
  [[ "$prune_inventory_ok" == true ]] || { prune_keep "${wt:-$branch}" 'Orca inventory unavailable'; return 1; }
  ids=$(jq -r --arg wt "$wt" --arg branch "$branch" --argjson git "$prune_worktrees" '
    .result.worktrees[] | select(.path == $wt or
      (.path as $p | any($git[]; .path == $p and .branch == $branch and $branch != ""))) |
    .linkedLinearIssue // empty' <<<"$prune_orca") || return 1
  if [[ "$branch $wt" =~ ([A-Z][A-Z0-9]*-[0-9]+) ]]; then
    ids="${ids}${ids:+$'\n'}${BASH_REMATCH[1]}"
  fi
  while IFS= read -r id; do
    [[ -n "$id" ]] || continue
    prune_ticket_check "$id" "${wt:-$branch}" inactive || return 1
  done <<<"$ids"
}
# Linked workers were already evaluated above; legacy marker/age rules must not
# bypass a failed compensation check (dirty checkout, unmerged head, or API error).
prune_legacy_safe() {
  local branch
  [[ "$prune_inventory_ok" == true ]] || { prune_keep "$1" 'Orca inventory unavailable'; return 1; }
  if jq -e --arg wt "$1" 'any(.result.worktrees[]; .path == $wt and (.linkedLinearIssue // "") != "")' <<<"$prune_orca" >/dev/null; then
    return 1
  fi
  branch=$(jq -r --arg wt "$1" '.[] | select(.path == $wt) | .branch' <<<"$prune_worktrees") || return 1
  prune_issue_safe "$1" "$branch"
}
prune_clean() {
  local status
  if ! status=$(git -C "$1" status --porcelain=v1 --untracked-files=all 2>&1); then
    prune_keep "$1" "git status failed: $status"; return 1
  fi
  [[ -z "$status" ]] || { prune_keep "$1" 'worktree is dirty'; return 1; }
}
prune_merged() {
  [[ "$prune_pr_ok" == true ]] && jq -e --arg sha "$1" --arg branch "${2:-}" '
    any(.[]; .merged_at != null and .head.sha == $sha and ($branch == "" or .head.ref == $branch))' <<<"$prune_prs" >/dev/null
}
# Return cached MERGED/CLOSED state for a PR number, or 1 when not found in the cache.
prune_pr_state_of() {
  [[ "$prune_pr_ok" == true ]] || return 1
  local state
  state=$(jq -r --arg pr "$1" '
    (.[] | select(.number == ($pr | tonumber)) |
      if .merged_at != null then "MERGED" else "CLOSED" end) // empty' <<<"$prune_prs") || return 1
  [[ -n "$state" ]] || return 1
  printf '%s' "$state"
}
prune_compensate() {
  local wt branch sha remote_sha result row ticket occupied current
  prune_inventory_ok=false
  prune_worktrees='[]'
  if ! prune_worktrees=$(prune_git_worktrees); then
    prune_keep compensation 'Git worktree inventory unavailable'; return 0
  fi
  if prune_orca=$(orca worktree list --repo "$REPO" --json 2>/dev/null) &&
    jq -e '.ok == true and (.result.worktrees | type == "array")' <<<"$prune_orca" >/dev/null 2>&1; then
    prune_inventory_ok=true
  else
    prune_keep compensation 'Orca worktree inventory unavailable'; return 0
  fi
  # One list-issues call replaces per-object `linear issue` lookups; a failure (rate limit or
  # otherwise) stops compensation so we never remove a worktree with an unknown ticket state.
  if ! prune_load_linear_issues; then
    echo 'prune: 不确定: linear unavailable，跳过补偿' >&2
    return 0
  fi
  prune_pr_ok=false
  prune_prs='[]'
  if [[ -n "${AGENT_GH_REPO:-}" ]] &&
    result=$(gh api --paginate "repos/$AGENT_GH_REPO/pulls?state=closed&per_page=100" 2>/dev/null) &&
    prune_prs=$(jq -se --arg repo "$AGENT_GH_REPO" '
      if all(.[]; type == "array") then add | map(select(.head.repo.full_name == $repo))
      else error("invalid PR pages") end' <<<"$result" 2>/dev/null); then
    prune_pr_ok=true
  else
    prune_keep compensation 'closed PR query failed; PR-dependent objects retained'
  fi
  prune_load_remote_heads
  # Orca workers need a completed linked issue and either a pushed head or a merged head.
  while IFS= read -r row; do
    prune_budget_exceeded && break
    prune_quota_reached && break
    wt=$(jq -r '.path' <<<"$row")
    ticket=$(jq -r '.linkedLinearIssue' <<<"$row")
    [[ "$wt" != "$self" && "$wt" != "$main" ]] || continue
    branch=$(git -C "$wt" symbolic-ref --quiet --short HEAD) || { prune_keep "$wt" 'branch unavailable'; continue; }
    prune_issue_safe "$wt" "$branch" || continue
    prune_ticket_check "$ticket" "$wt" || continue
    prune_clean "$wt" || continue
    sha=$(git -C "$wt" rev-parse --verify HEAD) || { prune_keep "$wt" 'head unavailable'; continue; }
    remote_sha=$(prune_remote_sha_of "$branch") || remote_sha=''
    if [[ "$sha" != "$remote_sha" ]] && ! prune_merged "$sha"; then
      prune_keep "$wt" 'head is neither confirmed pushed nor merged'; continue
    fi
    if ! result=$(orca worktree rm --worktree "path:$wt" --json 2>&1) ||
      ! jq -e '.ok == true' <<<"$result" >/dev/null 2>&1; then
      prune_keep "$wt" "Orca removal failed: $result"
    else
      prune_removed_count=$((prune_removed_count + 1))
    fi
  done < <(jq -c '.result.worktrees[] | select((.linkedLinearIssue // "") != "")' <<<"$prune_orca")
  # Git-only Claude worktrees; never hand Orca-managed paths to Git removal.
  while IFS= read -r row; do
    prune_budget_exceeded && break
    prune_quota_reached && break
    wt=$(jq -r '.path' <<<"$row")
    branch=$(jq -r '.branch' <<<"$row")
    case "$wt" in "$main"/.claude/worktrees/agent-*) ;; *) continue ;; esac
    [[ "$wt" != "$self" ]] || continue
    jq -e --arg wt "$wt" 'any(.result.worktrees[]; .path == $wt)' <<<"$prune_orca" >/dev/null && continue
    if [[ $(jq -r '.locked' <<<"$row") == true ]]; then prune_keep "$wt" 'worktree is locked'; continue; fi
    prune_issue_safe "$wt" "$branch" || continue
    prune_clean "$wt" || continue
    sha=$(git -C "$wt" rev-parse --verify HEAD) || { prune_keep "$wt" 'head unavailable'; continue; }
    prune_merged "$sha" || { prune_keep "$wt" 'head has no matching merged PR'; continue; }
    if git -C "$AGENT_REPO_PATH" worktree remove "$wt"; then
      prune_removed_count=$((prune_removed_count + 1))
    else
      prune_keep "$wt" 'Git removal failed'
    fi
  done < <(jq -c '.[]' <<<"$prune_worktrees")
  # Refresh occupancy after removals, including locked worktrees and the main checkout.
  if current=$(prune_git_worktrees) && result=$(git -C "$AGENT_REPO_PATH" for-each-ref --format='%(objectname) %(refname:short)' refs/heads/); then
    while read -r sha branch; do
      prune_budget_exceeded && break
      prune_quota_reached && break
      [[ -n "$branch" ]] || continue
      occupied=$(jq -r --arg branch "$branch" 'any(.[]; .branch == $branch)' <<<"$current")
      [[ "$occupied" == false ]] || continue
      prune_issue_safe '' "$branch" || continue
      prune_merged "$sha" || { prune_keep "$branch" 'head has no matching merged PR'; continue; }
      if git -C "$AGENT_REPO_PATH" branch -D -- "$branch"; then
        prune_removed_count=$((prune_removed_count + 1))
      else
        prune_keep "$branch" 'branch removal failed'
      fi
    done <<<"$result"
  else
    prune_keep 'local branches' 'cannot verify branch occupancy'
  fi
  # Reuse the cached remote heads to find agent/* branches without another network call.
  if [[ "$prune_remote_heads_ok" == true ]]; then
    while read -r sha branch; do
      prune_budget_exceeded && break
      prune_quota_reached && break
      [[ "$branch" =~ ^refs/heads/agent/([A-Z][A-Z0-9]*-[0-9]+)/[^/]+$ ]] || continue
      ticket=${BASH_REMATCH[1]}
      branch=${branch#refs/heads/}
      prune_issue_safe '' "$branch" || continue
      prune_ticket_check "$ticket" "$branch" || continue
      prune_merged "$sha" "$branch" || { prune_keep "$branch" 'remote head has no matching merged PR'; continue; }
      if git -C "$AGENT_REPO_PATH" push origin --delete "$branch"; then
        prune_removed_count=$((prune_removed_count + 1))
      else
        prune_keep "$branch" 'remote removal failed'
      fi
    done < <(awk '$2 ~ "^refs/heads/agent/" { print }' <<<"$prune_remote_heads")
  else
    prune_keep 'remote branches' 'remote heads query failed'
  fi
}
main="$(cd "$AGENT_REPO_PATH" && pwd -P)"
prune_inventory_ok=false
prune_worktrees='[]'
prune_compensate

while read -r created wt; do
  prune_quota_reached && break
  [[ "$wt" == "$self" ]] && continue
  prune_legacy_safe "$wt" || continue
  [[ -f "$wt/.omc/agent-run-finished" ]] || (( created < cutoff_ms )) || continue
  orca worktree rm --worktree "path:$wt" --force --json | jq -r --arg wt "$wt" '"pruned \($wt) ok=\(.ok)"'
  prune_removed_count=$((prune_removed_count + 1))
done < <(orca worktree list --repo "$REPO" --json \
  | jq -r --arg p "$PREFIX" '.result.worktrees[] | select(.path | split("/") | last | startswith($p)) | "\(.createdAt) \(.path)"')

# Only coordinator-marked units with a finished PR and a clean checkout are eligible.
main="$(cd "$AGENT_REPO_PATH" && pwd -P)"
while IFS= read -r wt; do
  prune_quota_reached && break
  [[ "$wt" == "$self" || "$wt" == "$main" || "$wt" == "$AGENT_REPO_PATH" ]] && continue
  prune_legacy_safe "$wt" || continue
  marker="$wt/.omc/agent-unit"
  [[ -e "$marker" || -L "$marker" ]] || continue
  if ! record=$(cat "$marker" 2>&1); then
    printf 'skip %s: cannot read agent-unit: %s\n' "$wt" "${record//$'\n'/ }" >&2
    continue
  fi
  if [[ ! "$record" =~ ^pr=([1-9][0-9]*)$ ]]; then
    printf 'skip %s: invalid agent-unit (expected pr=<N>)\n' "$wt" >&2
    continue
  fi
  pr="${BASH_REMATCH[1]}"
  if [[ -z "${AGENT_GH_REPO:-}" ]]; then
    printf 'skip %s: AGENT_GH_REPO is not set\n' "$wt" >&2
    continue
  fi
  state=''
  if state=$(prune_pr_state_of "$pr"); then
    :
  else
    if ! gh_error_file=$(mktemp 2>&1); then
      printf 'skip %s: cannot capture gh stderr: %s\n' "$wt" "${gh_error_file//$'\n'/ }" >&2
      continue
    fi
    trap 'rm "$gh_error_file"' EXIT
    if ! state=$(gh pr view "$pr" --repo "$AGENT_GH_REPO" --json state -q .state 2>"$gh_error_file"); then
      gh_error=$(cat "$gh_error_file")
      printf 'skip %s: gh pr view %s failed: %s\n' "$wt" "$pr" "${gh_error//$'\n'/ }" >&2
      rm "$gh_error_file"
      trap - EXIT
      continue
    fi
    rm "$gh_error_file"
    trap - EXIT
  fi
  [[ "$state" == MERGED || "$state" == CLOSED ]] || continue
  if ! status=$(git -C "$wt" status --porcelain 2>&1); then
    printf 'skip %s: git status failed: %s\n' "$wt" "${status//$'\n'/ }" >&2
    continue
  fi
  [[ -z "$status" ]] || continue
  if ! result=$(orca worktree rm --worktree "path:$wt" 2>&1); then
    printf 'skip %s: orca worktree rm failed: %s\n' "$wt" "${result//$'\n'/ }" >&2
    continue
  fi
  printf 'pruned %s (PR %s %s)\n' "$wt" "$pr" "$state"
  prune_removed_count=$((prune_removed_count + 1))
done < <(orca worktree list --repo "$REPO" --json | jq -r '.result.worktrees[].path')
