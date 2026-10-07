# roles/：角色 prompt 与 task spec 拼装

每个角色文件固定四段：`## Owns` / `## Does Not Own` / `## Verification` / `## Handoff`。短句、祈使、不解释。

| 文件 | 给谁 | 怎么用 |
| --- | --- | --- |
| `intake.md` | 用户在 Orca 里手动开的 claude 标签页（交互模式） | prompt `按 scripts/agent-gates/roles/intake.md 工作`；把需求变成合格票、汇报；`auto-failed` 由 resolver 接管 |
| `coordinator.md` | 定时任务启动的 coordinator | 与 `COORDINATOR.md` 一起读 |
| `worker.md` | 每个单元的 worker | 原文作为 task spec 开头 |
| `resolver.md` | driver resolve 启动的处置 Agent | 接管异常、预算耗尽和审批等待；提交经验证的合并/部署提案 |
| `workspace-reconciler.md` | 每轮流水线收尾的 Orca 资源 Agent | 盘点并安全清理 worktree/workspace；不确定项转人工决策 |
| `reviewer.md` | lane 2 异构审查命令 | 原文 + 简报经 stdin 喂给 `$AGENT_ROLE_REVIEWER_CMD` |
| `local-worker.md` | 项目特化（可选） | 追加在 `worker.md` 之后，只能加约束 |

共享五份（`intake.md` `coordinator.md` `worker.md` `reviewer.md` `resolver.md`）由 orca-factory 分发，项目不改；项目规矩写 `local-worker.md`（模板在 orca-factory `templates/local-worker.md.example`）。

intake 与 coordinator 永不同会话：intake 只写 Linear、只读仓库，由用户手动开会话；coordinator 由定时任务启动，领票并执行。一个会话里不能既建票又领票。

## task spec 拼装

先在简报头部填写票号、unit 名与备份分支，再拼装 task spec；三者须与本单元一致，不留占位符。

```bash
R=scripts/agent-gates/roles
cat "$R/worker.md" $( [ -f "$R/local-worker.md" ] && echo "$R/local-worker.md" ) .omc/tmp/<ID>/<unit>.brief.md > .omc/tmp/<ID>/<unit>.spec.md
orca orchestration task-create --run <run> --task-title "<ID>: <unit>" --display-name <unit> --spec "$(cat .omc/tmp/<ID>/<unit>.spec.md)" --json
```

## 单元简报模板（`<unit>.brief.md`）

```
# 简报 <ID> / <unit>

票号: <ID>　unit 名: <unit>　备份分支: agent/<ID>/<unit>

Target: <一句话：改哪里、达成什么>

Change:
1. <可验证的一条改动；追溯块 REQ 1 指它>
2. <…；REQ 2 指它>

Ownership: <本单元负责的文件/符号；与其他单元的边界>

INPUTS: <上游单元产物、要读的文件、符号名；没有写 无>

CHECK: <可执行命令，实现前必红、实现后必绿>
CHECK_IS_NEW_TEST: yes|no
SCOPE: <空格分隔 glob，与 gate spec 一致>
LANE: <gate 预估 1|2>

Constraints:
- <brief-constraints.md 命中本 SCOPE 的条目，原样>

步骤:
0. 安装依赖：<$AGENT_INSTALL_CMD 原值>
1. 先加断言跑 CHECK，确认红
2. 实现到绿
3. GATE_PROJECT_SRC=worktree scripts/agent-gates/gate.sh . <gate spec 绝对路径>；红了按 VERDICT 修，只改 SCOPE
4. 按 worker.md 完成最后一次提交与推送，再发 worker_done：--outcome、追溯块、gate 输出块、--files-modified（见 roles/worker.md Handoff）
```

gate spec 与简报分开放：`.omc/tmp/<ID>/<unit>.gate.spec` 只含 `UNIT / SCOPE / CHECK / CHECK_IS_NEW_TEST / CHECK_TEST_FILES`。
