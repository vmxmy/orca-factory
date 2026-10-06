# ZII-896 / executor — correction 1/3

本轮仅交付此文件，未修改生产代码、测试或 gate spec，未启动 worker。原票尚未实现；下一棒应先修订 SCOPE 与验收边界。本报告核查的起点是 `079cfb64ea2ddfd53fc86a045617c337e7860f95`，本地 `origin/main` 为 `d91c0f6e80b8a43c5705a1bc756a004712fafd8c`。

## 纠正前提

派发文本中“usage 包不存在、ledger 不接受 usage”的判断已过时。这两个依赖在当前 HEAD 和本地 origin/main 中均为已提交代码：

- `driver/internal/usage/contract.go:23` 定义 `TicketUsage`。
- `driver/internal/usage/usage.go:213` 提供 `Collect(ctx, ticket, worktrees, attribution ...Options) (TicketUsage, error)`，前三个参数可直接调用。
- `driver/internal/ledger/ledger.go:131` 校验可选 `usage` 字段；`driver/internal/ledger/usage_test.go:11` 覆盖保留 usage、旧格式兼容及错误类型。

因此无需等待缺失的 usage 桩，也无需扩 SCOPE 去修改 usage/ledger。仅向 merge/executor 注入 fake sink 并加测试，有机会使当前 CHECK 变绿；真正受阻的是生产调用、合并恢复路径与去重生命周期。CHECK 变绿与原票端到端完成应分别验收。

## 当前可复核证据

执行原 CHECK 的全部判断，另打印捕获的测试输出供核查：

```bash
cd driver && go vet ./... && out=$(go test -count=1 -v -run TestLedgerOnFinish ./internal/merge/... ./internal/executor/...) && { printf '%s\n' "$out"; grep -q -- '--- PASS: TestLedgerOnFinish' <<<"$out"; }
```

退出码为 **1**，输出：

```text
testing: warning: no tests to run
PASS
ok  github.com/vmxmy/orca-factory/driver/internal/merge     0.002s [no tests to run]
testing: warning: no tests to run
PASS
ok  github.com/vmxmy/orca-factory/driver/internal/executor  0.002s [no tests to run]
```

这是最终 grep 未找到目标测试的红；不是编译失败，也不是行为断言失败。`make vet` 另行执行并退出 **0**。

写报告前，用原 gate spec 执行仓库自查：

```bash
GATE_PROJECT_SRC=worktree bash scripts/agent-gates/gate.sh /home/laoxu/orca/workspaces/orca-factory/ZII-896-executor /home/laoxu/.local/state/orca-factory/orca-factory/ZII-896/gate.spec
```

退出码 **1**，原样输出：

```text
UNIT      executor
VERDICT   red
STEP      scope
REASON    empty diff
EVIDENCE
SCOPE     driver/internal/merge/* driver/internal/executor/*
LANE      1
```

写报告后以同一命令复跑，退出码仍为 **1**，最新输出为：

```text
UNIT      executor
VERDICT   red
STEP      scope
REASON    files outside SCOPE
EVIDENCE  correction.md
SCOPE     driver/internal/merge/* driver/internal/executor/*
LANE      1
```

gate 在 scope 步停止，未执行 project 检查；此处 LANE=1 是提前退出时的默认值。报告文件自身不在原功能票 SCOPE 内，仅按本轮明确授权的单文件 correction 交付，不能冒充功能实现或 green gate。首个输出块的空 EVIDENCE 行省去了行尾空格。

## 实际阻塞与遗漏

1. **生产 merge 调用在 SCOPE 外。** `driver/internal/scheduler/merge.go:192-193` 每次创建 `merge.New(...)`，Request 只传 repository、path、PR、catch-up rounds。`driver/internal/merge/merge.go:16` 的 Request 没有 ticket 标识、领取时刻或 worker worktree 列表；Channel 也没有 ledger sink。新增可选字段不会使未更新的生产调用者自动提供这些数据。
2. **恢复已合并 PR 会绕过 Channel。** `driver/internal/scheduler/merge.go:183` 的 `pr.Merged` 分支直接使用权威 MergeSHA，随后写完成回执和 Done，不调用 `Channel.Next`。只在 `finish(Merged, "")` 写 ledger 会漏掉这条恢复路径。当前 `mergeAdapter.Squash` 通过 `driver/internal/github/operations.go:106` 的 MergePR 回读并确认 Merged；沿用既有确认契约，避免把 merge 前 Snapshot 当作合并后回读证据。
3. **内存去重不能覆盖生产重建。** scheduler 每次重新创建 Channel；executor 的 `withLog`（`driver/internal/executor/log.go:19`）复制 Executor。去重状态需有明确所有者，至少跨同一领取的重复调用与 Channel 重建保持；若承诺进程恢复后的唯一写入，还需可恢复凭据或幂等 sink。`ledger.Stats` 的去重只改变统计结果，`ledger.Append` 本身不防重复写。
4. **领取时刻需要存储并传递。** `machine.State`、`status.Ticket` 和 executor checkpoint 当前没有领取时刻字段；executor 已有 `Now`。`merge.Handoff.EnqueuedAt` 是合并入队时刻，不能充当原领取时刻。恢复及 correction 需要沿用同一次领取的 started_at，而重新领取应产生新键；其定义应先写入 brief。
5. **usage 需要票号和真实工作树。** `usage.go:62-67` 要求如 `ZII-896` 的票号，应传 `t.Identifier`，不能传 Linear UUID `t.ID`。`merge.Handoff` 已有 Identifier 和 worker Worktree，但 scheduler 没有传给 Request。`Request.Path` 是仓库 checkout，不能默认等同 worker Worktree。ledger 的 ticket 字段也应明确使用可读票号，UUID 仅用于 Linear 写入。
6. **转人覆盖与 outcome 已有契约。** `executor/budget.go:27` 的 handoff 在标签更新及评论确认后才能记录成功转人；`executor.go:209` 的预算转人使用此函数，但普通 `PhaseHuman` 在 `executor.go:223` 构造并发送转人评论，未统一经过 handoff。测试应覆盖这些入口。`scripts/agent-gates/constitution/workflow.md` §4 已规定 outcome 为 `merged|escalated|no_ticket`，转人使用 **escalated**；`ledger/stats.go:150` 也按该值统计。原建议默认写 handoff 会破坏已有统计语义。
7. **记录字段不能靠填默认值完成。** 首次独立 gate、领票 tier、累计 corrections、ci_reds 和 stage_secs 的来源需逐项明确；`Result.Corrections` 是本次 merge 的增量，不是全票累计值。usage 采集失败按安全日志规则记录摘要，JSON 中省略 usage，其他字段仍写入；失败 Append 的重试与去重标记顺序也需要明确。

## 下一棒的执行顺序

1. coordinator 先选择交付边界。完整落地建议增加 `driver/internal/scheduler/*` 及确实需要的组合入口/测试到 SCOPE，覆盖数据传递、实际 sink 接线、已合并恢复和去重所有权；executor 的现有组合入口是 **SCOPE 内**的 `driver/internal/executor/entry.go:81`，不必假定所有接线都只能放 cmd/driver。若保留原 SCOPE，则明确本票只交付可注入组件能力，把生产接线与恢复列为依赖票；这属于验收变更，需上游明确决定。仅删去 usage 要求不能解决 scheduler 接线问题。
2. 保持已提交的 usage/ledger 契约，按明确的领取身份传递 ticket、started_at、worktrees 及统计字段。确定去重承诺是否覆盖重启，并在有共享调用者、已落盘 checkpoint/Linear handoff 的前提下保留既有恢复契约。
3. 先写真正的行为断言并观察失败，再实现。两包分别提供 `TestLedgerOnFinish`，覆盖成功合并确认、真实转人、带 usage、采集失败省略 usage 并日志、同键重复以及 Append 失败后重试；若验收完整生产链路，再覆盖 scheduler 已合并恢复和 Channel 重建。再次仅调用空队列 `Next` 不足以证明去重。
4. coordinator 修订 gate spec 的新测试元数据：当前 `CHECK_IS_NEW_TEST: no`、`CHECK_TEST_FILES` 为空，不能复核新断言在固定 base 上失败。指定独立测试文件及可在 base 编译的行为测试方案，再启用相应门禁；本轮未修改 spec。
5. 新 CHECK 应分别证明两个包中的目标测试执行且通过；原 grep 只要求任一 `--- PASS: TestLedgerOnFinish`，不能独立证明两条路径都有测试。随后按 driver/DEVELOPMENT.md 运行定向 Make 测试及所需 race 检查，保留 executor 整包 goleak 门禁，最后运行原仓库 gate 和 clean-checkout 验证。
6. 仅在验收边界与代码范围一致后提交实现，推送到 `agent/ZII-896/executor`；报告 gate 的实际结果，由 coordinator 决定后续，不开 PR、不合并。

删除路径：无。未执行实现后 green CHECK、完整 project gate 或 clean-checkout green 验证，因为本轮授权交付仅为 correction.md，未实现功能，现有 gate 已在 scope 步红灯退出。遵守单文件限制，本轮未改写 `~/.agents/LESSONS.md`；供后续获准记录的纠正为：`When a retry brief alleges missing dependencies, verify the tracked checkout and production callers before declaring the same blocker.`
