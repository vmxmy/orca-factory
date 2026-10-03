#!/usr/bin/env bash
# zhiyun-ai-dashboard's heavy gate checks, run by gate.sh from the unit worktree root.
# GATE_CHANGED = changed paths, one per line. Print "== <step>" before each step.
set -Eeuo pipefail

echo "== tsc"            # tsc -b, never --noEmit: web/tsconfig.json is files:[] + references
(cd web && npx tsc -b)

echo "== eslint"         # two configs: repo root (functions/, scripts/) and web/
./node_modules/.bin/eslint .
(cd web && npx eslint .)

echo "== guards"         # source-scanning guards sit outside the import graph, so `vitest related` misses them
(cd web && npx vitest run --project unit ../functions/test/)

related=()
while IFS= read -r f; do
  case "$f" in *.test.js|*.test.ts|*.test.mjs|functions/*|web/src/*) ;; *) continue ;; esac
  [[ -f "$f" ]] || continue
  if [[ "$f" == web/* ]]; then related+=("${f#web/}"); else related+=("../$f"); fi
done <<< "${GATE_CHANGED:-}"
if (( ${#related[@]} )); then
  echo "== vitest"
  (cd web && npx vitest related --run --project unit "${related[@]}")
fi
