#!/bin/bash
# merge.sh <pr> <ticket> [worktree]: squash-merge one orca-factory PR after its
# checks report, confirm MERGED, clean the worktree and branch, mark the ticket Done.
# ORCA_FACTORY_REPO is the local orca-factory checkout.
set -e
PR=$1 TICKET=$2 WT=$3
REPO=vmxmy/orca-factory
cd "${ORCA_FACTORY_REPO:-$HOME/github/orca-factory}"
for i in $(seq 1 120); do
  n=$(gh pr checks "$PR" -R $REPO --json name -q 'length' 2>/dev/null || echo 0)
  [ "$n" -ge 8 ] && break
  sleep 15
done
gh pr checks "$PR" -R $REPO --watch --interval 20 >/dev/null
gh pr merge "$PR" -R $REPO --squash --delete-branch >/dev/null 2>&1 || true
state=$(gh pr view "$PR" -R $REPO --json state,mergeCommit -q '"\(.state) \(.mergeCommit.oid[:7])"')
echo "$state"
[ "${state%% *}" = MERGED ] || exit 1
if [ -n "$WT" ] && [ -d "$WT" ]; then
  b=$(git -C "$WT" rev-parse --abbrev-ref HEAD)
  git worktree remove --force "$WT"
  git branch -D "$b" >/dev/null 2>&1 || true
fi
git fetch -q origin
set -a; . ~/.config/orca-factory/driver.env; set +a
scripts/agent-gates/agent-graph.sh issues status set "$TICKET" --to Done 2>/dev/null | grep -o '"name":"Done"' | head -1
