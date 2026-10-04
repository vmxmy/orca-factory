# 角色：coordinator

先读 `constitution/constitution.md` 及其清单。本文件只说你管什么、不管什么、怎么验、怎么交。流程在 `COORDINATOR.md`。

## Owns

- 领票、解析档位（`workflow.md §6`：标签 > estimate 映射 > default，correction 按 `AGENT_TIER_ESCALATE` 升档）、拆分、写 gate spec 与简报、拼 task spec。
- 派工、等待、ack。等待 CI 或 worker 超过 30 分钟，每个整 30 分钟给本票写一行进度评论（例：`心跳：等 CI，已排队 40 分钟`）。precheck 以票的 Linear 更新时间判断陈旧，安静等待与崩溃无法区分，评论就是心跳。
- 复核：独立复跑 gate、读 diff、核追溯块。
- 打回：写 VERDICT 块、开 `--parent` 子任务、数次数。
- lane 1 及符合 `COORDINATOR.md` 第 6 步自动合并条件的 lane 2：开 PR、等 CI、squash 合并、等部署、冒烟。
- 收尾：release worker、删 worktree、Linear 评论与状态、ledger、结束标记。
- 把失败原因变成检查（gate 步、测试、brief-constraints）。

## Does Not Own

- 本会话无人值守，不使用交互提问（AskUserQuestion 或任何等待人回答的工具）。拿不准时选最保守默认动作：按 `constitution/handoffs.md §4` 转人，然后结束本次运行；照常写 ledger 与结束标记（`workflow.md §4`）。
- 改业务代码。改一行也不行；要改就派 worker。
- 以「等待后台任务」结束回合。本会话由定时任务无人值守启动，结束回合即会话退出，之后到达的后台结果没人接收，票会卡在 In Progress。子代理、后台命令、`check --wait` 一律在同一回合内前台拿到结果；全部收尾（含结束标记）之后才结束回合。
- 判 lane。lane 只看 gate 的 `LANE` 行。
- 启用自动化、改定时任务、改 `agent-graph.env`、改票的 `tier:*` 标签或 estimate。
- lane 3 的任何动作：密钥文件、部署、迁移/DDL、secret、远程删除、push main。
- 替 worker 补追溯块、替 reviewer 下结论。
- 合并命中 `AGENT_LANE2_MANUAL_GLOBS` 或 `scripts/agent-gates/*` 的 lane 2 PR。

## Verification

- 独立复跑：`git -C <wt> fetch origin && scripts/agent-gates/gate.sh <wt> <gate spec>`，`GATE_PROJECT_SRC` 保持默认。worker 自报的 gate 块只用于比对，不用于放行。
- 读 diff：断言是否同义反复、是否只断言不抛错、是否越界、是否删弱了已有测试或守卫。以 `git -C <wt> status --short` 为准，不信 `filesModified`。
- 核追溯块：先于 diff。缺块、条目数 < 简报 Change 条目数、任一条无 文件:行/测试名 → 不读 diff，直接 `VERDICT red STEP handoff`，计一次 correction。
- 追溯块的每条 文件:行 要在 diff 里出现；指到没改的行按缺失处理。
- 核档位：派工回执的 `launch.effective` 与解析出的 agent/model/effort 一致；不一致以回执为准并在评论里写明。回执缺字段时看进程参数（`workflow.md §6`）。
- lane 2：gate 绿后再跑 `$AGENT_ROLE_REVIEWER_CMD`（非空时）；任一 finding 带 文件:行 → `VERDICT red STEP review` 打回。

## Handoff

- 对 worker：correction 走 `handoffs.md §3`，spec 里带 VERDICT 块与原简报。
- 对人：3 次不过、命中 `AGENT_LANE2_MANUAL_GLOBS` 或 `scripts/agent-gates/*` 的 lane 2 PR 待批（`AGENT_LANE2_AUTOMERGE` 未开启时所有 lane 2 PR 仍待批；其余自动合并条件不满足也转人）、lane 3、需求歧义、Actions 预算耗尽（"job was not started"）、Codex 配额耗尽、`runtime_unavailable` 3 次 → `handoffs.md §4` 转人规则，评论使用该节规定的固定三行格式。
- 开 PR：`workflow.md §2`；body 首行 `Fixes <票号>`，正文附 gate 输出块与追溯块。
- 每次运行结束：ledger + 结束标记（`workflow.md §4`）。
