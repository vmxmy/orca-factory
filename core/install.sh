#!/usr/bin/env bash
# Validate the agent graph setup for this checkout's repo: tools, the project profile and the issue backend.
# Scheduling belongs to `orca-factory-driver serve`; this script installs no Orca automation.
# Usage: scripts/agent-gates/install.sh [--dry-run]
# The script changes nothing, so --dry-run (still passed by onboard and ONBOARD 5b) behaves the same.
set -Eeuo pipefail
dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(dirname "$(git -C "$dir" rev-parse --path-format=absolute --git-common-dir)")"
fail() { echo "install: $*" >&2; exit 1; }

for t in orca orca-factory-driver gh git jq codex claude; do command -v "$t" >/dev/null || fail "missing tool: $t"; done
orca status --json | jq -e '.ok == true' >/dev/null || fail "Orca app is not running"

set -a; . "$dir/agent-graph.env"; set +a
case "${AGENT_ISSUE_BACKEND:-}" in
  github) ;;
  feishu)
    for v in AGENT_FEISHU_BASE AGENT_FEISHU_PROJECT; do
      [[ -n "${!v:-}" ]] || fail "agent-graph.env: $v is empty"
    done ;;
  *) fail 'AGENT_ISSUE_BACKEND must be github or feishu' ;;
esac
for v in AGENT_GH_REPO AGENT_MAX_IN_FLIGHT AGENT_RUN_WORKTREE_PREFIX AGENT_INSTALL_CMD AGENT_TIER_DEFAULT; do
  [[ -n "${!v:-}" ]] || fail "agent-graph.env: $v is empty"
done
# Tiers are agent:model:effort (exactly two colons; agent non-empty, model may be empty).
for v in AGENT_TIER_FAST AGENT_TIER_DEFAULT AGENT_TIER_STRONG; do
  [[ "${!v:-}" =~ ^[^:]+:[^:]*:[^:]*$ ]] || fail "agent-graph.env: $v='${!v:-}' must be agent:model:effort (two colons, agent non-empty)"
done
for f in COORDINATOR.md COORDINATOR.project.md gate.sh prune-run-worktrees.sh wait-runs.sh; do
  [[ -f "$dir/$f" ]] || fail "missing $f"
done
[[ -f "$dir/gate.project.sh" ]] || echo "install: note: no gate.project.sh, the gate runs only its generic checks"

orca-factory-driver issues validate --json | jq -e '.ok == true' >/dev/null || fail "cannot validate $AGENT_ISSUE_BACKEND issue backend"
echo "install: $repo profile valid (backend $AGENT_ISSUE_BACKEND); scheduling runs in orca-factory-driver serve"
