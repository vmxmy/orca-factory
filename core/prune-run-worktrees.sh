#!/usr/bin/env bash
# Remove worktrees left by finished automation runs. Each run gets a fresh worktree and a coordinator
# cannot delete its own, so every new run prunes its predecessors. A run is finished once its
# coordinator wrote .omc/agent-run-finished; runs older than MAX_AGE_HOURS are treated as crashed.
# Terminal liveness is no signal here: every Orca worktree keeps a shell open and an idle agent TUI stays up.
set -Eeuo pipefail

REPO="path:${AGENT_REPO_PATH:?main checkout path}"
PREFIX="${AGENT_RUN_WORKTREE_PREFIX:?set in agent-graph.env}"
MAX_AGE_HOURS="${AGENT_RUN_MAX_AGE_HOURS:-24}"
self="$(pwd -P)"
cutoff_ms=$(( ($(date +%s) - MAX_AGE_HOURS * 3600) * 1000 ))

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
prune_ticket_check() {
  local out
  if ! out=$(orca linear issue "$1" --json 2>/dev/null) ||
    ! jq -e --arg mode "${3:-completed}" '.ok == true and
      (.result.issue.state.type | type == "string") and
      (if $mode == "completed" then .result.issue.state.type == "completed"
       else .result.issue.state.type != "started" end)' <<<"$out" >/dev/null 2>&1; then
    prune_keep "$2" "ticket $1 is not confirmed ${3:-completed} (in flight or lookup failed)"
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
    any(.[]; .head.sha == $sha and ($branch == "" or .head.ref == $branch))' <<<"$prune_prs" >/dev/null
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
  prune_pr_ok=false
  prune_prs='[]'
  if [[ -n "${AGENT_GH_REPO:-}" ]] &&
    result=$(gh api --paginate "repos/$AGENT_GH_REPO/pulls?state=closed&per_page=100" 2>/dev/null) &&
    prune_prs=$(jq -se --arg repo "$AGENT_GH_REPO" '
      if all(.[]; type == "array") then add | map(select(.merged_at != null and .head.repo.full_name == $repo))
      else error("invalid PR pages") end' <<<"$result" 2>/dev/null); then
    prune_pr_ok=true
  else
    prune_keep compensation 'merged PR query failed; PR-dependent objects retained'
  fi
  # Orca workers need a completed linked issue and either a pushed head or a merged head.
  while IFS= read -r row; do
    wt=$(jq -r '.path' <<<"$row")
    ticket=$(jq -r '.linkedLinearIssue' <<<"$row")
    [[ "$wt" != "$self" && "$wt" != "$main" ]] || continue
    branch=$(git -C "$wt" symbolic-ref --quiet --short HEAD) || { prune_keep "$wt" 'branch unavailable'; continue; }
    prune_issue_safe "$wt" "$branch" || continue
    prune_ticket_check "$ticket" "$wt" || continue
    prune_clean "$wt" || continue
    sha=$(git -C "$wt" rev-parse --verify HEAD) || { prune_keep "$wt" 'head unavailable'; continue; }
    remote_sha=$(git -C "$AGENT_REPO_PATH" ls-remote --heads origin "refs/heads/$branch" 2>/dev/null) || remote_sha=''
    remote_sha=${remote_sha%%$'\t'*}
    if [[ "$sha" != "$remote_sha" ]] && ! prune_merged "$sha"; then
      prune_keep "$wt" 'head is neither confirmed pushed nor merged'; continue
    fi
    if ! result=$(orca worktree rm --worktree "path:$wt" --json 2>&1) ||
      ! jq -e '.ok == true' <<<"$result" >/dev/null 2>&1; then
      prune_keep "$wt" "Orca removal failed: $result"
    fi
  done < <(jq -c '.result.worktrees[] | select((.linkedLinearIssue // "") != "")' <<<"$prune_orca")
  # Git-only Claude worktrees; never hand Orca-managed paths to Git removal.
  while IFS= read -r row; do
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
    git -C "$AGENT_REPO_PATH" worktree remove "$wt" || prune_keep "$wt" 'Git removal failed'
  done < <(jq -c '.[]' <<<"$prune_worktrees")
  # Refresh occupancy after removals, including locked worktrees and the main checkout.
  if current=$(prune_git_worktrees) && result=$(git -C "$AGENT_REPO_PATH" for-each-ref --format='%(objectname) %(refname:short)' refs/heads/); then
    while read -r sha branch; do
      [[ -n "$branch" ]] || continue
      occupied=$(jq -r --arg branch "$branch" 'any(.[]; .branch == $branch)' <<<"$current")
      [[ "$occupied" == false ]] || continue
      prune_issue_safe '' "$branch" || continue
      prune_merged "$sha" || { prune_keep "$branch" 'head has no matching merged PR'; continue; }
      git -C "$AGENT_REPO_PATH" branch -D -- "$branch" || prune_keep "$branch" 'branch removal failed'
    done <<<"$result"
  else
    prune_keep 'local branches' 'cannot verify branch occupancy'
  fi
  if result=$(git -C "$AGENT_REPO_PATH" ls-remote --heads origin 'refs/heads/agent/*' 2>/dev/null); then
    while read -r sha branch; do
      [[ "$branch" =~ ^refs/heads/agent/([A-Z][A-Z0-9]*-[0-9]+)/[^/]+$ ]] || continue
      ticket=${BASH_REMATCH[1]}
      branch=${branch#refs/heads/}
      prune_issue_safe '' "$branch" || continue
      prune_ticket_check "$ticket" "$branch" || continue
      prune_merged "$sha" "$branch" || { prune_keep "$branch" 'remote head has no matching merged PR'; continue; }
      git -C "$AGENT_REPO_PATH" push origin --delete "$branch" || prune_keep "$branch" 'remote removal failed'
    done <<<"$result"
  else
    prune_keep 'remote branches' 'remote heads query failed'
  fi
}
main="$(cd "$AGENT_REPO_PATH" && pwd -P)"
prune_inventory_ok=false
prune_worktrees='[]'
prune_compensate

orca worktree list --repo "$REPO" --json \
  | jq -r --arg p "$PREFIX" '.result.worktrees[] | select(.path | split("/") | last | startswith($p)) | "\(.createdAt) \(.path)"' \
  | while read -r created wt; do
      [[ "$wt" == "$self" ]] && continue
      prune_legacy_safe "$wt" || continue
      [[ -f "$wt/.omc/agent-run-finished" ]] || (( created < cutoff_ms )) || continue
      orca worktree rm --worktree "path:$wt" --force --json | jq -r --arg wt "$wt" '"pruned \($wt) ok=\(.ok)"'
    done

# Only coordinator-marked units with a finished PR and a clean checkout are eligible.
main="$(cd "$AGENT_REPO_PATH" && pwd -P)"
orca worktree list --repo "$REPO" --json \
  | jq -r '.result.worktrees[].path' \
  | while IFS= read -r wt; do
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
    done
