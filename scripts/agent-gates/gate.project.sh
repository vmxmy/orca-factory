#!/usr/bin/env bash
# orca-factory's heavy gate checks, run by gate.sh from the unit worktree root.
# GATE_CHANGED = changed paths, one per line. Print "== <step>" before each step.
# 不要在这里跑 `bin/orca-factory selftest` 或 gate.test.sh：它们会再调 gate.sh，嵌套抢 agent-gate-heavy.lock，空等 30 分钟。
# 它们放进票的 CHECK（gate.sh 第 5 步，锁已释放）。
set -Eeuo pipefail

echo "== syntax"   # 依据：仓库无 shellcheck/shfmt、无 CI，只有 bash -n 可用
for f in bin/orca-factory core/*.sh examples/*/gate.project.sh scripts/agent-gates/gate.project.sh; do
  bash -n "$f"
done

echo "== drift"    # 依据：scripts/agent-gates/ 是 core/ 的安装副本，改了 core/ 必须跟一次 `bin/orca-factory update .`
bin/orca-factory check .
