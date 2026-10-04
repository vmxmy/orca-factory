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
