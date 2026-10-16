#!/usr/bin/env bash
# Entry point inside a checkout: load the project profile, then run a core script.
# Usage: scripts/agent-gates/agent-graph.sh <prune-run-worktrees|gate|ledger|issues> [args...]
set -Eeuo pipefail
# macOS ships bash 3.2: with set -u an empty array expands as unbound, hence the ${names[@]+...} guards below.
dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
names=(${!AGENT_@}); for v in ${names[@]+"${names[@]}"}; do printf -v "saved_$v" '%s' "${!v}"; done   # caller-set AGENT_* win over the profile
set -a; . "$dir/agent-graph.env"; set +a
for v in ${names[@]+"${names[@]}"}; do sv="saved_$v"; printf -v "$v" '%s' "${!sv}"; export "$v"; done
export AGENT_REPO_PATH="${AGENT_REPO_PATH:-$(dirname "$(git -C "$dir" rev-parse --path-format=absolute --git-common-dir)")}"
case "${AGENT_ISSUE_BACKEND:-}" in
  github|feishu) ;;
  *) echo "agent-graph: AGENT_ISSUE_BACKEND must be github or feishu" >&2; exit 2 ;;
esac
if [[ "${1:-}" == issues ]]; then
  exec bash "$dir/issues.sh" "${@:2}"
fi
exec bash "$dir/${1:?script name}.sh" "${@:2}"
