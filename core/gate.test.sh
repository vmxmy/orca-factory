#!/usr/bin/env bash
set -Eeuo pipefail
GATE="$(cd "$(dirname "$0")" && pwd)/gate.sh"
GATE_LOG_DIR=".omc/gates"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
export TMPDIR="$TMP"
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
[[ -f "$R/$GATE_LOG_DIR/u1/gate.log" ]]
grep -q 'no gate.project.sh at origin/main, skipped' "$R/$GATE_LOG_DIR/u1/gate.log"

R="$(mkrepo nested-verdict)"
cat > "$R/src/check.sh" <<'CHECK'
printf '%s\n' 'UNIT      inner' 'VERDICT   green' 'STEP      all' 'REASON    inner passed' 'EVIDENCE  inner evidence' 'SCOPE     src/*' 'LANE      1'
exit 1
CHECK
out="$("$GATE" "$R" "$(spec 'UNIT: nested-verdict' 'SCOPE: src/*' 'CHECK: bash src/check.sh')")" && rc=0 || rc=$?
verdict_lines="$(printf '%s\n' "$out" | grep -cE '^(UNIT|VERDICT|STEP|REASON|EVIDENCE|SCOPE|LANE)[[:space:]]')"
[[ $rc == 1 && "$(field "$out" VERDICT)" == red && "$(field "$out" STEP)" == check && "$verdict_lines" -eq 7 ]] || { printf 'ASSERT: nested verdict lines must leave exactly seven outer fields, got rc=%s lines=%s\n%s\n' "$rc" "$verdict_lines" "$out"; exit 1; }
printf 'nested verdict lines: PASS\n'

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

R="$(mkrepo missing-helper)"
printf 'source src/helper.sh\nprintf "1\\n"\n' > "$R/src/probe.sh"
printf 'true\n' > "$R/src/helper.sh"
printf '{"version":1,"id":"unchanged-value","probe":["bash","-e","src/probe.sh"],"expected":1}\n' > "$R/src/assertion.json"
out="$("$GATE" "$R" "$(spec 'UNIT: missing-helper' 'SCOPE: src/*' 'CHECK: bash -e src/probe.sh' 'CHECK_IS_NEW_TEST: yes' 'CHECK_TEST_FILES: src/probe.sh src/assertion.json' 'CHECK_ASSERTION: src/assertion.json')" 2>&1)" && rc=0 || rc=$?
[[ $rc == 2 && -z "$(field "$out" VERDICT)" ]] || { printf 'ASSERT: missing baseline helper must be infrastructure failure, got rc=%s\n%s\n' "$rc" "$out"; exit 1; }

# CHECK_IS_NEW_TEST=yes and check passes on base -> red check-red-on-base
R="$(mkrepo newtest)"; echo 'printf "1\\n"' > "$R/src/t.test.sh"
printf '{"version":1,"id":"already-passes","probe":["bash","src/t.test.sh"],"expected":1}\n' > "$R/src/assertion.json"
out="$("$GATE" "$R" "$(spec 'UNIT: u4' 'SCOPE: src/*' 'CHECK: bash src/t.test.sh' 'CHECK_IS_NEW_TEST: yes' 'CHECK_TEST_FILES: src/t.test.sh src/assertion.json' 'CHECK_ASSERTION: src/assertion.json')")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == check-red-on-base ]]
[[ "$(field "$out" LANE)" == 1 ]]
[[ "$(git -C "$R" worktree list | wc -l)" -eq 1 ]]

assertion_repo() {
  R="$(mkrepo "$1")"
  echo 1 > "$R/src/value"
  git -C "$R" add src/value
  git -C "$R" -c commit.gpgsign=false commit -qm value
  git -C "$R" update-ref refs/remotes/origin/main HEAD
  echo 2 > "$R/src/value"
  cat > "$R/src/probe.sh" <<'PROBE'
printf '{"value":%s}\n' "$(cat src/value)"
PROBE
  printf '{"version":1,"id":"value-is-two","probe":["bash","-e","src/probe.sh"],"expected":{"value":2}}\n' > "$R/src/assertion.json"
}
assertion_spec() { spec "UNIT: $1" 'SCOPE: src/*' 'CHECK: true' 'CHECK_IS_NEW_TEST: yes' 'CHECK_TEST_FILES: src/probe.sh src/assertion.json' 'CHECK_ASSERTION: src/assertion.json'; }

assertion_repo comparator
out="$("$GATE" "$R" "$(assertion_spec comparator)")"
[[ "$(field "$out" VERDICT)" == green ]]
records="$(sed -n '/^{"version":1,/p' "$R/$GATE_LOG_DIR/comparator/gate.log")"
jq -e -s 'length == 2 and .[0].phase == "base" and .[0].outcome == "assertion_failed" and .[1].phase == "head" and .[1].outcome == "passed" and all(.[]; .assertions == 1 and .id == "value-is-two") and .[0].manifest_digest == .[1].manifest_digest' <<< "$records" > /dev/null
printf 'trusted comparator mismatch then equality: PASS\n'

for mutation in head-expected base-expected base-argv base-identity; do
  assertion_repo "freeze-private-$mutation"
  jq --arg mutation "$mutation" '.probe += [$mutation] | if ($mutation | endswith("expected")) then .expected = {value:3} else . end' "$R/src/assertion.json" > "$TMP/manifest"
  cp "$TMP/manifest" "$R/src/assertion.json"
  digest="$(git -C "$R" hash-object src/assertion.json)"
  cat > "$R/src/probe.sh" <<'PROBE'
python3 -I - "$1" <<'PYTHON'
import json
import os
import sys
from pathlib import Path

mutation = sys.argv[1]
value = int(Path("src/value").read_text())
if (mutation.startswith("base-") and value == 1) or (mutation.startswith("head-") and value == 2):
    path = next(Path(os.environ["TMPDIR"]).glob("agent-gate-assertion.*/files/src/assertion.json"))
    manifest = json.loads(path.read_bytes())
    if mutation.endswith("expected"):
        manifest["expected"] = {"value": 2}
    elif mutation.endswith("argv"):
        manifest["probe"] = ["false"]
    else:
        manifest["id"] = "changed"
    path.write_text(json.dumps(manifest))
print(json.dumps({"value": value}))
PYTHON
PROBE
  out="$("$GATE" "$R" "$(assertion_spec "freeze-private-$mutation")" 2>&1)" && rc=0 || rc=$?
  if [[ "$mutation" == *expected ]]; then
    [[ $rc == 1 && "$(field "$out" STEP)" == check-assertion ]] || { printf 'ASSERT: private %s changed frozen expectation, got rc=%s\n%s\n' "$mutation" "$rc" "$out"; exit 1; }
    outcome=assertion_failed
  else
    [[ $rc == 0 && "$(field "$out" VERDICT)" == green ]] || { printf 'ASSERT: private %s changed frozen probe, got rc=%s\n%s\n' "$mutation" "$rc" "$out"; exit 1; }
    outcome=passed
  fi
  records="$(sed -n '/^{"version":1,/p' "$R/$GATE_LOG_DIR/freeze-private-$mutation/gate.log")"
  jq -e -s --arg digest "$digest" --arg outcome "$outcome" 'length == 2 and .[0].outcome == "assertion_failed" and .[1].outcome == $outcome and all(.[]; .id == "value-is-two" and .manifest_digest == $digest and .assertions == 1)' <<< "$records" > /dev/null
  printf 'frozen private %s: PASS\n' "$mutation"
done

assertion_repo large-manifest
python3 -I - "$R/src/assertion.json" <<'PYTHON'
import sys
from pathlib import Path

path = Path(sys.argv[1])
path.write_bytes(b" " * 262144 + path.read_bytes())
PYTHON
digest="$(git -C "$R" hash-object src/assertion.json)"
out="$("$GATE" "$R" "$(assertion_spec large-manifest)")"
[[ "$(field "$out" VERDICT)" == green ]]
records="$(sed -n '/^{"version":1,/p' "$R/$GATE_LOG_DIR/large-manifest/gate.log")"
jq -e -s --arg digest "$digest" 'length == 2 and .[0].outcome == "assertion_failed" and .[1].outcome == "passed" and all(.[]; .manifest_digest == $digest and .assertions == 1)' <<< "$records" > /dev/null
printf 'large manifest with original digest: PASS\n'

for failure in nul-key nul-number nul-string nul-prefix nul-suffix invalid-utf8 utf-16-le utf-16-be; do
  assertion_repo "manifest-$failure"
  python3 -I - "$R/src/assertion.json" "$failure" <<'PYTHON'
import sys
from pathlib import Path

path = Path(sys.argv[1])
raw = path.read_bytes()
case = sys.argv[2]
if case == "nul-key":
    raw = raw.replace(b'"version"', b'"ver\x00sion"')
elif case == "nul-number":
    raw = raw.replace(b'"version":1', b'"version":\x001')
elif case == "nul-string":
    raw = raw.replace(b'value-is-two', b'value-is-\x00two')
elif case == "nul-prefix":
    raw = b"\x00" + raw
elif case == "nul-suffix":
    raw += b"\x00"
elif case == "invalid-utf8":
    raw = raw.replace(b'value-is-two', b'value-is-\xfftwo')
else:
    raw = raw.decode("utf-8").encode(case)
path.write_bytes(raw)
PYTHON
  out="$("$GATE" "$R" "$(assertion_spec "manifest-$failure")" 2>&1)" && rc=0 || rc=$?
  [[ $rc == 2 && "$out" == *'assertion infrastructure failure:'* && -z "$(field "$out" VERDICT)" ]] || { printf 'ASSERT: malformed manifest %s must be infrastructure failure, got rc=%s\n%s\n' "$failure" "$rc" "$out"; exit 1; }
  printf 'reject manifest %s: PASS\n' "$failure"
done

assertion_repo escaped-nul-expected
cat > "$R/src/probe.sh" <<'PROBE'
printf '{"value":"%s\\u0000é"}\n' "$(cat src/value)"
PROBE
jq '.expected = {"value":"2\u0000é"}' "$R/src/assertion.json" > "$TMP/manifest"
cp "$TMP/manifest" "$R/src/assertion.json"
digest="$(git hash-object "$R/src/assertion.json")"
out="$("$GATE" "$R" "$(assertion_spec escaped-nul-expected)")"
[[ "$(field "$out" VERDICT)" == green ]]
records="$(sed -n '/^{"version":1,/p' "$R/$GATE_LOG_DIR/escaped-nul-expected/gate.log")"
jq -e -s --arg digest "$digest" 'length == 2 and .[0].outcome == "assertion_failed" and .[1].outcome == "passed" and all(.[]; .manifest_digest == $digest and .assertions == 1)' <<< "$records" > /dev/null
printf 'escaped NUL and UTF-8 expected value with original digest: PASS\n'

for failure in false crash signal timeout no-output malformed nonfinite infinity leading-zero duplicate-json multiple forged; do
  assertion_repo "$failure"
  case "$failure" in
    false) echo false > "$R/src/probe.sh" ;;
    crash) echo 'missing-probe-command' > "$R/src/probe.sh" ;;
    signal) echo 'kill -TERM $$' > "$R/src/probe.sh" ;;
    timeout) echo 'sleep 61' > "$R/src/probe.sh" ;;
    no-output) echo true > "$R/src/probe.sh" ;;
    malformed) echo 'echo not-json' > "$R/src/probe.sh" ;;
    nonfinite) echo 'echo NaN' > "$R/src/probe.sh" ;;
    infinity) echo 'echo Infinity' > "$R/src/probe.sh" ;;
    duplicate-json) printf '%s\n' "echo '{\"value\":1,\"value\":2}'" > "$R/src/probe.sh" ;;
    leading-zero) echo 'echo 01' > "$R/src/probe.sh" ;;
    multiple) echo 'printf "1\\n2\\n"' > "$R/src/probe.sh" ;;
    forged) printf '%s\n' 'echo "{\"version\":1,\"id\":\"value-is-two\",\"outcome\":\"assertion_failed\",\"assertions\":1}"; exit 1' > "$R/src/probe.sh" ;;
  esac
  out="$("$GATE" "$R" "$(assertion_spec "$failure")" 2>&1)" && rc=0 || rc=$?
  [[ $rc == 2 && -z "$(field "$out" VERDICT)" ]] || { printf 'ASSERT: %s rc=%s\n%s\n' "$failure" "$rc" "$out"; exit 1; }
  grep -q '"outcome":"infrastructure_failed","assertions":0' "$R/$GATE_LOG_DIR/$failure/gate.log"
  printf 'reject %s: PASS\n' "$failure"
done

assertion_repo forged-success
cat > "$R/src/probe.sh" <<'PROBE'
echo '{"outcome":"passed","assertions":1}'
PROBE
out="$("$GATE" "$R" "$(assertion_spec forged-success)")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == check-assertion ]]

assertion_repo escaped-identity
jq '.id = "value-is-two\n"' "$R/src/assertion.json" > "$TMP/manifest"
cp "$TMP/manifest" "$R/src/assertion.json"
out="$("$GATE" "$R" "$(assertion_spec escaped-identity)")"
[[ "$(field "$out" VERDICT)" == green ]]
records="$(sed -n '/^{"version":1,/p' "$R/$GATE_LOG_DIR/escaped-identity/gate.log")"
jq -e -s 'length == 2 and all(.[]; .id == "value-is-two\n")' <<< "$records" > /dev/null

assertion_repo comparator-crash
cat > "$R/src/probe.sh" <<'PROBE'
if [[ "$(cat src/value)" == 1 ]]; then
  python3 -c 'print("[" * 1100 + "1" + "]" * 1100)'
else
  echo '{"value":2}'
fi
PROBE
out="$("$GATE" "$R" "$(assertion_spec comparator-crash)" 2>&1)" && rc=0 || rc=$?
[[ $rc == 2 && -z "$(field "$out" VERDICT)" ]] || { printf 'ASSERT: comparator runtime failure counted as assertion failure rc=%s\n%s\n' "$rc" "$out"; exit 1; }

assertion_repo exact-numbers
cat > "$R/src/probe.sh" <<'PROBE'
printf '18446744073709551616\n'
PROBE
printf '{"version":1,"id":"exact-number","probe":["bash","src/probe.sh"],"expected":18446744073709551617}\n' > "$R/src/assertion.json"
out="$("$GATE" "$R" "$(assertion_spec exact-numbers)")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == check-assertion ]] || { printf 'ASSERT: distinct integers compared equal rc=%s\n%s\n' "$rc" "$out"; exit 1; }

for equality in boolean decimal; do
  assertion_repo "$equality"
  case "$equality" in
    boolean) actual=1; expected=true ;;
    decimal) actual=0.123456789012345678901; expected=0.123456789012345678902 ;;
  esac
  printf 'echo %s\n' "$actual" > "$R/src/probe.sh"
  printf '{"version":1,"id":"typed-value","probe":["bash","src/probe.sh"],"expected":%s}\n' "$expected" > "$R/src/assertion.json"
  out="$("$GATE" "$R" "$(assertion_spec "$equality")")" && rc=0 || rc=$?
  [[ $rc == 1 && "$(field "$out" STEP)" == check-assertion ]] || { printf 'ASSERT: %s values compared equal rc=%s\n%s\n' "$equality" "$rc" "$out"; exit 1; }
done

assertion_repo missing-python
mkdir "$TMP/no-python"
for tool in git sed head sort grep mkdir date jq timeout cat; do ln -s "$(command -v "$tool")" "$TMP/no-python/$tool"; done
out="$(PATH="$TMP/no-python" /bin/bash "$GATE" "$R" "$(assertion_spec missing-python)" 2>&1)" && rc=0 || rc=$?
[[ $rc == 2 && "$out" == *'Python3 are required'* && -z "$(field "$out" VERDICT)" ]]

assertion_repo head-no-output
cat > "$R/src/probe.sh" <<'PROBE'
if [[ "$(cat src/value)" == 1 ]]; then echo '{"value":1}'; fi
PROBE
out="$("$GATE" "$R" "$(assertion_spec head-no-output)" 2>&1)" && rc=0 || rc=$?
[[ $rc == 2 && -z "$(field "$out" VERDICT)" ]]
grep -q '"phase":"head","outcome":"infrastructure_failed","assertions":0' "$R/$GATE_LOG_DIR/head-no-output/gate.log"

assertion_repo final-check
S="$(assertion_spec final-check)"
sed '/^CHECK:/c\CHECK: printf '\''{"outcome":"passed","assertions":1}\\n'\''; false' "$S" > "$TMP/spec-other"
out="$("$GATE" "$R" "$TMP/spec-other")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == check ]]

assertion_repo freeze
cat > "$R/src/probe.sh" <<'PROBE'
printf '{"version":1,"id":"changed","probe":["false"],"expected":1}\n' > src/assertion.json
printf '{"value":%s}\n' "$(cat src/value)"
PROBE
out="$("$GATE" "$R" "$(assertion_spec freeze)")"
[[ "$(field "$out" VERDICT)" == green ]]
records="$(sed -n '/^{"version":1,/p' "$R/$GATE_LOG_DIR/freeze/gate.log")"
jq -e -s 'length == 2 and all(.[]; .id == "value-is-two" and .assertions == 1) and .[0].manifest_digest == .[1].manifest_digest' <<< "$records" > /dev/null

assertion_repo head-mismatch
echo 3 > "$R/src/value"
out="$("$GATE" "$R" "$(assertion_spec head-mismatch)")" && rc=0 || rc=$?
[[ $rc == 1 && "$(field "$out" STEP)" == check-assertion ]]

for failure in version zero-assertion invalid-manifest duplicate-manifest absent-manifest duplicate traversal symlink baseline-symlink; do
  assertion_repo "$failure"
  S="$(assertion_spec "$failure")"
  case "$failure" in
    version) jq '.version = 2' "$R/src/assertion.json" > "$TMP/manifest"; cp "$TMP/manifest" "$R/src/assertion.json" ;;
    zero-assertion) echo '{}' > "$R/src/assertion.json" ;;
    invalid-manifest) echo '{' > "$R/src/assertion.json" ;;
    duplicate-manifest) echo '{"version":1,"id":"duplicate","probe":["true"],"expected":1,"expected":2}' > "$R/src/assertion.json" ;;
    absent-manifest) sed '/^CHECK_TEST_FILES:/c\CHECK_TEST_FILES: src/probe.sh' "$S" > "$TMP/spec-other"; S="$TMP/spec-other" ;;
    duplicate) sed '/^CHECK_TEST_FILES:/c\CHECK_TEST_FILES: src/probe.sh src/assertion.json src/probe.sh' "$S" > "$TMP/spec-other"; S="$TMP/spec-other" ;;
    traversal) sed '/^CHECK_TEST_FILES:/c\CHECK_TEST_FILES: src/probe.sh src/assertion.json ../outside' "$S" > "$TMP/spec-other"; S="$TMP/spec-other" ;;
    symlink) mv "$R/src/probe.sh" "$TMP/probe"; ln -s "$TMP/probe" "$R/src/probe.sh" ;;
    baseline-symlink)
      mv "$R/src/probe.sh" "$TMP/probe"
      ln -s "$TMP/probe" "$R/src/probe.sh"
      git -C "$R" add src/probe.sh
      git -C "$R" -c commit.gpgsign=false commit -qm symlink
      git -C "$R" update-ref refs/remotes/origin/main HEAD
      rm "$R/src/probe.sh"; cp "$TMP/probe" "$R/src/probe.sh" ;;
  esac
  out="$("$GATE" "$R" "$S" 2>&1)" && rc=0 || rc=$?
  [[ $rc == 2 && -z "$(field "$out" VERDICT)" ]] || { printf 'ASSERT: %s rc=%s\n%s\n' "$failure" "$rc" "$out"; exit 1; }
done

R="$(mkrepo legacy)"; echo changed > "$R/src/value"
out="$("$GATE" "$R" "$(spec 'UNIT: legacy-yes' 'SCOPE: src/*' 'CHECK: false' 'CHECK_IS_NEW_TEST: yes' 'CHECK_TEST_FILES: src/value')" 2>&1)" && rc=0 || rc=$?
[[ $rc == 2 && "$out" == *'requires CHECK_ASSERTION'* && "$out" == *'fresh reviewed handoff'* ]]
out="$("$GATE" "$R" "$(spec 'UNIT: legacy-no' 'SCOPE: src/*' 'CHECK: true' 'CHECK_IS_NEW_TEST: no')")"
[[ "$(field "$out" VERDICT)" == green ]]
printf 'legacy yes migration diagnostic and legacy no compatibility: PASS\n'

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

# Timing out on a held heavy lock is infrastructure (exit 2, no VERDICT), not a red verdict on the diff
rm -f "$TMP/started"
printf '#!/usr/bin/env bash\ntouch "%s/started"\n' "$TMP" > "$R/scripts/agent-gates/gate.project.sh"
flock -o "$TMP/agent-gate-heavy.flock" sleep 30 & holder=$!
sleep 0.5
out="$(TMPDIR="$TMP" GATE_PROJECT_SRC=worktree GATE_LOCK_WAIT=1 timeout 20 "$GATE" "$R" "$S" 2>/dev/null)" && rc=0 || rc=$?
kill "$holder"; wait "$holder" 2>/dev/null || true
[[ $rc == 2 && -z "$(field "$out" VERDICT)" && ! -e "$TMP/started" ]] || { echo "ASSERT: heavy lock timeout rc=$rc VERDICT=$(field "$out" VERDICT)"; exit 1; }

printf 'agent gate regression: PASS\n'
