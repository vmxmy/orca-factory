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

orca worktree list --repo "$REPO" --json \
  | jq -r --arg p "$PREFIX" '.result.worktrees[] | select(.path | split("/") | last | startswith($p)) | "\(.createdAt) \(.path)"' \
  | while read -r created wt; do
      [[ "$wt" == "$self" ]] && continue
      [[ -f "$wt/.omc/agent-run-finished" ]] || (( created < cutoff_ms )) || continue
      orca worktree rm --worktree "path:$wt" --force --json | jq -r --arg wt "$wt" '"pruned \($wt) ok=\(.ok)"'
    done

# Only coordinator-marked units with a finished PR and a clean checkout are eligible.
main="$(cd "$AGENT_REPO_PATH" && pwd -P)"
orca worktree list --repo "$REPO" --json \
  | jq -r '.result.worktrees[].path' \
  | while IFS= read -r wt; do
      [[ "$wt" == "$self" || "$wt" == "$main" || "$wt" == "$AGENT_REPO_PATH" ]] && continue
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
