#!/usr/bin/env bash
# orca-factory's heavy gate checks, run by gate.sh from the unit worktree root.
# GATE_CHANGED = changed paths, one per line. Print "== <step>" before each step.
# 不要在这里跑 `bin/orca-factory selftest` 或 gate.test.sh：它们会再调 gate.sh，嵌套抢 agent-gate-heavy.lock，空等 30 分钟。
# 它们放进票的 CHECK（gate.sh 第 5 步，锁已释放）。
set -Eeuo pipefail
# 依据：生产 AGENT_* 配置泄漏进 go test 会改变测试行为：AGENT_ISSUE_BACKEND 让 ledger 子命令递归执行测试二进制（2026-10-15 撑爆内存被 oomd 杀光 worker），AGENT_TIER_* 让合并验证中的 executor 测试失败（同日 OF-36）。本文件从基线读取，旧分支的测试也受保护。
for v in $(compgen -e | grep '^AGENT_'); do unset "$v"; done

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
  echo "请把产物放进票据主账本/GitHub/Orca，或先降低基线再提交（当前次数：${omc_count}，基线：${OMC_BASELINE}）" >&2
  exit 1
fi

echo "== driver"
if [ -f driver/go.mod ]; then
  (cd driver && go vet ./... && go test -count=1 ./...)
fi

echo "== driver-lint-vuln"   # 依据：OF-24，CI 跑 make lint/make vuln 但 gate 不跑，PR 开出后才发现失败（PR #421 的 gorilla/websocket 漏洞 GO-2026-6278 与 errcheck）
driver_go_tooling_changed=0   # 只在改动命中 driver/ 或 Go 工具链时才跑，避免拖慢与 driver 无关的票
while IFS= read -r f; do
  case "$f" in
    *.md) ;;
    driver/*|Makefile|scripts/dev-tools.lock|scripts/install-dev-tools.sh) driver_go_tooling_changed=1 ;;
  esac
done <<< "${GATE_CHANGED:-}"
if [ -f driver/go.mod ] && [ "$driver_go_tooling_changed" = 1 ]; then
  echo "== driver-lint"
  make lint
  echo "== driver-vuln"
  make vuln
fi

echo "== driver-wired"   # 依据：合并通道写完未接线，上线后一次都没合并（2026-10-06）
if [ -f driver/go.mod ]; then
  # 每个 internal 包都必须能从 cmd/ 到达
  unwired=$(cd driver && LC_ALL=C comm -23 <(go list ./internal/... | LC_ALL=C sort) <(go list -deps ./cmd/... | LC_ALL=C sort))
  if [ -n "$unwired" ]; then
    echo "以下包没有从 driver/cmd 接线，写完的功能不会运行：${unwired}" >&2
    exit 1
  fi
fi

echo "== driver-contracts"   # 依据：外部接口解析须用录制的真实样本测试（2026-10-05 驱动器上线复盘）
if [ -f driver/go.mod ]; then
  # 基线取分支与 origin/main 的分叉点：分支不得删减契约测试，main 之后新增的契约测试不让旧底座的分支变红
  base=$(git merge-base HEAD origin/main) || { echo "契约基线：找不到与 origin/main 的分叉点" >&2; exit 1; }
  # 测试文件随被测解析代码整体删除时不计入基线；仍存在的文件不得删减契约测试，
  # 除非被删的测试连同它读取的全部录制样本一起删除（契约退役，样本删除在 diff 中可审）
  CONTRACT_BASELINE=0
  while IFS= read -r file; do
    [ -f "$file" ] || continue
    while IFS= read -r name; do
      if ! grep -q "^func ${name}(" "$file"; then
        samples=$(git show "$base:$file" | awk -v n="func ${name}(" 'index($0,n)==1{p=1} p{print} p&&/^}/{exit}' | grep -oE 'testdata/recorded/[^"]+' | sort -u)
        retired=${samples:+1}
        for sample in $samples; do [ -e "driver/$sample" ] && retired=; done
        [ -n "$retired" ] && { echo "契约退役：$file $name（样本已删除）"; continue; }
      fi
      CONTRACT_BASELINE=$((CONTRACT_BASELINE + 1))
    done < <(git show "$base:$file" | sed -n 's/^func \(TestRecorded[A-Za-z0-9_]*\)(.*/\1/p')
  done < <(git grep -l '^func TestRecorded' "$base" -- 'driver/*_test.go' | sed "s#^$base:##")
  contract_passed=$( { (cd driver && go test -count=1 -v -run '^TestRecorded' ./...) || true; } | grep -c '^--- PASS: TestRecorded' || true)
  if [ "$contract_passed" -lt "$CONTRACT_BASELINE" ]; then
    echo "契约测试通过 ${contract_passed} 个，低于基线 ${CONTRACT_BASELINE}；解析外部输出的代码要用 driver/testdata/recorded/ 的真实样本测试（见 driver/CONTRACTS.md）" >&2
    exit 1
  fi
fi

echo "== ziikoo-driver-mod"   # 依据：OF-232，mod 的测试不在 CI 也不在 gate 里，OF-228/230/231 能合并只凭本地手动检查；合并通道在 driver 服务进程里跑 gate，服务 PATH 需含 claude
mod_changed=0   # 只在改动命中 mod 目录时才跑
while IFS= read -r f; do
  case "$f" in
    *.md) ;;
    .claude/skills/ziikoo-driver/*) mod_changed=1 ;;
  esac
done <<< "${GATE_CHANGED:-}"
if [ "$mod_changed" = 1 ]; then
  if ! command -v claude >/dev/null; then
    echo "改动命中 .claude/skills/ziikoo-driver/，要用 claude plugin test 验证，但 PATH 里找不到 claude；合并通道在 driver 服务进程里跑 gate，要在 orca-factory-driver.service 的 PATH drop-in 里加入 claude 所在目录" >&2
    exit 1
  fi
  claude plugin test .claude/skills/ziikoo-driver
  claude plugin validate .claude/skills/ziikoo-driver
fi
