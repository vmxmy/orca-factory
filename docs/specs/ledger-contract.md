# Shell ledger 行为契约

## 0. 冻结范围

本规格冻结 `core/ledger.sh` 的现有可观察行为，供 Go 替换实现使用。它不是功能改进方案。不得把现有 Go 账本的轮转、批量提交、时间窗口、字段校验或锁语义当作 shell 的既有行为。

- 冻结日期：2026-10-08。
- 源码基线：`76a944fbdaaaacb768da3856f5beda59bc3dd810`。
- 权威实现：`core/ledger.sh:1-88`。
- 安装副本：`scripts/agent-gates/ledger.sh:1-88`。本次 `cmp` 确认两份内容相同。
- 第 1–7 节保留冻结时研究证据。S2 实施增量见第 8 节；本次没有访问真实 Linear、改写已有账本或重置预算。
- 下文 `文件:行号` 均相对仓库根目录，行号对应冻结时源码。

**核心结论：账本持久化在 Linear 评论，不在本地 JSONL 文件。追加没有锁和写前去重；查询在内存中去重。一次失败返回不能证明远端没有写入。**

## 1. 调用者清单

### 1.1 搜索范围与计数口径

全仓库搜索覆盖隐藏文件、忽略文件以及 `driver/`、`core/`、`bin/`、`scripts/`、`mods/`、`templates/` 和文档。排除 `.git/` 内部数据。搜索使用 `rg`，并补查泛化入口和 Go 同名实现，避免只搜索字面量 `ledger.sh`。

```sh
rg -n --hidden --no-ignore --glob '!.git/**' --max-columns 200 --max-columns-preview \
  'ledger\.sh|\[ledger\] agent-graph|agent-graph\.sh ledger (append|stats)' .
rg -n --hidden --no-ignore --glob '!.git/**' --max-columns 200 --max-columns-preview \
  '\bledger\b|AGENT_LINEAR_COUNT_FILE|orca-factory-linear-calls' .
```

计数结果：**11 个可执行调用位置，分布在 4 个文件**。其中 2 个是动态分派入口、1 个是生产看板调用、8 个是已有自测调用位置。循环中的调用按源码位置计一次，不按运行次数计数。另有 **8 个明确写出命令的文档调用位置**，单独列出，不与可执行调用位置混算。实现自身、mock 匹配分支、录制数据、历史记录及同名 Go API 不计入 shell 调用者数量。

### 1.2 可执行调用位置

| 编号 | 位置 | 行为 |
| --- | --- | --- |
| C01 | `core/agent-graph.sh:19` | `exec bash "$dir/${1:?script name}.sh" "${@:2}"`；首参数为 `ledger` 时调用同目录 `ledger.sh` |
| C02 | `scripts/agent-gates/agent-graph.sh:19` | 安装后的同一动态分派入口；生产调用走此位置 |
| C03 | `mods/swarm-board/hooks/register.tsx:840` | 执行 `bash scripts/agent-gates/agent-graph.sh ledger stats`，读取文本统计 |
| C04 | `bin/orca-factory:382` | 自测：无账本票时查询 |
| C05 | `bin/orca-factory:385-386` | 自测：追加完整主路径样例 |
| C06 | `bin/orca-factory:391` | 自测：重复评论、非账本评论、坏 JSON 与评论上限警告 |
| C07 | `bin/orca-factory:396` | 自测：空评论集查询 |
| C08 | `bin/orca-factory:399` | 自测：追加坏 JSON 的退出码 |
| C09 | `bin/orca-factory:407-408` | 自测：循环覆盖 `list-issues`、`issue`、`comment`、`save-issue` 失败；`issue` 用 `stats`，其他用 `append` |
| C10 | `bin/orca-factory:417` | 自测：底层 CLI 非零退出时查询 |
| C11 | `bin/orca-factory:1623` | 自测：追加时记录 Linear 调用计数 |

C03 对统计结果缓存 10 分钟；失败或空输出时沿用旧统计，不因刷新按钮清除缓存。依据：`mods/swarm-board/hooks/register.tsx:834-846`。现有 mock 与缓存断言在 `mods/swarm-board/hooks/register.test.ts:479-481,493-497`；mock 不是真实调用位置。

### 1.3 文档驱动的实际调用约定

| 位置 | 使用者与命令 |
| --- | --- |
| `core/constitution/workflow.md:35` | coordinator 收尾执行 `scripts/agent-gates/agent-graph.sh ledger append '<json>'` |
| `scripts/agent-gates/constitution/workflow.md:35` | 上述条款的安装副本 |
| `core/COORDINATOR.md:127` | coordinator 学习步骤执行 `scripts/agent-gates/agent-graph.sh ledger stats` |
| `scripts/agent-gates/COORDINATOR.md:127` | 上述流程的安装副本 |
| `core/roles/intake.md:18` | intake 汇报执行 `scripts/agent-gates/agent-graph.sh ledger stats` |
| `scripts/agent-gates/roles/intake.md:18` | 上述角色的安装副本 |
| `mods/swarm-board/skills/pipeline/SKILL.md:18` | 看板技能执行 `scripts/agent-gates/agent-graph.sh ledger stats` |
| `ONBOARD.md:213` | 接入后人工查看 `scripts/agent-gates/agent-graph.sh ledger stats` |

补充收尾约定：每张已领票各写一条记录，账本失败不阻塞本票收尾；全部已领票收尾后才写一次 `.omc/agent-run-finished`。依据：`core/COORDINATOR.md:118-123` 及同路径安装副本的相同行号。`core/roles/coordinator.md:12,17,40` 及安装副本引用收尾职责，但未增加独立命令调用位置。账本脚本本身不创建结束标记，也不写业务票的失败说明。

### 1.4 分发路径与同名实现边界

- `bin/orca-factory:19,21,31` 定义 `core/` 来源与 `scripts/agent-gates/` 目标；`:50-57,81` 在 init 时复制全部 core 文件；`:103-118` 在 update 时同步。因此迁移须考虑已经分发到其他仓库的入口，不得仅更改本仓库引用。
- `driver/` **没有调用 shell `ledger.sh`**。`driver/cmd/driver/ledger.go:17,49,51,57,59` 注册独立 `driver ledger` 命令并调用 Go 包。这是共享存储格式的关联实现，不是 C01–C11 的 shell 调用者。
- Go 专用票账本：`driver/internal/ledger/ledger.go:157-170,187-245` 缓冲、加进程内 mutex、满 400 评论轮转到带编号标题、把多行放入一个评论；`driver/internal/ledger/stats.go:17-67` 默认按票/评论更新时间选最近 7 天的票，支持逐行解析。shell 不具备这些行为。
- Go 业务票记录：`driver/internal/executor/budget.go:48,64-66,85` 和 `driver/internal/scheduler/merge.go:241,417,423,433` 直接读写当前业务票 `t.ID` 的评论，不写 shell 专用账本票。格式生产者分别为 `driver/internal/executor/run_ledger.go:17-36,57-84`、`driver/internal/merge/ledger.go:15-34,62-95`。它们的写前查询去重不是 shell 的写入契约，也不是跨进程原子去重。
- 看板另有直接 Linear 读取：`mods/swarm-board/hooks/register.tsx:803-822` 从快照或最多 3 张账本票读取评论，支持编号标题；`:805,822,881` 调用格式解析器。`mods/swarm-board/hooks/usage.ts:120-129` 按票取最新用量；`mods/swarm-board/hooks/view-data.ts:4-18` 逐行解析并按键去重。它们不是 shell `stats` 的数据选择规则。
- `README.md:15,28` 是功能/目录说明；`docs/specs/shell-to-driver-convergence.md:8-9,31,53-59` 是迁移范围；`docs/research/coordinator-early-exit.md:51` 是历史只读调查。录制数据与忽略的 `.omc/` 历史记录不是新增可执行调用者。

## 2. 账本路径与环境变量

### 2.1 持久化地址

| 项目 | 当前契约 | 依据 |
| --- | --- | --- |
| 逻辑地址 | `AGENT_LINEAR_TEAM` + `AGENT_LINEAR_PROJECT` 内，标题**精确等于** `[ledger] agent-graph` 的票，其评论 `body` | `core/ledger.sh:36-39,48-54` |
| 发现方式 | 每次操作都执行 `orca linear list-issues --team "$AGENT_LINEAR_TEAM" --project "$AGENT_LINEAR_PROJECT" --query "[ledger] agent-graph" --json` | `core/ledger.sh:34-39` |
| 选择方式 | 按返回顺序过滤精确标题，取第一张票的 `.identifier // .id`；不排序，不选择最新票，不识别 `#2` 等编号标题 | `core/ledger.sh:39` |
| 缺票写入 | 创建精确标题票，状态 `Backlog`；创建请求不传标签，再向该票追加评论 | `core/ledger.sh:47-54` |
| 缺票查询 | stdout 为 `runs=0\n`，退出 0；不创建票 | `core/ledger.sh:57-58` |
| 本地账本文件 | **不存在**。没有账本文件名、目录、文件权限、轮转文件或本地重放队列 | `core/ledger.sh:1-88` |

模糊搜索结果中只“提到”该标题的票必须排除。已有多个精确标题时仅用返回的第一张。发现结果的 `identifier` 缺失/null/false 时回退 `id`；空字符串不回退。如果第一张匹配票无法提供标识，不继续找第二张。脚本不检查 issue-list 的截断标记、归档状态或分页，也不验证字段类型。

### 2.2 环境与入口

| 变量 | 行为 |
| --- | --- |
| `AGENT_LINEAR_TEAM` | 必需且非空。直接执行脚本前必须已导出；缺失/空值由 Bash 参数展开报错并退出 |
| `AGENT_LINEAR_PROJECT` | 必需且非空。规则同上；team 检查先于 project 检查 |
| `AGENT_LINEAR_COUNT_FILE` | 非空时覆盖调用计数文件路径。不是运行账本路径 |
| `TMPDIR` | count 路径未覆盖时，非空 `TMPDIR` 为计数文件目录；未设置/为空用 `/tmp` |
| `PATH` | 查找外部 `orca`、`jq`、`date`、`sleep`；脚本不固定它们的版本和位置 |

依据：`core/ledger.sh:10-11,18-28`。脚本不直接读取 `LINEAR_API_KEY` 或 `AGENT_REPO_PATH`，认证由外部 `orca linear` 负责。

常用入口先读取同目录 `agent-graph.env`，导出配置，并恢复调用者已有的所有 `AGENT_*` 值，因此调用者环境覆盖 profile。然后 `exec bash` 转交参数和退出状态。依据：`core/agent-graph.sh:8-12,19`、`scripts/agent-gates/agent-graph.sh:8-12,19`。当前仓库 team/project 配置位置为 `scripts/agent-gates/agent-graph.env:6-7`；Go 实现不得硬编码这里的项目 ID。

### 2.3 唯一本地写入：调用计数

路径表达式为 `${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}`。每次 API **尝试前**执行追加：

```text
<date +%s 的 Unix 秒> ledger\n
```

字段由一个 ASCII 空格分隔，记录由 LF 分隔；这是纯文本计数，不是 JSON，不含 ticket 或 budget。使用 `>>`，路径可为相对路径；脚本不创建父目录、不加锁、不截断、不轮转、不 fsync。写入失败及其 stderr 被忽略，不改变账本操作结果。依据：`core/ledger.sh:17-20`。

同一文件还被 `core/agent-graph.sh:15-16`、`core/linear-precheck.sh:37`、`core/prune-run-worktrees.sh:29` 及相应安装副本使用。看板消费者在 `mods/swarm-board/hooks/register.tsx:374-401` 读取默认 `TMPDIR` 路径，统计最近 3600 秒；它不在此处读取自定义 `AGENT_LINEAR_COUNT_FILE`。这是既有边界，不应误称覆盖变量在消费者侧也生效。

## 3. 数据格式

### 3.1 评论封装、分隔符与编码

正常评论正文为 `ledger <compact JSON object>`：

- 前缀精确为 7 个 ASCII 字节 `ledger `，末尾为一个空格。不是制表符分隔、不是 CSV。
- `append` 用 `jq -c .` 解析并重编码参数二，再加前缀。保留所有 JSON 字段，不只保留下表字段；空白、数字表示和转义可能被 jq 规范化。
- 正常单对象输入产生一个评论中的一条紧凑 JSON 记录。Bash 命令替换去掉 jq 输出末尾 LF，`--body` 不额外添加末尾 LF。JSON 字符串内部换行由 JSON 转义表达。
- jq 文本输出使用 UTF-8；没有 `-a`，不要求把非 ASCII 字符转成 `\uXXXX`。没有 BOM、压缩或自定义二进制封装。脚本不额外校验原始字节编码，输入容错行为受 jq 影响。
- 查询要求整个评论 `body` 是字符串且从位置 0 开始为 `ledger `。前导空格、`Ledger `、`ledger\t` 均不匹配。去掉前缀后，对**整个剩余正文**做一次 `fromjson`，不按行拆分。

依据：`core/ledger.sh:44-45,54,65-71`。

### 3.2 业务字段与实际校验

下表给出生产者约定，不代表 shell 会校验这些类型。约定依据：`core/ledger.sh:4-6`、`core/constitution/workflow.md:39,56,58`。

| 字段 | 业务约定 | shell 实际使用 |
| --- | --- | --- |
| `ticket` | 票标识字符串；`no_ticket` 时票字段省略 | 与 `started_at` 组成查询去重键 |
| `lane` | 泳道编号 | 非 null 的值参与 lane 分组，无类型限制 |
| `tier` | `fast` / `default` / `strong`；领取时首跑档位，后续升档不改写 | 非 `no_ticket` 行按该值分组；缺失/null/false 用 `unknown` |
| `outcome` | `merged` / `escalated` / `no_ticket` | 精确计数三种结局；其他值仍计入总 runs；精确 `no_ticket` 不参与 tier 分组 |
| `first_gate` | `green` / `red`；coordinator 第一次独立复跑结果 | 精确 `green` 计入通过数；缺失/null/false 不进入百分比分母 |
| `corrections` | correction 次数，含 CI 红与追溯块缺失 | 缺失/null/false 排除后求平均 |
| `ci_reds` | CI 红次数 | 缺失/null/false 按 0 求和 |
| `pr` | PR 编号 | 只持久化，不参与统计 |
| `minutes` | 运行分钟数 | 缺失/null/false 排除后求平均 |
| `started_at` | 运行开始时间 | 只作为原始 JSON 值进入去重键，不解析时间、不规范化时区 |
| `finished_at` | 运行结束时间 | 只持久化，不筛选时间窗口 |
| 其他字段 | 现有 Go 生产者还含 `stage_secs`、`usage` | shell 原样保留其 JSON 值，统计不读取或验证它们 |

追加校验**只有 JSON 能解析且 jq 输出非空**。不要求对象、不要求任何字段、不校验 enum、不拒绝负数、不校验时间格式、不校验数值类型。不读取 stdin 作为记录。多余命令行参数被忽略。

现存不对称行为必须显式保留或另行获得变更授权：

| 输入 | `append` | `stats` |
| --- | --- | --- |
| `{}` | 接受并写 `ledger {}` | 接受为对象；缺失键为 null |
| `null` / `false` / `[]` / JSON 字符串或数字 | 接受 | 非对象，发警告并跳过 |
| 空串、纯空白、缺少参数二 | jq 无有效输出，退出 2；不访问 Linear | 不适用 |
| 非法 JSON | 退出 2；不访问 Linear | 匹配前缀的坏 JSON 评论发警告并跳过 |
| 多个 JSON 文档，如 `{} {}` | jq 产生 `{}\n{}`，写入**一个**评论 `ledger {}\n{}` | 整段 `fromjson` 失败，发警告并跳过；不是两条记录 |
| 错误字段类型但顶层为对象 | 接受 | 不是坏评论过滤条件；最终聚合可能发生 jq 错误并非零退出 |

## 4. 读写操作与输出

### 4.1 `append '<json>'`

1. 在任何 API 调用前，用 jq 校验并紧凑化参数二。
2. 查询账本票，筛选精确标题。
3. 找不到票时创建 `Backlog` 票。从 `.result.issue.identifier // .id // empty` 读取标识；取不到标识则退出 1。
4. 同步执行 `orca linear comment add "$issue" --body "ledger $row" --json`。
5. 成功退出 0，stdout 为空；API 响应被抑制。没有 buffer、后台 flush 或落盘重试队列。

依据：`core/ledger.sh:43-54`。追加不先读评论，不查重复键，不覆盖、不更新、不删除已有评论。每次调用都会尝试新建评论。

### 4.2 `stats`

1. 查询账本票；找不到则输出 `runs=0\n` 并成功退出。
2. 执行 `orca linear issue "$issue" --comments --json`，只使用本次返回的 `.result.comments`。
3. `.result.meta.sections.comments.hasMore == true` 或 `.capReached == true` 时，在 stderr 警告 500 条上限/更多页面；继续统计已有返回，不获取后续页。
4. 只处理匹配前缀的字符串正文。非账本正文静默忽略；匹配前缀但无法解析成对象的正文逐条警告并跳过。
5. 按 `unique_by([.ticket, .started_at])` 在内存中去重。不删除重复远端评论。
6. 空记录数组只输出 `runs=0\n`；否则输出下列顺序的行，每行末尾 LF。

依据：`core/ledger.sh:56-82`。没有按记录日期过滤，没有最近 7 天窗口，没有 `--all` 语义；`stats --all` 只是忽略额外参数。

```text
runs=<N> merged=<N> escalated=<N> no_ticket=<N>
avg_corrections=<number> ci_reds=<number> avg_minutes=<number>
lane=<value> runs=<N> first_gate_green_pct=<integer>
tier=<value> runs=<N> first_gate_green_pct=<integer>
```

- 所有匹配且去重后的对象均计入 `runs`，包括缺少/未知 outcome 的对象。
- 平均值按 jq 的 `(add / length * 100 | round) / 100` 计算，空输入为 0。数字按 jq 插值显示，不固定输出两位小数。
- `ci_reds` 是默认 0 后的 jq `add`，不是平均值。
- 每个 lane 一行，然后每个 tier 一行。分组顺序采用 jq `group_by` 的 JSON 值排序，不按到达顺序或字符串化后字典序自行重排。lane 只排除 null；false lane 仍可成组。
- tier 排除精确 `outcome == "no_ticket"` 的行；tier 的 `// "unknown"` 对缺失、null、false 生效，空字符串不替换。
- 百分比以该组 `.first_gate // empty` 的数量为分母。任何非 null/false 值进入分母，只有字符串 `green` 进入分子；分母为 0 时输出 0；结果用 jq `round` 取整。
- 去重比较的是 JSON 值，不是解析后的时间。缺失与显式 null 的键等价；两个无票、无开始时间的对象可能同属 `[null,null]`。相同键冲突时遵循 jq `unique_by` 行为；本机 jq 1.8.1 的探针保留输入中的第一条，不合并字段，也不按 `finished_at` 选最新。
- 最终聚合的类型错误不在坏评论的 `try/catch` 内。可能已经输出总计行后才报错；非零退出时 stdout 不保证为空。

## 5. 退出码与错误处理

### 5.1 命令结果

| 情况 | 退出状态与输出 | 依据 |
| --- | --- | --- |
| append 成功 | 0；正常 stdout 为空 | `core/ledger.sh:54` |
| stats 成功、无票或无有效记录 | 0；`runs=0\n` | `core/ledger.sh:58,72` |
| stats 成功、有记录 | 0；第 4.2 节文本；警告可同时出现在 stderr | `core/ledger.sh:62-82` |
| JSON 无效或输出为空 | 2；stderr 含 `ledger: invalid json row`；非法语法还可能有 jq 原始诊断 | `core/ledger.sh:44-45` |
| 无子命令或不支持的子命令 | 环境检查通过后返回 2；stderr 含 `usage: ledger.sh append '<json>' \| ledger.sh stats (Linear comment storage)` | `core/ledger.sh:84-86` |
| team/project 未设置或为空 | Bash 参数展开错误，当前 Bash 探针退出 1；错误包含相应 required 文本；**优先于 usage/JSON 校验** | `core/ledger.sh:10-11` |
| API 重试耗尽 | 1；逐次失败诊断加最终 aborted 诊断；不是底层 CLI 原始退出码 | `core/ledger.sh:13-31` |
| 创建响应无有效 issue 标识 | 1；stderr 含 `ledger: save-issue returned no ledger issue identifier`；不追加评论 | `core/ledger.sh:50-52` |
| API 外的 jq 结构/聚合错误 | `set -e` 终止，保留该命令非零状态和诊断；不统一转换成 1，也不当作空账本 | `core/ledger.sh:8,39,65-82` |
| count 文件不可写 | 单独忽略；API 继续运行 | `core/ledger.sh:17-20` |

### 5.2 外部调用与重试

全部四类调用 `list-issues`、`save-issue`、`comment add`、`issue` 共用 `linear()`：

- 每个 API 调用最多 3 次尝试；两次间隔固定 `sleep 2`。成功立即返回；最后一次失败后不再 sleep。
- `sleep` 非零的行为取决于原 Bash 调用上下文：发现 `list-issues` 位于 `|| return 1` 条件中，创建 `save-issue` 与查询 `issue` 位于未继承 errexit 的命令替换中，均继续下一次尝试；顶层 `comment add` 的 sleep 失败立即按 sleep 状态退出。隔离探针中第一次 API `ok:false`、sleep 退出 7、第二次 API 成功时，前三条路径最终退出 0，comment 路径退出 7，不执行第二次 API。Go 保留此差异，不统一忽略或终止 sleep 失败。
- 成功条件必须同时满足：`orca` 退出 0，响应能由 jq 解析并满足 `.ok == true`。退出 0 但 `ok:false` 仍是失败；退出非零但响应 `ok:true` 也失败。
- 每次尝试均记录 count，包括失败和重试。count 文件失败不影响尝试。
- `orca` stdout 被捕获；stderr 不被捕获，会直接到调用者 stderr。
- 失败原因取 `jq -c '.error // .message // .'`；解析失败回退原始 stdout，空值显示 `empty response`。
- 每次失败打印 `ledger: orca linear <首个子命令> failed (attempt <n>/3, exit <status>): <reason>`。
- 重试耗尽打印 `ledger: orca linear <首个子命令> failed after 3 attempts; ledger operation aborted`，返回 1。
- wrapper 只检查 envelope 的 `ok`，不校验 `comment add` 成功响应中的 comment ID；后续 jq 才读取发现/创建/查询 payload。
- 没有脚本级超时、截止时间、退避增长、取消 API 或信号清理逻辑。依赖底层 CLI 和进程生命周期。

依据：`core/ledger.sh:13-31`。无重试时，append 有票需 2 次 API，无票需 3 次；stats 无票需 1 次，有票需 2 次。四类 API 都会重试，不仅只读操作。

### 5.3 调用者失败策略

脚本返回非零并中止当前账本操作。它不负责收尾容错。coordinator 依据 `core/constitution/workflow.md:34-38` 及安装副本，在业务票评论记录账本失败原因，无票时只写 stderr，然后继续写结束标记。看板依据第 1.2 节使用上一次有效统计。Go 替换不能把 API 失败变成 `runs=0` 来掩盖错误，也不能让本次账本失败重置既有运行预算。

## 6. 并发、锁与原子性

| 边界 | 当前保证 | 当前不保证 |
| --- | --- | --- |
| 同一次脚本执行 | validate → discover → 可选 create → comment 的同步顺序；stats 等待读取完成后处理 | 整个操作不是事务 |
| 账本票发现/创建 | 每次使用精确标题搜索 | 无 `flock`、mutex、CAS、唯一约束或二次确认；两个无票 append 可同时创建两张同标题票 |
| 评论追加 | 不读改写已有正文；每次向 Orca 提交一个完整 body | 客户端没有幂等键和写前去重；远端已成功但响应丢失时，重试可产生重复评论 |
| 不确定写入 | 重试失败后返回错误 | 非零退出不证明“未写入”；没有写后查询确认，也没有回滚 |
| stats 与 append 并行 | stats 统计其 API 响应中可见的评论 | 无共享锁、读屏障或跨 API 快照；不保证看到刚成功的 append，也不保证评论排序 |
| stats 去重 | 本次读取内按键去重 | 不合并多个账本票、不读取未返回分页、不修复远端重复、不实现 exactly-once |
| 本地调用计数 | `>>` 追加，失败忽略 | 无锁、记录级原子性证明、fsync 或可靠计量保证；不能当作业务账本或预算存储 |

依据：完整 `core/ledger.sh:8-82` 未调用锁/事务原语；追加路径在 `:46-54`，重试路径在 `:13-31`，内存去重在 `:70`。

Linear/Orca 服务自身是否保证单次 comment 写入的原子性不由此脚本证明。此规格只冻结客户端行为，不把远端未知保证升级为客户端承诺。已经存在的两张同标题票可能使不同调用返回不同首票，从而分散记录；后续 `stats` 仍只读当次选中的一张票。

## 7. Go 替换的验收边界与已有证据

替换必须保持既有调用入口/参数转发、team/project 地址、精确标题与首票选择、评论前缀、未知字段、输入宽容度、stdout 格式、去重键、缺票只读行为、警告、错误退出与 count 副作用。不能静默引入对象字段强校验、7 天窗口、编号票轮转、按行读取批量评论、异步成功返回或新的远端存储地址。这些均是行为变更，需要另行授权。

并发限制也是规格的一部分：当前不承诺 exactly-once、不承诺创建唯一票、不承诺完整统计。Go 替换不得降低已有同步写入保证，也不得声称当前实现提供不存在的锁或原子性。新增并发保护如影响跨调用行为，应作为显式变更处理。

本次实际检查：

- 全仓库 `rg` 搜索与调用点源码读取，覆盖四个指定目录并包含隐藏/忽略文件。
- `cmp core/ledger.sh scripts/agent-gates/ledger.sh` 成功。
- 在会话 scratchpad 中提取并运行**已有** `bin/orca-factory:342-422` 账本自测片段。使用原始 `core/ledger.sh` 的隔离副本及已有 fake `orca`/`sleep`，未调用真实 Linear。结果：`selftest: Linear ledger reports tier, deduplicates, warns, handles empty stats and retries failures`，退出 0。
- 在相同 fake 环境直接运行原始脚本探针，确认 `null`、`false`、多 JSON 文档的 append 均退出 0；查询跳过这些非对象/不可解析评论；额外参数被忽略；缺失环境优先于 usage，退出 1。
- jq 1.8.1 探针确认相同 `(ticket, started_at)` 的冲突对象保留第一条输入记录。

冻结研究阶段未运行完整 `bin/orca-factory selftest`、Go Make 入口或真实并发/远端写入实验。S2 的本地验证见第 8 节；真实服务的原子性与并发结果仍未验证。

## 8. S2 实施增量（2026-10-08）

- 新契约入口为 `driver ledger-shell append/stats`；无配置、无网络的 `ledger-shell --contract-version` 输出 `ledger-shell-v1`。原 `driver ledger` 不变。
- Go 接管命令分派、发现/创建/追加/查询、三次尝试、固定 sleep、尝试前 count 及 PATH 中 Orca 的进程执行。保持 Orca stderr 直传，不使用共享 runx 的固定路径、超时或诊断捕获。相对 PATH（包括 `.`）按既有 shell 契约查找可执行文件；只在此进程边界处理 Go `exec.ErrDot`，不修改全局 GODEBUG 或共享 runx。
- JSON/统计表达式仍交给 PATH 中现有 jq。复用 `ledger.Title`，不复用不等价的 Go stats helper。任意 JSON 文档、未知字段、整个评论解析、jq JSON 排序、jq 错误状态及聚合失败前 stdout 因此保持；没有实现通用 jq 解释器，也没有执行旧 shell。此交付是 shell 编排迁入 Go，不是纯 Go 或外部依赖清零。
- `core/ledger.sh` 仅保留 team/project 检查、driver 定位、契约探测及 exec。`AGENT_DRIVER_BIN` 是可选可执行路径，默认 `orca-factory-driver`；原 `agent-graph.sh` 的环境覆盖和动态分派不变。缺失/不兼容 driver 退出 1，不回退旧实现。
- `bin/orca-factory init/update` 在任何 core 写入前检查 driver 及 ledger 入口。识别冻结原始 blob `b34287c3b5e5fbdd6cd03e5a26e2a8572a7fe1a0` 与当前源码；拒绝自定义/未知/符号链接。暂存到目标同目录，明确设置 `0755` 可读可执行权限及语法检查成功后 rename；失败保留原入口。原子性只覆盖 ledger，不涉及 Linear 或运行状态。
- 新公开 CLI 行为断言覆盖多 JSON/非对象、未知字段、首票与去重、数组排序、警告及 count；关键失败断言覆盖坏字段类型的原始 jq 退出 5/部分 stdout、非法 JSON 的退出 2、Orca stderr 与 exit 7 重试、jq 边界失败。安装断言覆盖缺失/不兼容 driver、自定义/符号链接冲突、rename 失败及原版/当前版成功切换。均先红后绿；这些是合成隔离测试，不冒充录制样本。
- 审查修正：原始 shell 使用版本管理夹具 `driver/testdata/ledger-shell/original-ledger.sh`，来源 SHA、Git blob 和 SHA-256 见相邻 provenance JSON；这是历史行为测试夹具，不是新生产 fallback。安装测试不再读取旧 Git 对象，在无 `.git`/旧对象的源码副本先复现 exit 128、修正后聚焦 race 通过。安装权限、相对 PATH 和 sleep 上下文断言均先红后绿；本批聚焦 test/race 各 3 tests、退出 0，未重复完整 lint/selftest 或重型 gate。
- 完整 selftest 使用新构建的 driver 与原有 fake Orca/sleep，实际通过既有账本、自接入、gate、插件 validate/test、计数等回归；没有真实 Linear 写入。
- 主工作区开发安装副本已通过显式隔离候选 driver 的 `bin/orca-factory update .` 同步，仅 ledger 内容改变（`changed=1 added=0 unchanged=18`）；`check` 与安装入口契约探测均退出 0，源码与副本一致。候选 driver 位于 `/tmp/claude-1000/-home-laoxu-github-orca-factory/79717a69-f186-41dc-9e91-0049c99bec51/scratchpad/s2-integration-driver`，来源源码与当前主工作区 driver 文件字节一致。
- 代码合并与生产/客户发布分开：开发分支使用显式候选 driver 验收，不要求先覆盖日常 PATH 的旧 driver；客户部署不是代码合并前置条件。正式 `/home/laoxu/.local/bin/orca-factory-driver` 保持原字节，尚不支持该契约，须在合并后配套更新。没有重启服务、扫描或修改其他客户仓库；客户目标清单、逐目标配套检查、状态保留及发布结果继续作为真实发布待办。
