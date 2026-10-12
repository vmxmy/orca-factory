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
CHECK_IS_NEW_TEST="${CHECK_IS_NEW_TEST:-no}"
for k in UNIT SCOPE CHECK; do
  [[ -n "${!k}" ]] || { echo "spec missing $k" >&2; exit 2; }
done
if [[ "$CHECK_IS_NEW_TEST" == yes && -z "$CHECK_TEST_FILES" ]]; then
  echo "spec missing CHECK_TEST_FILES" >&2; exit 2
fi

LOG_DIR="$WT/.omc/gates/$UNIT"
mkdir -p "$LOG_DIR"
LOG="$LOG_DIR/gate.log"
TMP_BASE=""
LOCK="${TMPDIR:-/tmp}/agent-gate-heavy.lock"
HAVE_LOCK=0
cleanup() {
  if [[ -n "$TMP_BASE" ]]; then git -C "$WT" worktree remove --force "$TMP_BASE" >/dev/null 2>&1 || true; fi
  if [[ "$HAVE_LOCK" == 1 ]]; then rmdir "$LOCK" 2>/dev/null || true; fi
}
trap cleanup EXIT

log() { printf '%s\n' "$@" >> "$LOG"; }
log "=== gate $UNIT $(date -u +%FT%TZ) base=$BASE project_src=$PROJECT_SRC ==="

verdict() {
  local v="$1" step="$2" reason="$3" evidence="$4"
  printf 'UNIT      %s\nVERDICT   %s\nSTEP      %s\nREASON    %s\n' "$UNIT" "$v" "$step" "$reason"
  printf 'EVIDENCE  %s\n' "$(printf '%s' "$evidence" | head -n 20)"
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

# 3. check-red-on-base
if [[ "$CHECK_IS_NEW_TEST" == yes ]]; then
  TMP_BASE="$(mktemp -d "${TMPDIR:-/tmp}/agent-gate-base.XXXXXX")"
  rmdir "$TMP_BASE"
  git -C "$WT" worktree add --detach "$TMP_BASE" "$BASE" >> "$LOG" 2>&1
  for f in $CHECK_TEST_FILES; do
    mkdir -p "$TMP_BASE/$(dirname "$f")"
    cp "$WT/$f" "$TMP_BASE/$f"
  done
  if [[ -d "$WT/node_modules" && ! -e "$TMP_BASE/node_modules" ]]; then ln -s "$WT/node_modules" "$TMP_BASE/node_modules"; fi
  if [[ -d "$WT/web/node_modules" && ! -e "$TMP_BASE/web/node_modules" ]]; then ln -s "$WT/web/node_modules" "$TMP_BASE/web/node_modules"; fi
  if run check-red-on-base "$TMP_BASE" "$CHECK"; then
    verdict red check-red-on-base "new test already passes on base, not a check" "$(tail20)"
  fi
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
  deadline=$(( $(date +%s) + 1800 ))
  until mkdir "$LOCK" 2>/dev/null; do
    (( $(date +%s) < deadline )) || verdict red project "timed out waiting for heavy lock $LOCK" ""
    sleep 5
  done
  HAVE_LOCK=1
  export GATE_CHANGED="$CHANGED"
  if ! run project "$WT" "bash '$PROJECT_GATE'"; then
    step="$(printf '%s' "$OUT" | sed -n 's/^== //p' | tail -n 1)"
    verdict red "${step:-project}" "project check ${step:-} failed" "$(tail20)"
  fi
  rmdir "$LOCK" 2>/dev/null || true
  HAVE_LOCK=0
fi

# 5. check
run check "$WT" "$CHECK" || verdict red check "CHECK failed" "$(tail20)"
verdict green all "all gates passed" ""
