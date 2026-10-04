# 共享条款：工程

由 orca-factory 分发，项目不得覆盖；项目特化只能写 `local-engineering.md` 追加。

## 1. gate 先行

- 唯一结论来源是 `gate.sh` 的输出块：`UNIT / VERDICT / STEP / REASON / EVIDENCE / SCOPE / LANE`。
- lane 由 gate 的 `LANE` 行决定，判据在 `agent-graph.env` 的 `AGENT_LANE2_GLOBS` / `AGENT_LANE3_GLOBS`；`scripts/agent-gates/*` 永远 lane 2。gate 没跑之前只有预估。
- gate 红了修改动，不修 gate。gate 文件（`gate.sh`、`gate.project.sh`、本目录）只在 lane 2 PR 里改。

## 2. 先红后绿

- 先写会红的断言（`CHECK`），在 base 上确认它红，再实现到绿。gate 的 `check-red-on-base` 步会复核：新测试在 origin/main 上必须失败。
- 断言不得同义反复：期望值不得用实现同款调用生成；不得只断言"不抛错"或"返回非空"。
- `CHECK_IS_NEW_TEST: yes` 必须同时给 `CHECK_TEST_FILES`。

## 3. 只改 SCOPE

- 只改 gate spec `SCOPE` 内的文件。越界 = `VERDICT red STEP scope`。
- 需要越界 → 停下走 handoff（worker 发 escalation，coordinator 改 plan），不自行扩 SCOPE。
- 顺手修改、顺手格式化、顺手重命名一律不做。

## 4. 不信自报

- "已通过""已测试""filesModified" 都是声明，不是证据。
- 证据只有三种：gate 输出块、`git diff`、测试命令的退出码与输出。
- coordinator 独立复跑 gate，`GATE_PROJECT_SRC` 保持默认 `origin/main`（项目检查脚本从 main 取，工人改不动自己的门）；worker 自跑只能用 `GATE_PROJECT_SRC=worktree`，且只作自查。
- 每条声明都要能回指到 文件:行、测试名或 gate 步。回指不了的声明按不存在处理。

## 5. 压制标记禁令

- 新增行不得含 `@ts-nocheck`、`@ts-ignore`、`@ts-expect-error`、`as any`、`.only(`、`.skip(`；gate 的 `suppression` 步会拦。
- 不得删除、注释、放宽已有测试、lint 规则、类型约束、守卫阈值来换绿。
- 不得用 `try/catch` 吞错、用 `eslint-disable` 绕 lint。

## 6. 确定性优先

- 能用脚本或测试判定的，不用模型判断；能加进 `gate.project.sh` 的检查，不写进 prompt。
- 模型判断（复核 diff、异构审查）只覆盖确定性检查到不了的地方，结论必须带 文件:行 证据，且只能导致打回或升级，不能单独放行。
- 同一失败原因出现两次就把它变成检查：加 gate 步、加测试，或追加 `docs/agent-graph/brief-constraints.md`。

## 7. 产物存储

- 流水线产物（账本、简报、标记、日志、评审记录）不得写入本地自建路径。
- 把产物放进用户已有的工具链：Linear（票、评论、账本）、GitHub（PR、gate 证据）、Orca（任务 spec、worktree 元数据、归属），以及用户在初始化时选定的飞书。
- 本地只允许当前 worktree 内一次性的草稿；下一轮运行不得依赖它。
- 新增任何产物前，先说明它存在哪个工具里。
