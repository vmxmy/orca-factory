#!/usr/bin/env bash
# orca-factory's heavy gate checks, run by gate.sh from the unit worktree root.
# GATE_CHANGED = changed paths, one per line. Print "== <step>" before each step.
# 不要在这里跑 `bin/orca-factory selftest` 或 gate.test.sh：它们会再调 gate.sh，嵌套抢 agent-gate-heavy.lock，空等 30 分钟。
# 它们放进票的 CHECK（gate.sh 第 5 步，锁已释放）。
set -Eeuo pipefail

OMC_BASELINE=27 # 只允许调低，不允许调高

echo "== syntax"   # 依据：仓库无 shellcheck/shfmt、无 CI，只有 bash -n 可用
for f in bin/orca-factory core/*.sh examples/*/gate.project.sh scripts/agent-gates/gate.project.sh; do
  bash -n "$f"
done

echo "== drift"    # 依据：scripts/agent-gates/ 是 core/ 的安装副本，改了 core/ 必须跟一次 `bin/orca-factory update .`
bin/orca-factory check .

echo "== omc-ratchet"
omc_count=$( { grep -ro '\.omc' core || [ "$?" -eq 1 ]; } | wc -l | tr -d '[:space:]')
if [ "$omc_count" -gt "$OMC_BASELINE" ]; then
  echo "请把产物放进 Linear/GitHub/Orca，或先降低基线再提交（当前次数：${omc_count}，基线：${OMC_BASELINE}）" >&2
  exit 1
fi

echo "== driver"
if [ -f driver/go.mod ]; then
  (cd driver && go vet ./... && go test -count=1 ./...)
fi

echo "== driver-wired"   # 依据：合并通道写完未接线，上线后一次都没合并（2026-10-06）
if [ -f driver/go.mod ]; then
  # 每个 internal 包都必须能从 cmd/ 到达；release 待接线（另有票）
  unwired=$(cd driver && LC_ALL=C comm -23 <(go list ./internal/... | LC_ALL=C sort) <(go list -deps ./cmd/... | LC_ALL=C sort) | grep -v '/internal/release$' || true)
  if [ -n "$unwired" ]; then
    echo "以下包没有从 driver/cmd 接线，写完的功能不会运行：${unwired}" >&2
    exit 1
  fi
fi

echo "== driver-contracts"   # 依据：外部接口解析须用录制的真实样本测试（2026-10-05 驱动器上线复盘）
if [ -f driver/go.mod ]; then
  # 基线取分支与 origin/main 的分叉点：分支不得删减契约测试，main 之后新增的契约测试不让旧底座的分支变红
  base=$(git merge-base HEAD origin/main) || { echo "契约基线：找不到与 origin/main 的分叉点" >&2; exit 1; }
  CONTRACT_BASELINE=$(git grep -h '^func TestRecorded' "$base" -- 'driver/*_test.go' | wc -l)
  contract_passed=$( { (cd driver && go test -count=1 -v -run '^TestRecorded' ./...) || true; } | grep -c '^--- PASS: TestRecorded' || true)
  if [ "$contract_passed" -lt "$CONTRACT_BASELINE" ]; then
    echo "契约测试通过 ${contract_passed} 个，低于基线 ${CONTRACT_BASELINE}；解析外部输出的代码要用 driver/testdata/recorded/ 的真实样本测试（见 driver/CONTRACTS.md）" >&2
    exit 1
  fi
fi
