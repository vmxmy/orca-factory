#!/usr/bin/env bash
set -Eeuo pipefail
set -f   # SCOPE / lane globs are matched with [[ == ]], never pathname-expanded against the cwd

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <unit-worktree> <spec-file>" >&2
  exit 2
fi
WT="$(cd "$1" && pwd)"
SPEC="$2"
BASE="${GATE_BASE:-origin/main}"
PROJECT_SRC="${GATE_PROJECT_SRC:-origin/main}"   # ref to read gate.project.sh from; 'worktree' = $WT copy
LOCK_WAIT="${GATE_LOCK_WAIT:-1800}"               # seconds to wait for the heavy lock
LANE_GLOBS_HARD='scripts/agent-gates/*'           # always lane 2, regardless of env
# Read profile defaults in a subshell so unrelated assignments stay isolated.
# As with gate.project.sh, only explicit worktree mode reads the checkout.
lane_profile_glob() (
  local profile
  if [[ "$PROJECT_SRC" == worktree ]]; then
    [[ -f "$WT/scripts/agent-gates/agent-graph.env" ]] || return 0
    profile="$(cat "$WT/scripts/agent-gates/agent-graph.env")"
  else
    profile="$(git -C "$WT" show "$PROJECT_SRC:scripts/agent-gates/agent-graph.env" 2>/dev/null)" || return 0
  fi
  source /dev/stdin <<< "$profile" > /dev/null
  case "$1" in
    2) printf '%s' "${AGENT_LANE2_GLOBS:-}" ;;
    3) printf '%s' "${AGENT_LANE3_GLOBS:-}" ;;
  esac
)
LANE2_GLOBS="${AGENT_LANE2_GLOBS:-}"
LANE3_GLOBS="${AGENT_LANE3_GLOBS:-}"
# An explicitly set empty value also takes precedence over the profile.
[[ ${AGENT_LANE2_GLOBS+x} ]] || LANE2_GLOBS="$(lane_profile_glob 2)"
[[ ${AGENT_LANE3_GLOBS+x} ]] || LANE3_GLOBS="$(lane_profile_glob 3)"
LANE=1
[[ -f "$SPEC" ]] || { echo "spec not found: $SPEC" >&2; exit 2; }

spec_get() { sed -n "s/^$1:[[:space:]]*//p" "$SPEC" | head -n 1; }
UNIT="$(spec_get UNIT)"
SCOPE="$(spec_get SCOPE)"
CHECK="$(spec_get CHECK)"
CHECK_IS_NEW_TEST="$(spec_get CHECK_IS_NEW_TEST)"
CHECK_TEST_FILES="$(spec_get CHECK_TEST_FILES)"
CHECK_ASSERTION="$(spec_get CHECK_ASSERTION)"
CHECK_IS_NEW_TEST="${CHECK_IS_NEW_TEST:-no}"
for k in UNIT SCOPE CHECK; do
  [[ -n "${!k}" ]] || { echo "spec missing $k" >&2; exit 2; }
done
if [[ "$CHECK_IS_NEW_TEST" == yes && -z "$CHECK_TEST_FILES" ]]; then
  echo "spec missing CHECK_TEST_FILES" >&2; exit 2
fi
if [[ "$CHECK_IS_NEW_TEST" == yes && -z "$CHECK_ASSERTION" ]]; then
  echo "new-test evidence requires CHECK_ASSERTION: add a version 1 JSON assertion manifest to CHECK_TEST_FILES; refresh pre-handoff gate specs, or request a fresh reviewed handoff for frozen work" >&2
  exit 2
fi

LOG_DIR="$WT/.omc/gates/$UNIT"
mkdir -p "$LOG_DIR"
LOG="$LOG_DIR/gate.log"
TMP_BASE=""
TMP_ASSERTION=""
# flock is released with its descriptor, so a SIGKILLed gate cannot leave it held.
LOCK="${TMPDIR:-/tmp}/agent-gate-heavy.flock"
cleanup() {
  if [[ -n "$TMP_ASSERTION" ]]; then rm -rf "$TMP_ASSERTION"; fi
  if [[ -n "$TMP_BASE" ]]; then git -C "$WT" worktree remove --force "$TMP_BASE" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

log() { printf '%s\n' "$@" >> "$LOG"; }
log "=== gate $UNIT $(date -u +%FT%TZ) base=$BASE project_src=$PROJECT_SRC ==="

verdict() {
  local v="$1" step="$2" reason="$3" evidence="$4"
  printf 'UNIT      %s\nVERDICT   %s\nSTEP      %s\nREASON    %s\n' "$UNIT" "$v" "$step" "$reason"
  printf 'EVIDENCE  %s\n' "$(printf '%s' "$evidence" | head -n 20 | sed '2,$s/^/| /')"
  printf 'SCOPE     %s\n' "$SCOPE"
  printf 'LANE      %s\n' "$LANE"
  log "VERDICT $v STEP $step REASON $reason LANE $LANE"
  if [[ "$v" == green ]]; then exit 0; fi
  exit 1
}

# run <step> <dir> <cmd>: logs output, returns command status, output in $OUT
run() {
  local step="$1" dir="$2" cmd="$3" rc=0
  log "--- [$step] $(date -u +%FT%TZ) (cd $dir && $cmd)"
  OUT="$( (cd "$dir" && bash -c "$cmd") 2>&1 )" || rc=$?
  log "$OUT" "--- [$step] exit $rc $(date -u +%FT%TZ)"
  return $rc
}
tail20() { printf '%s' "$OUT" | tail -n 20; }

# 1. scope
CHANGED="$( { git -C "$WT" diff --name-only "$BASE...HEAD"; git -C "$WT" diff --name-only HEAD; git -C "$WT" ls-files --others --exclude-standard; } | sed '/^$/d' | sort -u)"
log "--- [scope] changed files:" "$CHANGED"
[[ -n "$CHANGED" ]] || verdict red scope "empty diff" ""
OUT_OF_SCOPE=""
while IFS= read -r f; do
  hit=0
  for g in $SCOPE; do
    # shellcheck disable=SC2053
    if [[ "$f" == $g ]]; then hit=1; break; fi
  done
  [[ $hit == 1 ]] || OUT_OF_SCOPE+="$f"$'\n'
done <<< "$CHANGED"
[[ -z "$OUT_OF_SCOPE" ]] || verdict red scope "files outside SCOPE" "$OUT_OF_SCOPE"

# 1b. lane: decided here from globs, not by the coordinator.
match_any() {  # match_any <file> <globs...>
  local f="$1" g; shift
  for g in "$@"; do
    # shellcheck disable=SC2053
    [[ "$f" == $g ]] && return 0
  done
  return 1
}
LANE3_HITS=""
while IFS= read -r f; do
  # shellcheck disable=SC2086
  if match_any "$f" $LANE3_GLOBS; then LANE3_HITS+="$f"$'\n'; continue; fi
  # shellcheck disable=SC2086
  if match_any "$f" $LANE_GLOBS_HARD $LANE2_GLOBS; then LANE=2; fi
done <<< "$CHANGED"
if [[ -n "$LANE3_HITS" ]]; then
  LANE=3
  verdict red lane "changed files are lane 3 (never automated)" "$LANE3_HITS"
fi
log "--- [lane] $LANE"

# 2. suppression
SUPP_RE='@ts-nocheck|@ts-ignore|@ts-expect-error|as any|\.only\(|\.skip\('
SRC_PATHS=('*.ts' '*.tsx' '*.js' '*.jsx' '*.mjs' ':!scripts/agent-gates/*')
HITS="$( { git -C "$WT" diff "$BASE" -- "${SRC_PATHS[@]}" | grep -E '^\+' | grep -Ev '^\+\+\+' || true; } | grep -E "$SUPP_RE" || true)"
UNTRACKED="$(git -C "$WT" ls-files --others --exclude-standard -- "${SRC_PATHS[@]}")"
if [[ -n "$UNTRACKED" ]]; then
  while IFS= read -r f; do
    HITS+="$(grep -HnE "$SUPP_RE" "$WT/$f" 2>/dev/null || true)"$'\n'
  done <<< "$UNTRACKED"
  HITS="$(printf '%s' "$HITS" | sed '/^$/d')"
fi
log "--- [suppression] hits:" "$HITS"
[[ -z "$HITS" ]] || verdict red suppression "added lines contain suppression markers" "$HITS"

assertion_infra() {
  log "--- [assertion] infrastructure failure: $*"
  echo "assertion infrastructure failure: $* (see $LOG)" >&2
  exit 2
}

assertion_file() {
  local root="$1" path="$2" part current="$1"
  [[ "$path" =~ ^[a-zA-Z0-9_./-]+$ && "$path" != /* ]] || return 1
  local remaining="$path"
  while [[ -n "$remaining" ]]; do
    part="${remaining%%/*}"
    [[ -n "$part" && "$part" != . && "$part" != .. ]] || return 1
    current="$current/$part"
    [[ ! -L "$current" ]] || return 1
    if [[ "$remaining" == */* ]]; then remaining="${remaining#*/}"; else remaining=""; fi
  done
  [[ -f "$root/$path" ]]
}

assertion_json() {
  python3 -I - "$@" 3<<< "${ASSERTION_MANIFEST:-}" <<'PYTHON'
import json
import os
import sys
from decimal import Decimal
from pathlib import Path


def reject_constant(_):
    raise ValueError("non-finite number is not JSON")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON object key")
        result[key] = value
    return result


def parse(text):
    return json.loads(text, parse_int=Decimal, parse_float=Decimal,
                      parse_constant=reject_constant, object_pairs_hook=unique_object)


def typed(value):
    if isinstance(value, dict):
        return ("object", {key: typed(item) for key, item in value.items()})
    if isinstance(value, list):
        return ("array", [typed(item) for item in value])
    return (type(value).__name__, value)


try:
    if sys.argv[1] == "freeze":
        raw = Path(sys.argv[2]).read_bytes().decode("utf-8")
        parse(raw)
        print(json.dumps(raw, ensure_ascii=True))
    else:
        with os.fdopen(3, encoding="ascii") as snapshot:
            manifest = parse(json.load(snapshot))
        actual = parse(Path(sys.argv[2]).read_bytes().decode("utf-8"))
        sys.exit(0 if typed(actual) == typed(manifest["expected"]) else 42)
except (ValueError, OSError, ArithmeticError, RecursionError) as error:
    print("invalid assertion JSON: " + str(error), file=sys.stderr)
    sys.exit(2)
PYTHON
}

run_assertion() {
  local phase="$1" dir="$2" rc=0 outcome arg assertions=0
  local argv=()
  while IFS= read -r -d '' arg; do argv+=("$arg"); done < <(jq -j 'fromjson | .probe[] + "\u0000"' <<< "$ASSERTION_MANIFEST")
  log "--- [assertion-$phase] id=$ASSERTION_ID_JSON manifest_digest=$ASSERTION_DIGEST"
  (cd "$dir" && timeout -k 5 60 "${argv[@]}") > "$TMP_ASSERTION/actual.json" 2> "$TMP_ASSERTION/stderr" || rc=$?
  log "$(cat "$TMP_ASSERTION/stderr")" "$(cat "$TMP_ASSERTION/actual.json")" "--- [assertion-$phase] probe exit $rc"
  outcome=infrastructure_failed
  if [[ "$rc" == 0 ]]; then
    rc=0
    assertion_json compare "$TMP_ASSERTION/actual.json" 2>> "$LOG" || rc=$?
    case "$rc" in
      0) outcome=passed; assertions=1 ;;
      42) outcome=assertion_failed; assertions=1 ;;
    esac
  fi
  local record
  record="$(jq -cn --argjson id "$ASSERTION_ID_JSON" --arg digest "$ASSERTION_DIGEST" --arg phase "$phase" --arg outcome "$outcome" --argjson assertions "$assertions" '{version:1,id:$id,manifest_digest:$digest,phase:$phase,outcome:$outcome,assertions:$assertions}')" || assertion_infra "cannot record comparator result"
  log "$record"
  [[ "$outcome" != infrastructure_failed ]] || assertion_infra "$phase probe did not produce one JSON observation with exit 0"
  ASSERTION_OUTCOME="$outcome"
}

if [[ "$CHECK_IS_NEW_TEST" == yes ]]; then
  command -v jq > /dev/null && command -v timeout > /dev/null && command -v python3 > /dev/null || assertion_infra "jq, timeout and Python3 are required"
  TMP_ASSERTION="$(mktemp -d "${TMPDIR:-/tmp}/agent-gate-assertion.XXXXXX")" || assertion_infra "cannot freeze assertion"
  declared=" "
  for f in $CHECK_TEST_FILES; do
    assertion_file "$WT" "$f" || assertion_infra "invalid, missing or symlinked CHECK_TEST_FILES path: $f"
    [[ "$declared" != *" $f "* ]] || assertion_infra "duplicate CHECK_TEST_FILES path: $f"
    declared+="$f "
    mkdir -p "$TMP_ASSERTION/files/$(dirname "$f")" && cp "$WT/$f" "$TMP_ASSERTION/files/$f" || assertion_infra "cannot freeze test dependency: $f"
  done
  [[ "$declared" == *" $CHECK_ASSERTION "* ]] || assertion_infra "CHECK_ASSERTION must be in CHECK_TEST_FILES"
  ASSERTION_MANIFEST="$(assertion_json freeze "$TMP_ASSERTION/files/$CHECK_ASSERTION" 2>> "$LOG")" || assertion_infra "malformed CHECK_ASSERTION JSON"
  jq -e '
    fromjson |
    if type == "object" and keys == ["expected","id","probe","version"] and .version == 1
      and (.id | type == "string" and length > 0 and (contains("\u0000") | not))
      and (.probe | type == "array" and length > 0 and all(.[]; type == "string" and (contains("\u0000") | not)))
      and (.probe[0] | length > 0)
    then . else error("expected version 1 assertion with id, probe argv and expected JSON") end
  ' <<< "$ASSERTION_MANIFEST" > /dev/null 2>> "$LOG" || assertion_infra "malformed CHECK_ASSERTION manifest"
  ASSERTION_ID_JSON="$(jq -c 'fromjson | .id' <<< "$ASSERTION_MANIFEST")" || assertion_infra "cannot read assertion id"
  ASSERTION_DIGEST="$(jq -j . <<< "$ASSERTION_MANIFEST" | git hash-object --stdin)" || assertion_infra "cannot digest assertion"
  readonly ASSERTION_MANIFEST ASSERTION_ID_JSON ASSERTION_DIGEST
  TMP_BASE="$(mktemp -d "${TMPDIR:-/tmp}/agent-gate-base.XXXXXX")" || assertion_infra "cannot prepare base"
  rmdir "$TMP_BASE"
  git -C "$WT" worktree add --detach "$TMP_BASE" "$BASE" >> "$LOG" 2>&1 || assertion_infra "cannot check out base"
  for f in $CHECK_TEST_FILES; do
    parent="$(dirname "$f")"
    current="$TMP_BASE"
    remaining="$parent"
    while [[ "$remaining" != . && -n "$remaining" ]]; do
      part="${remaining%%/*}"; current="$current/$part"
      [[ ! -L "$current" ]] || assertion_infra "symlinked baseline test parent: $f"
      if [[ "$remaining" == */* ]]; then remaining="${remaining#*/}"; else remaining=""; fi
    done
    [[ ! -L "$TMP_BASE/$f" ]] || assertion_infra "symlinked baseline test file: $f"
    mkdir -p "$TMP_BASE/$parent" && cp "$TMP_ASSERTION/files/$f" "$TMP_BASE/$f" || assertion_infra "cannot prepare test dependency: $f"
  done
  if [[ -d "$WT/node_modules" && ! -e "$TMP_BASE/node_modules" ]]; then ln -s "$WT/node_modules" "$TMP_BASE/node_modules" || assertion_infra "cannot prepare node_modules"; fi
  if [[ -d "$WT/web/node_modules" && ! -e "$TMP_BASE/web/node_modules" ]]; then ln -s "$WT/web/node_modules" "$TMP_BASE/web/node_modules" || assertion_infra "cannot prepare web/node_modules"; fi
  run_assertion base "$TMP_BASE"
  [[ "$ASSERTION_OUTCOME" == assertion_failed ]] || verdict red check-red-on-base "new assertion already passes on base" "$ASSERTION_ID_JSON"
  run_assertion head "$WT"
  [[ "$ASSERTION_OUTCOME" == passed ]] || verdict red check-assertion "new assertion fails on head" "$ASSERTION_ID_JSON"
fi

# 4. project checks: scripts/agent-gates/gate.project.sh (heavy, serialized).
# The script is read from $PROJECT_SRC (default origin/main), never from the worker's checkout,
# so a worker cannot weaken its own gate. GATE_PROJECT_SRC=worktree reads $WT's copy (self-run / self-test).
# It prints "== <step>" before each step; the last marker names the failing step.
PROJECT_GATE="$LOG_DIR/gate.project.sh"
if [[ "$PROJECT_SRC" == worktree ]]; then
  if [[ -f "$WT/scripts/agent-gates/gate.project.sh" ]]; then
    cp "$WT/scripts/agent-gates/gate.project.sh" "$PROJECT_GATE"
  else
    rm -f "$PROJECT_GATE"; log "--- [project] no gate.project.sh in worktree, skipped"
  fi
elif git -C "$WT" show "$PROJECT_SRC:scripts/agent-gates/gate.project.sh" > "$PROJECT_GATE" 2>/dev/null; then
  :
else
  rm -f "$PROJECT_GATE"; log "--- [project] no gate.project.sh at $PROJECT_SRC, skipped"
fi
if [[ -f "$PROJECT_GATE" ]]; then
  exec 9>"$LOCK"
  # Waiting out another gate is infrastructure, not a red verdict on this diff.
  if ! flock -w "$LOCK_WAIT" 9; then
    log "--- [project] timed out waiting for heavy lock $LOCK"
    echo "timed out waiting for heavy lock $LOCK" >&2
    exit 2
  fi
  export GATE_CHANGED="$CHANGED"
  # The project gate must not inherit fd 9: a leftover daemon would hold the lock.
  if ! run project "$WT" "bash '$PROJECT_GATE'" 9>&-; then
    step="$(printf '%s' "$OUT" | sed -n 's/^== //p' | tail -n 1)"
    verdict red "${step:-project}" "project check ${step:-} failed" "$(tail20)"
  fi
  exec 9>&-
fi

# 5. check
run check "$WT" "$CHECK" || verdict red check "CHECK failed" "$(tail20)"
verdict green all "all gates passed" ""
