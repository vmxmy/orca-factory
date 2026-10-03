#!/usr/bin/env bash
# Entry point inside a checkout: load the project profile, then run a core script.
# Usage: scripts/agent-gates/agent-graph.sh <linear-precheck|prune-run-worktrees|gate|ledger> [args...]
set -Eeuo pipefail
dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
names=(${!AGENT_@}); for v in "${names[@]}"; do printf -v "saved_$v" '%s' "${!v}"; done   # caller-set AGENT_* win over the profile
set -a; . "$dir/agent-graph.env"; set +a
for v in "${names[@]}"; do sv="saved_$v"; printf -v "$v" '%s' "${!sv}"; export "$v"; done
export AGENT_REPO_PATH="${AGENT_REPO_PATH:-$(dirname "$(git -C "$dir" rev-parse --path-format=absolute --git-common-dir)")}"
exec bash "$dir/${1:?script name}.sh" "${@:2}"
