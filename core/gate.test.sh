#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GATE="$ROOT/scripts/agent-gates/gate.sh"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
export GATE_BASE=origin/main
unset GATE_PROJECT_SRC AGENT_LANE2_GLOBS AGENT_LANE3_GLOBS   # temp repos have no gate.project.sh at origin/main -> project step skipped
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t

mkrepo() {
  local d="$TMP/$1"
  git init -q "$d"
  mkdir -p "$d/src"
  echo 'base' > "$d/src/a.js"
  echo '.omc/' > "$d/.gitignore"   # like the real repo: gate logs and the materialized project gate stay out of the diff
  git -C "$d" add -A
  git -C "$d" -c commit.gpgsign=false commit -qm init
  git -C "$d" update-ref refs/remotes/origin/main HEAD
  echo "$d"
}
spec() { printf '%s\n' "$@" > "$TMP/spec.txt"; echo "$TMP/spec.txt"; }
field() { printf '%s\n' "$1" | awk -v k="$2" '$1==k {sub(/^[A-Z]+ +/, ""); print; exit}'; }

# In-scope change + CHECK=true -> green
R="$(mkrepo green)"; echo 'changed' > "$R/src/a.js"
out="$("$GATE" "$R" "$(spec 'UNIT: u1' 'SCOPE: src/*' 'CHECK: true')")"
[[ "$(field "$out" VERDICT)" == green && "$(field "$out" STEP)" == all ]]
[[ "$(field "$out" LANE)" == 1 ]]
[[ -f "$R/.omc/gates/u1/gate.log" ]]
grep -q 'no gate.project.sh at origin/main, skipped' "$R/.omc/gates/u1/gate.log"

# File outside SCOPE -> red scope
R="$(mkrepo scope)"; echo 'x' > "$R/other.txt"
out="$("$GATE" "$R" "$(spec 'UNIT: u2' 'SCOPE: src/*' 'CHECK: true')")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" VERDICT)" == red && "$(field "$out" STEP)" == scope ]]
grep -q 'other.txt' <<< "$out"
[[ "$(field "$out" LANE)" == 1 ]]

# Added line with @ts-ignore -> red suppression
R="$(mkrepo supp)"; printf '// @ts-ignore\nx\n' >> "$R/src/a.js"
out="$("$GATE" "$R" "$(spec 'UNIT: u3' 'SCOPE: src/*' 'CHECK: true')")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == suppression ]]
[[ "$(field "$out" LANE)" == 1 ]]

# CHECK_IS_NEW_TEST=yes and check passes on base -> red check-red-on-base
R="$(mkrepo newtest)"; echo 'exit 0' > "$R/src/t.test.sh"
out="$("$GATE" "$R" "$(spec 'UNIT: u4' 'SCOPE: src/*' 'CHECK: bash src/t.test.sh' 'CHECK_IS_NEW_TEST: yes' 'CHECK_TEST_FILES: src/t.test.sh')")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == check-red-on-base ]]
[[ "$(field "$out" LANE)" == 1 ]]
[[ "$(git -C "$R" worktree list | wc -l)" -eq 1 ]]

# CHECK=false -> red check
R="$(mkrepo check)"; echo 'changed' > "$R/src/a.js"
out="$("$GATE" "$R" "$(spec 'UNIT: u5' 'SCOPE: src/*' 'CHECK: false')")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == check ]]
[[ "$(field "$out" LANE)" == 1 ]]

# Spec missing UNIT -> exit 2
"$GATE" "$R" "$(spec 'SCOPE: src/*' 'CHECK: true')" >/dev/null 2>&1 && rc=0 || rc=$?
[[ $rc == 2 ]]

# AGENT_LANE2_GLOBS hit -> green, LANE 2
R="$(mkrepo lane2)"; mkdir -p "$R/src/lane2"; echo 'x' > "$R/src/lane2/x.js"
out="$(AGENT_LANE2_GLOBS='src/lane2/*' "$GATE" "$R" "$(spec 'UNIT: u6' 'SCOPE: src/*' 'CHECK: true')")"
[[ "$(field "$out" VERDICT)" == green ]]
grep -q '^LANE      2$' <<< "$out"

# Unset lane globs load from the base profile, including trailing comments.
R="$(mkrepo laneenv)"; mkdir -p "$R/scripts/agent-gates" "$R/x"
cat > "$R/scripts/agent-gates/agent-graph.env" <<'ENV'
AGENT_LANE2_GLOBS='x/*' # project lane 2
AGENT_LANE3_GLOBS='private/*' # project lane 3
AGENT_GH_REPO='profile-only'
LANE_GLOBS_HARD='x/*' # must not leak into the gate
ENV
echo 'base' > "$R/x/a"
git -C "$R" add -A && git -C "$R" -c commit.gpgsign=false commit -qm profile && git -C "$R" update-ref refs/remotes/origin/main HEAD
echo 'changed' > "$R/x/a"
out="$("$GATE" "$R" "$(spec 'UNIT: env-ref' 'SCOPE: x/*' 'CHECK: true')")"
[[ "$(field "$out" VERDICT)" == green ]]
grep -q '^LANE      2$' <<< "$out"

# Explicit empty caller values win, while the other unset glob still loads.
out="$(AGENT_LANE2_GLOBS='' "$GATE" "$R" "$(spec 'UNIT: env-empty' 'SCOPE: x/*' 'CHECK: true')")"
grep -q '^LANE      1$' <<< "$out"
out="$(AGENT_LANE3_GLOBS='x/*' "$GATE" "$R" "$(spec 'UNIT: env-override' 'SCOPE: x/*' 'CHECK: true')")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == lane ]]
grep -q '^LANE      3$' <<< "$out"

# Default source ignores edits to the profile; worktree mode uses them.
printf "AGENT_LANE2_GLOBS='elsewhere/*' # changed\nAGENT_LANE3_GLOBS='x/*' # protected\n" > "$R/scripts/agent-gates/agent-graph.env"
out="$("$GATE" "$R" "$(spec 'UNIT: env-source' 'SCOPE: x/* scripts/*' 'CHECK: true')")"
grep -q '^LANE      2$' <<< "$out"
out="$(GATE_PROJECT_SRC=worktree "$GATE" "$R" "$(spec 'UNIT: env-worktree' 'SCOPE: x/* scripts/*' 'CHECK: true')")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == lane ]]
grep -q '^LANE      3$' <<< "$out"

# AGENT_LANE3_GLOBS hit -> red lane, LANE 3
R="$(mkrepo lane3)"; mkdir -p "$R/src/secret"; echo 'k' > "$R/src/secret/k"
out="$(AGENT_LANE3_GLOBS='src/secret/*' "$GATE" "$R" "$(spec 'UNIT: u7' 'SCOPE: src/*' 'CHECK: true')")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" VERDICT)" == red && "$(field "$out" STEP)" == lane ]]
grep -q 'src/secret/k' <<< "$out"
[[ "$(field "$out" LANE)" == 3 ]]

# scripts/agent-gates/* is lane 2 even with no env globs (hard-coded)
R="$(mkrepo hard)"; mkdir -p "$R/scripts/agent-gates"; echo 'x' > "$R/scripts/agent-gates/anything"
out="$("$GATE" "$R" "$(spec 'UNIT: u8' 'SCOPE: scripts/*' 'CHECK: true')")"
[[ "$(field "$out" VERDICT)" == green && "$(field "$out" LANE)" == 2 ]]

# GATE_PROJECT_SRC=origin/main reads the project gate from the ref, not the worktree copy
R="$(mkrepo projsrc)"; mkdir -p "$R/scripts/agent-gates"
printf '#!/usr/bin/env bash\necho "== ref"; exit 1\n' > "$R/scripts/agent-gates/gate.project.sh"
git -C "$R" add -A && git -C "$R" -c commit.gpgsign=false commit -qm gate && git -C "$R" update-ref refs/remotes/origin/main HEAD
printf '#!/usr/bin/env bash\nexit 0\n' > "$R/scripts/agent-gates/gate.project.sh"   # worker weakens its copy
echo 'changed' > "$R/src/a.js"
out="$("$GATE" "$R" "$(spec 'UNIT: u9' 'SCOPE: src/* scripts/*' 'CHECK: true')")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == ref ]]
out="$(GATE_PROJECT_SRC=worktree "$GATE" "$R" "$(spec 'UNIT: u10' 'SCOPE: src/* scripts/*' 'CHECK: true')")"
[[ "$(field "$out" VERDICT)" == green && "$(field "$out" LANE)" == 2 ]]

# A project gate killed by SIGKILL (driver timeout kills the process group) must not leave the heavy lock behind
R="$(mkrepo lockkill)"; mkdir -p "$R/scripts/agent-gates"
printf '#!/usr/bin/env bash\ntouch "%s/started"; sleep 300\n' "$TMP" > "$R/scripts/agent-gates/gate.project.sh"
echo 'changed' > "$R/src/a.js"
S="$(spec 'UNIT: u11' 'SCOPE: src/* scripts/*' 'CHECK: true')"
TMPDIR="$TMP" GATE_PROJECT_SRC=worktree setsid "$GATE" "$R" "$S" >/dev/null 2>&1 & pid=$!
for _ in $(seq 300); do [[ -e "$TMP/started" ]] && break; sleep 0.1; done
[[ -e "$TMP/started" ]]
kill -KILL -- "-$pid"; wait "$pid" 2>/dev/null || true
printf '#!/usr/bin/env bash\nexit 0\n' > "$R/scripts/agent-gates/gate.project.sh"
out="$(TMPDIR="$TMP" GATE_PROJECT_SRC=worktree timeout 60 "$GATE" "$R" "$S")" || { echo "ASSERT: gate after SIGKILLed holder did not finish within 60s (stale heavy lock)"; exit 1; }
[[ "$(field "$out" VERDICT)" == green ]]

printf 'agent gate regression: PASS\n'
