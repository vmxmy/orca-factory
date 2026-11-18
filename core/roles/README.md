# roles/：角色 prompt 与 task spec 拼装

每个角色文件固定四段：`## Owns` / `## Does Not Own` / `## Verification` / `## Handoff`。短句、祈使、不解释。

| 文件 | 给谁 | 怎么用 |
| --- | --- | --- |
| `intake.md` | 用户在 Orca 里手动开的 claude 标签页（交互模式） | prompt `按 scripts/agent-gates/roles/intake.md 工作`；把需求变成合格票、汇报；`auto-failed` 由 resolver 接管 |
| `intake-chat.md` | `driver feishu-gateway` 每轮启动的只读需求整理 Agent | 网关以 `--append-system-prompt-file` 传入检出中的本文件；在飞书话题里追问并输出 `intake-draft-v1` 草稿，不建票 |
| `onboard.md` | `driver onboard` 或需求助手“接入”按钮启动的接入 Agent | driver 以 `--append-system-prompt-file` 传入 orca-factory 根目录中的本文件；在目标仓库独立 worktree 里 init、填项目文件、跑 ONBOARD 5a～5d、开接入 PR，不合并，不写飞书 |
| `coordinator.md` | 定时任务启动的 coordinator | 与 `COORDINATOR.md` 一起读 |
| `worker.md` | 每个单元的 worker | 原文作为 task spec 开头 |
| `driver-worker.md` | driver 执行器派的 worker | 原文 + `local-worker.md`（若有）+ 简报，作为 task spec |
| `correction.md` | driver 执行器的 correction 会话 | 原文 + correction 文件路径、失败原因、简报，作为 prompt |
| `resolver.md` | driver resolve 启动的处置 Agent | 接管异常、预算耗尽和审批等待；提交经验证的合并/部署提案 |
| `reviewer.md` | lane 2 异构审查命令 | driver 按结构化角色或 `_CMD` 生成命令，原文 + 简报经 stdin 喂给 reviewer |
| `local-worker.md` | 项目特化（可选） | 追加在 `worker.md` 或 `driver-worker.md` 之后，只能加约束 |

共享九份（`intake.md` `intake-chat.md` `onboard.md` `coordinator.md` `worker.md` `driver-worker.md` `correction.md` `reviewer.md` `resolver.md`）由 orca-factory 分发，项目不改；项目规矩写 `local-worker.md`（模板在 orca-factory `templates/local-worker.md.example`）。

intake 与 coordinator 永不同会话：intake 只写配置选定的票据主账本、只读仓库，由用户手动开会话；coordinator 由定时任务启动，领票并执行。一个会话里不能既建票又领票。

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
CHECK_TEST_FILES: <yes 时列出观察脚本、断言 manifest 及全部测试依赖>
CHECK_ASSERTION: <yes 时填 CHECK_TEST_FILES 中的相对 JSON 文件路径>
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

gate spec 与简报分开放：`.omc/tmp/<ID>/<unit>.gate.spec` 只含 `UNIT / SCOPE / CHECK / CHECK_IS_NEW_TEST / CHECK_TEST_FILES / CHECK_ASSERTION`。

gate 的 `suppression` 步也检查 Go：`*.go` 新增行中的 `t.Skip(...)`、`t.Skipf(...)`、`t.SkipNow()` 和 `//nolint`（含指定规则）会判红；新增或修改任意目录下的 `.golangci.*` 配置同样判红。已有且未改动的标记和配置不受影响。不提供放行开关；确需新增 skip 时，与 TS 抑制标记一样转人处理。

## 新测试的确定性证据

`CHECK` 仍是最终 head 检查的任意 shell 命令。`CHECK_IS_NEW_TEST: no` 或省略该字段时，原有行为不变。
`CHECK_IS_NEW_TEST: yes` 还要求 `CHECK_ASSERTION`，它必须是 `CHECK_TEST_FILES` 中的一个普通相对文件。例如：

```text
UNIT: executor
SCOPE: src/* test/*
CHECK: bash test/value.test.sh
CHECK_IS_NEW_TEST: yes
CHECK_TEST_FILES: test/value.observe.sh test/value.assertion.json test/helper.sh
CHECK_ASSERTION: test/value.assertion.json
```

`test/value.assertion.json` 的完整内容为：

```json
{"version":1,"id":"value-is-two","probe":["bash","test/value.observe.sh"],"expected":{"value":2}}
```

`probe` 是非空 argv 数组，gate 直接执行它，不作 shell 求值。观察脚本从实现读取实际值，退出 0，stdout 仅输出一个 JSON 值。
错误诊断写 stderr。gate 内固定的 JSON 深度相等比较器完成一次带 `id` 的断言。
比较器需要 `jq`、GNU `timeout` 和 Python3，缺少任何一个都是退出 2 的基础设施错误。
Python3 标准库读取 manifest 与观察文件的原始字节，按 UTF-8 严格解析 JSON，拒绝原始 NUL、无效编码、NaN、Infinity、前导零和重复对象键。
expected 中合法的 `\u0000` 转义仍受支持。数字按十进制精确比较；布尔值与数字分开，`true` 不等于 `1`。
gate 将已验证的 manifest 原文保存在不可变内存快照中。base 与 head 的 ID、probe argv、expected 和原始内容摘要均来自该快照，不重新读取 manifest 路径。
probe 修改工作树或临时依赖副本中的 manifest 不会改变断言。base 值不等才证明断言失败，head 值必须相等，然后运行项目检查与原有 `CHECK`。
`CHECK` 打印的通过、失败或测试数量不构成新测试证据。

每个 probe 的时限为 60 秒，超时后最迟再过 5 秒强制终止。超时、信号、非零退出、空输出、多个 JSON 值、非法 JSON、无效 manifest、缺失依赖或失败的准备步骤均为基础设施失败，退出 2，没有 `VERDICT` 块。
base 已相等时是 `VERDICT red STEP check-red-on-base`；head 不相等时是 `VERDICT red STEP check-assertion`。
日志记录比较器生成的 `version / id / manifest_digest / phase / outcome / assertions`，完成比较的断言数为 1，基础设施失败为 0。
声明文件只接受 ASCII 字母、数字和 `_`、`.`、`/`、`-`，拒绝绝对路径、`.` 或 `..` 路径段、重复路径及符号链接。
列全新测试需要的 helper 与 fixture；复制声明的测试文件不代表可以复制本次修改的业务实现到基线。

## 升级已有新测试票

先升级 Factory，再从 Factory 源码检出目录运行 `bin/orca-factory update /absolute/path/to/consumer-repo`，参数为消费者仓库的绝对路径。
消费者仓库不包含 `bin/orca-factory`。只有更新 Factory 自身的安装副本时，才在 Factory 源码检出目录运行 `bin/orca-factory update .`。
分发后，按 Factory 的 `driver/DEVELOPMENT.md` 验证和更新 Driver。

旧 `CHECK_IS_NEW_TEST: yes` 缺少 manifest 时明确拒绝；不把任意失败当红，也不自动改成 `no`。
票的完整正文承载 `CHECK_IS_NEW_TEST`、`CHECK_TEST_FILES` 和 `CHECK_ASSERTION`；飞书保留在正文，不新增表字段。
票面全文里 `CHECK`、`SCOPE`、`CHECK_IS_NEW_TEST`、`CHECK_TEST_FILES` 和 `CHECK_ASSERTION` 的字段行（`字段:` 行或同名标题）各只能出现一次。driver 读到第二处时拒绝该票，不派工，也不把两处合并。
intake 的通用 CHECK 仍可使用；声明新测试时，在完整正文补齐这三个字段。

尚未交接的票由负责人补充 manifest 与字段，再用既有 `gate-spec-refresh: <原因>` 指令刷新当前 gate spec。
刷新只重放当前 head 的 gate，不清空 checkpoint、attempt 或请求 UUID。
已经冻结的旧新测试 handoff 在验证时返回迁移原因。交给既有 resolver 复核并生成带完整新证据的新交接和新 queue ID。
保留旧 spec、handoff、预算与收据；不改写冻结记录，不通过删除状态重开执行。
