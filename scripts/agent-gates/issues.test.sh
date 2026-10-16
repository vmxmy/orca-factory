#!/usr/bin/env bash
# Offline backend-selection regression tests; no production config or network.
set -Eeuo pipefail
factory="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/repo/scripts/agent-gates"
cp "$factory"/core/*.sh "$tmp/repo/scripts/agent-gates/"
git init -q "$tmp/repo"
printf 'AGENT_GH_REPO=owner/repo\n' > "$tmp/repo/scripts/agent-gates/agent-graph.env"
export TEST_CALLS="$tmp/calls" AGENT_LINEAR_COUNT_FILE="$tmp/count"
cat > "$tmp/bin/orca" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TEST_CALLS"
exit 17
STUB
chmod +x "$tmp/bin/orca"
export PATH="$tmp/bin:$PATH"
entry="$tmp/repo/scripts/agent-gates/agent-graph.sh"
fail() { echo "issues.test: $*" >&2; exit 1; }
: > "$TEST_CALLS"
rc=0
AGENT_ISSUE_BACKEND=invalid bash "$entry" linear issue ZII-1 --json > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$rc" == 2 && ! -s "$TEST_CALLS" ]] || fail 'unknown backend must fail before external calls'
rc=0
( unset AGENT_ISSUE_BACKEND; bash "$entry" linear issue ZII-1 --json ) > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$rc" == 17 && "$(cat "$TEST_CALLS")" == 'linear issue ZII-1 --json' ]] || fail 'absent backend must preserve Linear argv and exit status'
grep -q '^AGENT_ISSUE_BACKEND=github' "$factory/templates/agent-graph.env.example" || fail 'new template must select github'
! grep -q '^AGENT_LINEAR_' "$factory/templates/agent-graph.env.example" || fail 'new GitHub template must not require Linear configuration'
: > "$TEST_CALLS"
rc=0
AGENT_ISSUE_BACKEND=feishu AGENT_FEISHU_BASE= bash "$entry" issues issue ZII-1 --json > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$rc" != 0 && ! -s "$TEST_CALLS" ]] && grep -q AGENT_FEISHU_BASE "$tmp/err" || fail 'feishu backend must require AGENT_FEISHU_BASE before external calls'
echo 'issues.test: backend selection PASS'
cat > "$tmp/bin/orca-factory-driver" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TEST_CALLS"
if [[ "${DRIVER_FAIL:-0}" != 0 ]]; then exit "$DRIVER_FAIL"; fi
case "$*" in
  'issues snapshot --json') cat "$ISSUES_FIXTURE" ;;
  'issues read --id ZII-1 --json') jq '{ok:true,result:{issue:.result.issues[0]}}' "$ISSUES_FIXTURE" ;;
  'issues validate --json') echo '{"ok":true}' ;;
  'ledger stats') echo runs=7 ;;
  'prune-worktrees --contract-version') echo prune-worktrees-v1 ;;
  'prune-worktrees') jq -e '.ok and .result.complete' "$ISSUES_FIXTURE" >/dev/null ;;
  *) echo '{"ok":true,"result":{}}' ;;
esac
STUB
chmod +x "$tmp/bin/orca-factory-driver"
: > "$TEST_CALLS"
DRIVER_FAIL=9 AGENT_ISSUE_BACKEND=feishu AGENT_FEISHU_BASE=BASE_TOKEN bash "$entry" issues issue ZII-1 --json > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$(cat "$TEST_CALLS")" == 'issues read --id ZII-1 --json' ]] || fail 'feishu backend must use the native driver CLI'
export ISSUES_FIXTURE="$tmp/issues.json" AGENT_ISSUE_BACKEND=github AGENT_DRIVER_BIN="$tmp/bin/orca-factory-driver"
cat > "$ISSUES_FIXTURE" <<'JSON'
{"ok":true,"result":{"complete":true,"issues":[{"id":"legacy-uuid","identifier":"ZII-1","number":7,"url":"https://github.com/owner/repo/issues/7","title":"held","description":"## SCOPE\ncore/a.sh","state":{"name":"Todo","type":"unstarted"},"labels":{"nodes":[{"name":"ready-for-agent"},{"name":"auto-failed"}],"pageInfo":{"hasNextPage":false}},"comments":{"nodes":[],"pageInfo":{"hasNextPage":false}},"relations":{"nodes":[],"pageInfo":{"hasNextPage":false}},"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false}},"blockedBy":[],"commentsComplete":true,"complete":true}]}}
JSON
: > "$TEST_CALLS"
bash "$entry" issues list-issues --json > "$tmp/out"
jq -e '.ok and .result.complete and .result.issues[0].identifier == "ZII-1" and .result.issues[0].labels[1].name == "auto-failed"' "$tmp/out" >/dev/null || fail 'normalized snapshot lost identity or hold'
bash "$entry" linear list-issues --json > "$tmp/out"
[[ "$(bash "$entry" ledger stats)" == runs=7 ]] || fail 'GitHub ledger did not delegate'
! grep -q '^linear ' "$TEST_CALLS" || fail 'GitHub mode called Linear'
rc=0
DRIVER_FAIL=23 bash "$entry" issues list-issues --json > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$rc" == 23 ]] || fail 'driver failure exit status lost'
cp "$ISSUES_FIXTURE" "$tmp/complete.json"
printf '{"ok":true,"result":{"issues":[]}\n' > "$ISSUES_FIXTURE"
rc=0
bash "$entry" issues list-issues --json > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$rc" != 0 ]] || fail 'incomplete snapshot was accepted'
echo 'issues.test: GitHub routing and completeness PASS'
cp "$tmp/complete.json" "$ISSUES_FIXTURE"
export AGENT_MAX_IN_FLIGHT=2 AGENT_STALE_HOURS=8 AGENT_SCOPE_AVOID=0
export AGENT_REPO_PATH="$tmp/repo" AGENT_RUN_WORKTREE_PREFIX=run-
export AGENT_PRECHECK_CACHE_DIR="$tmp/cache" AGENT_SNAPSHOT_FILE="$tmp/snapshot.json"
unset AGENT_LINEAR_TEAM AGENT_LINEAR_PROJECT AGENT_LABEL_READY AGENT_LABEL_CLAIMED AGENT_LABEL_FAILED
rc=0
bash "$entry" linear-precheck > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$rc" == 0 ]] && grep -q '^claimable= in_flight=0' "$tmp/out" || { cat "$tmp/out" "$tmp/err"; fail "GitHub auto-failed+ready must stay unclaimed without Linear variables (exit $rc)"; }
# A valid, unblocked fresh ticket is discoverable; dependencies are checked by read.
jq '.result.issues[0].labels.nodes = [{name:"ready-for-agent"}]' "$tmp/complete.json" > "$ISSUES_FIXTURE"
bash "$entry" linear-precheck > "$tmp/out" 2> "$tmp/err" || { cat "$tmp/err"; fail 'complete GitHub candidate unavailable'; }
grep -q '^claimable=ZII-1 ' "$tmp/out" || fail 'candidate identity changed'
jq '.result.complete=false' "$tmp/complete.json" > "$ISSUES_FIXTURE"
rc=0
bash "$entry" linear-precheck > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$rc" == 3 ]] || fail 'incomplete GitHub precheck must exit uncertain'
cp "$tmp/complete.json" "$ISSUES_FIXTURE"
cat > "$tmp/bin/orca" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TEST_CALLS"
case "$1" in
  linear) echo 'forbidden Linear call' >&2; exit 99 ;;
  --version) echo 1.0.0 ;;
  *) echo '{"ok":true,"result":{"worktrees":[],"automations":[]}}' ;;
esac
STUB
cat > "$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
printf 'gh %s\n' "$*" >> "$TEST_CALLS"
echo '[]'
STUB
chmod +x "$tmp/bin/gh"
for tool in codex claude; do ln -s gh "$tmp/bin/$tool"; done
cat >> "$tmp/repo/scripts/agent-gates/agent-graph.env" <<'PROFILE'
AGENT_ISSUE_BACKEND=github
AGENT_MAX_IN_FLIGHT=2
AGENT_RUN_WORKTREE_PREFIX=run-
AGENT_INSTALL_CMD=true
AGENT_TIER_FAST=codex:model:low
AGENT_TIER_DEFAULT=codex:model:medium
AGENT_TIER_STRONG=codex:model:high
PROFILE
cp "$factory/core/COORDINATOR.md" "$tmp/repo/scripts/agent-gates/"
touch "$tmp/repo/scripts/agent-gates/COORDINATOR.project.md"
bash "$tmp/repo/scripts/agent-gates/install.sh" --dry-run > "$tmp/out" 2> "$tmp/err" || { cat "$tmp/err"; fail 'GitHub installation requires Linear configuration'; }
bash "$entry" prune-run-worktrees > "$tmp/out" 2> "$tmp/err" || { cat "$tmp/err"; fail 'GitHub prune requires Linear configuration'; }
AGENT_ISSUE_BACKEND=github "$factory/bin/orca-factory" doctor --json > "$tmp/out"
jq -e '[.recommendation.missing[] | select(test("[Ll][Ii][Nn][Ee][Aa][Rr]"))] | length == 0' "$tmp/out" >/dev/null || fail 'GitHub doctor requires Linear'
! grep -q '^linear ' "$TEST_CALLS" || fail 'GitHub install/precheck/prune/doctor called Linear'
echo 'issues.test: GitHub precheck/install/prune/doctor PASS'
AGENT_ISSUE_BACKEND=feishu "$factory/bin/orca-factory" doctor --json > "$tmp/out" || fail 'Feishu doctor must accept the feishu backend'
jq -e '[.recommendation.missing[] | select(test("[Ll][Ii][Nn][Ee][Aa][Rr]"))] | length == 0' "$tmp/out" >/dev/null || fail 'Feishu doctor requires Linear'
echo 'issues.test: Feishu doctor PASS'

: > "$TEST_CALLS"
( unset AGENT_ISSUE_BACKEND; cd "$tmp/repo"; "$factory/bin/orca-factory" doctor --json ) > "$tmp/out"
! grep -q '^linear ' "$TEST_CALLS" || fail 'doctor ignored current project GitHub profile'
echo 'issues.test: profile doctor PASS'

# Snapshot intentionally omits comments; metadata/dependencies still must be complete.
jq '.result.issues[0] |= (.commentsComplete=false | .comments={nodes:[],pageInfo:{hasNextPage:true}})' "$tmp/complete.json" > "$ISSUES_FIXTURE"
bash "$entry" issues list-issues --json > "$tmp/out" || fail 'lean snapshot rejected intentional comment omission'
jq -e '.result.issues[0].commentsComplete == false' "$tmp/out" >/dev/null || fail 'lean comment completeness lost'
echo 'issues.test: lean snapshot PASS'
: > "$TEST_CALLS"
jq '.result.complete=false' "$tmp/complete.json" > "$ISSUES_FIXTURE"
rc=0
bash "$entry" prune-run-worktrees > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$rc" != 0 ]] || fail 'incomplete GitHub prune must fail before cleanup'
! grep -Eq '^(worktree rm|gh )' "$TEST_CALLS" || fail 'incomplete GitHub prune touched resources'
cp "$tmp/complete.json" "$ISSUES_FIXTURE"
bash "$entry" issues status set ZII-1 --to 'In Progress' --json >/dev/null
bash "$entry" issues label remove ZII-1 --label ready-for-agent --label auto-impl --json >/dev/null
bash "$entry" issues comment add ZII-1 --body '保留 correction 3/3' --json >/dev/null
grep -Fxq 'issues update --id ZII-1 --state In Progress' "$TEST_CALLS" || fail 'status not delegated'
grep -Fxq 'issues update --id ZII-1 --remove-label ready-for-agent --remove-label auto-impl' "$TEST_CALLS" || fail 'label args changed'
grep -Fxq 'issues comment --id ZII-1 --body-file -' "$TEST_CALLS" || fail 'comment not delegated via stdin'
echo 'issues.test: fail-closed prune and mutation contracts PASS'

jq '.result.issues[0].labels.nodes = [{name:"ready-for-agent"}]' "$tmp/complete.json" > "$ISSUES_FIXTURE"
: > "$TEST_CALLS"
AGENT_SCOPE_AVOID=1 bash "$entry" linear-precheck > "$tmp/out" 2> "$tmp/err" || { cat "$tmp/err"; fail 'scoped candidate unavailable'; }
[[ "$(grep -c '^issues snapshot' "$TEST_CALLS")" == 1 ]] || fail 'precheck enumerates snapshot more than once'
! grep -q '^issues read' "$TEST_CALLS" || fail 'precheck fetches full comments to read scope or blockers already in snapshot'
echo 'issues.test: one-snapshot precheck PASS'

# Feishu ledger backend (OF-51): install.sh --dry-run requires its own keys
# and validates the ledger is reachable through the same driver entry point
# agent-graph.sh issues validate uses, before linear/github did.
feishu_repo="$tmp/feishu-repo"
mkdir -p "$feishu_repo/scripts/agent-gates"
git init -q "$feishu_repo"
cp "$factory"/core/*.sh "$feishu_repo/scripts/agent-gates/"
cp "$factory/core/COORDINATOR.md" "$feishu_repo/scripts/agent-gates/"
touch "$feishu_repo/scripts/agent-gates/COORDINATOR.project.md"
cat > "$feishu_repo/scripts/agent-gates/agent-graph.env" <<'PROFILE'
AGENT_ISSUE_BACKEND=feishu
AGENT_GH_REPO=owner/repo
AGENT_FEISHU_BASE=BASE_TOKEN
AGENT_FEISHU_PROJECT=github.com/owner/repo
AGENT_MAX_IN_FLIGHT=2
AGENT_RUN_WORKTREE_PREFIX=run-
AGENT_INSTALL_CMD=true
AGENT_TIER_FAST=codex:model:low
AGENT_TIER_DEFAULT=codex:model:medium
AGENT_TIER_STRONG=codex:model:high
PROFILE
: > "$TEST_CALLS"
bash "$feishu_repo/scripts/agent-gates/install.sh" --dry-run > "$tmp/out" 2> "$tmp/err" \
  || { cat "$tmp/err"; fail 'Feishu installation must dry-run clean with ledger keys present'; }
grep -q 'dry run, nothing changed' "$tmp/out" || fail 'Feishu dry-run did not report no changes'
! grep -q '^linear ' "$TEST_CALLS" || fail 'Feishu install called Linear'

sed -i '/^AGENT_FEISHU_BASE=/d' "$feishu_repo/scripts/agent-gates/agent-graph.env"
rc=0
bash "$feishu_repo/scripts/agent-gates/install.sh" --dry-run > "$tmp/out" 2> "$tmp/err" || rc=$?
[[ "$rc" != 0 ]] && grep -q 'AGENT_FEISHU_BASE' "$tmp/err" || fail 'missing AGENT_FEISHU_BASE must fail install with the key name'
echo 'issues.test: Feishu install dry-run PASS'
