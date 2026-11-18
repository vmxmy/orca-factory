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
  if ! pr_head=$(gh pr view "$PR" -R $REPO --json headRefOid -q '.headRefOid'); then
    echo "保留 worktree $WT：无法读取 PR head"
  elif ! git -C "$WT" merge-base --is-ancestor HEAD "$pr_head"; then
    echo "保留 worktree $WT：HEAD 不属于该 PR 或无法确认提交关系"
  elif ! changes=$(git -C "$WT" status --porcelain --untracked-files=all --ignore-submodules=none); then
    echo "保留 worktree $WT：无法读取工作区状态"
  elif [ -n "$changes" ]; then
    echo "保留 worktree $WT：有未提交改动"
  else
    b=$(git -C "$WT" rev-parse --abbrev-ref HEAD)
    git worktree remove "$WT"
    git branch -D "$b" >/dev/null 2>&1 || true
  fi
fi
git fetch -q origin
set -a; . ~/.config/orca-factory/driver.env; set +a
done_result=$(scripts/agent-gates/agent-graph.sh issues status set "$TICKET" --to Done)
printf '%s\n' "$done_result" | grep -m 1 -o '"name":"Done"'
