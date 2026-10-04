# orca-factory 与 Orca 高级功能集成：调研研究方案

调研基线：本机 Orca `1.4.218`（`orca --version`）[1]；官方源码取 tag `v1.4.218`，与本机版本一致[21]；官网文档页无版本号[6]-[19]；GitHub 最新发布为 `v1.4.220`（2026-10-04），本机落后两个小版本[20]。

证据强度标签（表内使用）：

| 标签 | 含义 |
| --- | --- |
| 本机 | 本机 `--help`、`skills get`、`agent-context` 或只读查询确认。与版本匹配，优先级最高。 |
| 源码 | `stablyai/orca` 的 `v1.4.218` 源码确认。 |
| 文档 | 仅官网文档写了，未被本机或源码交叉确认。 |
| 推测 | 由以上证据推断，未验证。 |

文档与本机不一致时，以本机为准，并在正文指出。

## 1. 结论摘要

先做的三项，按「证据强度 × 收益 × 改动量」排序：

1. **worktree checkpoints + `orca worktree ps` 作为 swarm-board 的数据源。** `worktree set --linear-issue <ID> --comment <阶段> --workspace-status in-progress|in-review` 把票进度写回 Orca 卡片；`worktree ps --json` 已返回 `workspaceStatus`、`comment`、`linkedLinearIssue`、`linkedPR`、`liveTerminalCount`、`lastActivityAt`[5]，可替换 swarm-board 里「`terminal list` + 用 `objective` 字符串匹配 Run」的做法（`mods/swarm-board/hooks/register.tsx:235-240`）。命令稳定、不需要新基础设施。
2. **编排可靠性补强，而不是「换成编排」。** 流水线已经在用 `run-create / task-create / worker-start / check --wait / worker_done / escalation / ask / worker-list`。缺口是：`--retry-request` 与 `request-show`（`workflow.md §5` 目前对 `runtime_unavailable` 盲重试，可能重复建 Task 或重复起 worker）、收尾时 `worker-list --terminal-state reclaimable` 的结算核对、`worker-show` 的 `observation.agentWait`（识别等人类输入的工人，转人）。这些都在本机版本匹配的 skill 里写明[3]。
3. **自动化与仓库配置卫生。** 修复两个定时任务的配置漂移；评估升到 `1.4.220`（`v1.4.219` 修了编排工人与自动化卡在文件夹信任提示的问题[20]）；用 `orca.yaml` 的 `scripts.setup` 与 `setupAgentStartupPolicy: wait-for-setup` 取代「靠 UI 设置 setup 脚本」的接入步骤（源码支持，文档未写，且有信任确认的未知项，见 §5、§6）。

不在前三，但收益最高、必须先实验的一项：**用 `worker-start --on <environment>` 把工人放到隔离机器**，缓解凭证暴露[3][15]。它会打破「coordinator 在本机路径上复跑 gate 并提交」的现有模型，见 C8。

不建议现在做：`--reuse-session`（与「一次运行一个新 worktree + 票据是数据」的隔离假设冲突）、`gate-create` 作为「需要人」通道（gate 由 coordinator 自己 resolve，不是异步人类审批）、通知推送（Orca 没有 CLI 或 agent 可调用的通知接口）。

## 2. 现状清单：流水线今天用了 Orca 的哪些能力

提取方法：`grep` 仓库内所有 `orca <子命令>` 调用。`scripts/agent-gates/` 是 `core/` 的安装副本，不重复列出。

| 能力 | 调用位置（文件:行） | 作用 |
| --- | --- | --- |
| `orca status --json` | `core/install.sh:13`；`ONBOARD.md:15` | 确认 Orca 在运行 |
| `orca repo list` / `repo add` | `ONBOARD.md:16` | 确认仓库已注册 |
| `orca agent-context` | `ONBOARD.md:53` | 查可用 agent id |
| `orca automations list/create/edit/run` | `core/install.sh:47-55`；`ONBOARD.md:166-167` | 创建（禁用态）、更新、手动触发、启用定时任务 |
| `automations` 选项 `--trigger '*/5 * * * *' --timezone --provider claude --repo path: --workspace-mode new-per-run --precheck --precheck-timeout 180 --missed-run-grace-minutes 10 --prompt --disabled` | `core/install.sh:49-55` | 每 5 分钟前置扫描后才启动 coordinator |
| `orca automations list` / `runs --id` | `mods/swarm-board/hooks/register.tsx:217,219` | 看板读自动化与运行历史 |
| `orca skills get orchestration --full` | `core/install.sh:39`（写进定时任务 prompt） | coordinator 每次运行先加载编排协议 |
| `orca orchestration run-create` | `core/COORDINATOR.md:23` | 每票一个 Run |
| `orca orchestration task-create`（`--deps`、`--parent`） | `core/COORDINATOR.md:25`；`core/constitution/handoffs.md:46`；`core/roles/README.md:22` | 每单元一个 Task |
| `orca orchestration worker-start --worktree new-top-level --repo --base-branch --name --agent --model --effort` | `core/COORDINATOR.md:28-30`；`core/constitution/workflow.md:7` | 在独立 worktree 起 Codex 工人；回执取 `launch.effective`（`workflow.md:54`） |
| `worker-start --worktree path:<wt>`（correction） | `core/constitution/handoffs.md:47` | 打回时复用原 worktree |
| `orchestration check --run --types worker_done,escalation,question --wait --timeout-ms 1800000 --ack` | `core/constitution/handoffs.md:63` | coordinator 等待与确认交付 |
| `orchestration check --terminal <handle>` | `core/roles/worker.md:27` | 工人读 coordinator 追加指令 |
| `orchestration send --type worker_done / escalation`（含 `--outcome --files-modified --task-id --dispatch-id --dispatch-capability`） | `core/constitution/handoffs.md:8,56` | 工人完成与升级信号 |
| `orchestration ask` / `reply` | `core/constitution/handoffs.md:57` | 单一事实问题 |
| `orchestration task-update --status failed --result` | `core/constitution/handoffs.md:45,48` | correction 流程里标记父 Task |
| `orchestration worker-list --run`（读 `projection.nextAction`） | `core/constitution/handoffs.md:64` | 连续 3 次空等后的排查 |
| `orchestration worker-release --dispatch` | `core/COORDINATOR.md:61` | 成功收尾时释放工人终端 |
| `orchestration run-list` / `task-list --run` / `terminal list` | `mods/swarm-board/hooks/register.tsx:235,240,250` | 看板拼装在途状态 |
| `orca worktree list --repo` / `worktree rm --worktree path: [--force]` | `core/prune-run-worktrees.sh:14,19`；`core/constitution/workflow.md:10`；`core/COORDINATOR.md:61` | 清理已结束运行与已合并单元的 worktree |
| `orca linear team list/labels/states`、`project list` | `core/install.sh:29,33`；`ONBOARD.md:64-67` | 接入时校验 Linear id |
| `orca linear list-issues / issue [--relations] [--comments] / relation add / status set / label add|remove / comment add / save-issue / estimate` | `core/linear-precheck.sh:37,47,58,66-68`；`core/COORDINATOR.md:9-10,61`；`core/roles/intake.md:12-19,37`；`mods/swarm-board/hooks/register.tsx:195,257` | 领票、记账、转人、建票 |

未使用（本机 `orca --help` 有，流水线没碰）：`worktree set`（checkpoints）、`worktree ps`、`worktree create`、`gate-create/resolve/list`、`send --to dispatch:<id>`、`worker-read`、`worker-show`、`worker-stop/abandon/retain`、`request-show` 与 `--retry-request`、`task-list --ready --brief`、`agent hooks`、`environment *`、`serve`、`skills install/share`、`search`[1][2]。

流水线共调用约 20 个 orca 子命令。`coordinator-start`、`coordinator-stop`、`orchestration run` 已退役、无效果[6]；本仓库没有调用它们（`grep` 无命中）。

## 3. Orca 高级功能清单

稳定性取自文档标注；文档没有标注的写「未核实」。官网 `/docs/cli/orchestration` 称编排为「Experimental」，但 `/docs/settings` 的实验功能列表没有编排开关[6][17]。本机 `orca --help` 无门控，且流水线已在运行，所以编排对本机可用。

| 功能 | 稳定性 | 一句话作用 | 证据 | 来源 |
| --- | --- | --- | --- | --- |
| 编排 Run/Task/Dispatch/消息/决策门 | 实验（文档）；本机可用 | 记录谁拥有任务、哪次尝试有效、何时结算 | 本机 + 文档 | [3][6] |
| `worker-start --on <environment>` | 实验；远程服务器为 Beta | 工人在已配对的远程 Orca 服务器执行；Run 仍在本机 | 本机 + 文档 | [1][3][15] |
| 定时自动化（`--precheck`、`--precheck-timeout`、`--missed-run-grace-minutes`、`--reuse-session`） | 未核实（文档无标注） | cron/RRULE 触发 agent；前置探针非 0 则记 `skipped_precheck` | 本机 + 源码 | [1][7][21] |
| worktree checkpoints（`worktree set --comment --workspace-status --linear-issue`） | 未核实（文档无标注） | 把阶段与卡片状态写进 worktree 元数据 | 本机 + 文档 | [1][4][8] |
| `orca worktree ps` | 未核实 | 跨 worktree 的紧凑编排摘要，含状态、评论、PR、终端数 | 本机 | [1][5] |
| Agent hibernation | 实验，默认关 | 暂停空闲后台 agent，重开 worktree 自动 resume | 文档 | [9][17] |
| Agent 状态 hooks（`orca agent hooks on/off/status`） | 未核实 | 把 working/waiting/done 报给 Orca；供 `worktree ps`、`worker-list` 使用 | 本机 + 源码 + 文档 | [5][10][21] |
| `worker-show` 的 `observation.agentWait` | 未核实 | 标出卡在「只有人能答」的提示上的工人，证据源为 hook、提示文本或标题 | 本机 | [1] |
| Agents feed（`/docs/activity`）/ Activity Page | 实验（设置页列为实验）；文档称「默认开」 | 跨 worktree 的 agent 事件线程 | 文档 | [11][17] |
| Agent Dashboard | 实验 | 「Needs You / Working / Done」看板 | 文档 | [17] |
| 通知与收件箱 | 未核实 | agent 完成时的系统通知、声音、worktree 角标；没有 CLI 或 agent 触发接口 | 文档 | [12] |
| Skills registry（`skills install/update/share/installed/get/list`） | 未核实 | 把 Orca 自带 skill 装进 agent；`share` 发布不公开链接（需账号与默认关的权限） | 本机 + 文档 | [1][13] |
| MCP 服务器 | 未核实 | Settings → Integrations → MCP 注册外部工具 | 文档 | [13][17] |
| SSH worktree | 未核实 | 在 SSH 目标上建 worktree 和跑 agent；断线保活默认 5 分钟（可配） | 文档 | [14] |
| 远程 Orca 服务器 / `orca serve` / `environment add` | Beta（文档） | 另一台机器拥有整个运行时；凭证用服务器的 | 本机 + 文档 | [1][15] |
| `orca.yaml`：`worktree.sharedDirectories`、`.worktreeinclude` | 未核实（文档有写） | 共享（symlink）大目录、复制被忽略的 `.env` | 文档 + 源码 | [16][21] |
| `orca.yaml`：`scripts.setup`、`scripts.archive`、`setupAgentStartupPolicy`、`defaultTabs`、`issueCommand`、`environmentRecipes` | 未核实（文档页未写） | 提交进 git 的 setup/归档脚本与启动策略 | 源码 | [21] |
| 每工作区环境配方（`environmentRecipes`、`orca vm recipe doctor`） | 实验（设置页的 Cloud VM） | 为每个工作区按需建一次性环境 | 本机 skill + 文档 | [5][17] |
| `orca agent-context` | 未核实 | 机器可读命令 schema（本机 239 条命令） | 本机 | [2] |
| `orca skills get orchestration --full` 与 `--reference` | 未核实 | 与本机版本匹配的完整指南；紧凑版 13 KB，完整版 42 KB（本机实测） | 本机 | [3] |
| Keep computer awake（Settings → Agents） | 未核实 | 防休眠，模式「On / Agent / Off」 | 文档 | [17] |
| `orca search` | 未核实 | 全文搜索 agent 会话 | 本机 | [1] |

## 4. 差距与候选集成

优先级：P0 = 立即核对；P1 = 先做；P2 = 实验后再定；P3 = 等 Orca 稳定或收益小。

### 4.1 立即需要核对的问题（流水线现状与 Orca 当前行为的冲突）

| 编号 | 问题 | 证据 | 影响 | 核对方式 |
| --- | --- | --- | --- | --- |
| I1 | 两个定时任务配置漂移。`agent-linear-scan (orca-factory)` 的 `precheckTimeout=60`、`missedRunGraceMinutes=720`；`agent-linear-scan (zhiyun-ai-dashboard)` 为 `180 / 10`。`core/install.sh:49-50`（更新路径）设 `--precheck-timeout 180`，但从不设 grace；`:55`（创建路径）才设 grace，且不检查结果（`:50` 检查了 `.ok == true`，`:55` 没有）。 | 本机只读查询[5]；`core/install.sh:49-55`；默认超时 60、上限 600 见 `src/shared/automation-precheck.ts:3-4`[21] | 本机实测 orca-factory 的 precheck 耗时 10-17 秒（5 条运行记录[5]），目前没有超时。但 `linear-precheck.sh` 每张票多次调用 `orca linear`（含 3 次重试、间隔 2 秒，`core/linear-precheck.sh:25-33`），票多时会超过 60 秒，超时按「跳过」记录，表现为静默不领票。 | `orca automations show <id> --json` 对比两个任务；`orca automations runs --id <id> --json` 看 `skipped_precheck` 的 `error` 是否含 `timed out`。默认 grace 值未核实：源码中没有常量，`720` 只出现在测试夹具里，推测为默认值。 |
| I2 | `handoffs.md:48` 要求 correction 通过后执行 `task-update --id <task> --status completed`。本机 skill 写明有效 `worker_done` 自动结算 Task，「不要再跟 `task-update --status completed`」。 | `core/constitution/handoffs.md:45-48`；`orchestration` skill「Completion accounting」[3] | correction 流程是对父 Task 手动改状态，可能合法，也可能与自动结算冲突。 | 见实验 E2 的第 4 步：在沙箱里走一遍 correction，读 `task-list` 状态。 |
| I3 | `core/COORDINATOR.md:61` 只在「成功收尾」释放工人终端。转人（`§7`）与 correction 路径没有 `worker-release` / `worker-retain` / 复用的决定。skill 要求每个已结算的工人终端有下一个归属，并要求「`worker-list --run <run> --terminal-state reclaimable` 返回空之前不结束 coordinator 回合」。 | `core/COORDINATOR.md:61-65`；`orchestration` skill[3] | 终端泄漏；与 `prune-run-worktrees.sh:5` 的注释（终端存活不是信号）一致，但会占 Orca 资源。 | E2 的第 5 步：收尾后读 `worker-list --terminal-state reclaimable`。 |
| I4 | `ONBOARD.md:169` 写「`orca repo` 没有设置 hooks 的子命令，Settings → Repository → Hooks 只能在 UI 里配」。本机 CLI 确实没有 hooks 子命令，但 `orca.yaml` 在 `v1.4.218` 支持 `scripts.setup`、`scripts.archive`、`setupAgentStartupPolicy`、`defaultTabs`。 | `ONBOARD.md:169`；`src/shared/orca-yaml-hook-types.ts`[21]；官网只写了 `sharedDirectories`[16] | 接入手册遗漏了可进 git 的 setup 脚本；但提交的 setup 脚本在 UI 里有内容哈希信任确认（`ensure-hooks-confirmed.ts`），无人值守时的行为未核实（见 R9）。 | 实验 E4。 |
| I5 | 官网 `/docs/cli/orchestration` 说编排须在 Settings → Experimental 开启；`/docs/settings` 的实验功能列表无编排；`/docs/cli/reference` 说 CLI 注册在「Settings -> Experimental -> CLI」。本机 `--help` 无门控。 | [6][17][18][1] | 文档内部不一致，以本机为准。无需改流水线，但接入手册若引用文档说法要避免。 | 无需实验；在 ONBOARD 里不写门控。 |
| I6 | 本机 `1.4.218`，最新 `1.4.220`。`v1.4.219` 的发布说明称：「编排工人、自动化和手机启动的 agent 不再卡在文件夹信任提示」「`worker-abandon` 现在能结算卡住的工人，并记录操作者」「工人完成报告在会话压缩后不再被拒绝」[20]。 | [20]（页面被截断，完整说明未核实） | 无人值守运行最怕卡在人类提示上，这三条直接相关。 | 读 `v1.4.219`、`v1.4.220` 完整发布说明后，在非生产仓库升级并跑 E2。 |
| I7 | `core/install.sh:39` 的 prompt 让 coordinator 每次运行加载 `orchestration --full`（42 KB）。skill 自称紧凑版足够本地循环（13 KB），其余按 `--reference references/<file>.md` 按需取。 | 本机实测字节数；`orchestration` skill「Conditional references」[3] | 每次领票运行多读约 29 KB 上下文，且其中包含 Windows、legacy 迁移等本流水线不用的内容。 | 把 prompt 改为紧凑版，对比一次运行的行为；这是低风险改动，但改 `core/install.sh` 属于 lane 2 流水线本体，需走 PR。 |

### 4.2 候选集成总表

| 编号 | 候选 | 解决的痛点 | 依赖的 Orca 功能 | 预期收益 | 风险与未知 | 优先级 |
| --- | --- | --- | --- | --- | --- | --- |
| C1 | **worktree checkpoints 写回 + `worktree ps` 作看板数据源**。`worker-start` 后 `worktree set --worktree path:<wt> --linear-issue <ID> --comment "<阶段>" --workspace-status in-progress`；gate 绿后 `in-review`；合并后 `completed`。swarm-board 改读 `worktree ps --json`。 | 在途状态散在 Linear 评论、`terminal list`、`run-list`；看板用 `objective` 字符串匹配 Run（`register.tsx:236`），脆弱。人在 Orca 里看不到票进度。 | `worktree set`、`worktree ps`、`worktree show`[1][4][8] | 本机、人和看板共用一份进度；删掉字符串匹配；每次 `worktree ps` 一次调用取多 worktree。 | `--comment` 整体替换，须先读（文档要求「read first」）[8]；`--linear-issue` 是否会反向同步 Linear 状态：未核实；`worktree ps` 的行数上限与多主机范围（help 说明 `scope:` 行）[1]；评论长度与频率限制：文档未写。 | P1 |
| C2 | **编排幂等：`--retry-request` + `request-show`** 取代 `workflow.md §5` 的盲重试。 | `runtime_unavailable` 时重试 `task-create` / `worker-start` 可能重复建 Task 或重复起工人；`workflow.md:40-42` 只说「隔几秒重试，最多 3 次」。 | 每个编排 mutation 的 `--retry-request <id>`；`orchestration request-show --request <id>`[1][3] | 消除重复派工；把「响应丢失」从转人变成可恢复。 | `linear *` 命令没有这个机制（但标签、评论重复无害）；`request-show` 返回 `absent` 的语义是「无记录」，不等于没发生[3]；需要 coordinator 生成并持久化 id（放 `.omc/tmp/<ID>/`）。 | P1 |
| C3 | **收尾结算与异常识别**：收尾前 `worker-list --run <run> --terminal-state reclaimable`；等待超时后用 `worker-show` 的 `agentWait` 识别「等人」；`worker-read --source auto` 取证据写进转人评论；correction 时决定复用终端还是释放。 | I3；`handoffs.md:64` 只在连续 3 次空等后看 `nextAction`，没有利用 `agentWait`；转人评论里的证据靠 coordinator 自己读 worktree。 | `worker-list`、`worker-show`、`worker-read`、`worker-release`、`worker-retain`[1][3] | 终端不泄漏；「等人」的工人早转人，而不是占满 30 分钟等待；转人评论带转录证据。 | `agentWait` 为 `null` 与字段缺失含义不同（缺失 = 没看）[1]；`unverifiable` 不授权停止或重试[3]；`worker-read` 读转录的隐私与体量未核实。 | P1 |
| C4 | **自动化卫生**：I1 的漂移修复；`install.sh` 更新路径也设 grace 并检查结果；记录 precheck 语义（见 §6 E6）；评估 Keep computer awake。 | I1；`skipped_precheck` 每个 tick 写一条运行记录（5 条里 4 条），长期噪声；机器休眠时定时任务不跑（文档未写休眠行为）[7]。 | `automations edit --precheck-timeout --missed-run-grace-minutes`；Settings → Agents → Keep computer awake[7][17] | 减少静默漏领；明确休眠行为。 | 休眠与关闭应用时的错过运行语义：文档未写；源码里 `missedBeyondGrace` 按「到期后经过的时间 > grace + 2×tick」判定[21]，默认 grace 值未核实。运行记录的保留与合并策略（`automation-skip-coalescing`、`automation-run-retention`）未读。 | P1 |
| C5 | **仓库配置：`orca.yaml` + `.worktreeinclude`**：`scripts.setup`（装依赖）、`setupAgentStartupPolicy: wait-for-setup`（工人等依赖装完再收 prompt）、`worktree.sharedDirectories`、`.worktreeinclude`。 | 新 worktree 没依赖，工人跑不了测试；`ONBOARD.md:169` 把 setup 留给用户在 UI 里配，且 `start-immediately`（见 help）会让工人和 setup 同时跑。 | `orca.yaml`；`worker-start --setup run`；Settings → Repository → Hooks[1][16][21] | 接入一次到位，进 git 可审阅；工人拿到可用的 worktree。 | 提交的 setup 脚本在 UI 里要内容哈希信任确认，改一次脚本就再确认一次，无人值守的行为未核实；`sharedDirectories` 用 symlink，并行工人共享同一 `node_modules`，有互相覆盖风险；文档只写了 `sharedDirectories` 和 `.worktreeinclude`，其余键靠源码，可能随版本变；脚本失败语义未核实。 | P1（实验后） |
| C6 | **hibernation 与无人值守相容性**。 | 工人和 coordinator 终端是常驻 PTY，占内存。 | `Settings → Experimental → Agent hibernation`[9] | 降内存。 | 文档条件：存在未结算的 Dispatch（`pending`/`dispatched`/未知）时不休眠，结算后允许；默认 30 分钟；非 resumable 的 agent 不休眠[9]。coordinator 在 `check --wait` 中是 working，不应休眠；但已结算未释放的工人 30 分钟后会休眠，resume 失败时回到新 prompt。文档未提自动化与无头运行。收益小于风险时不开。 | P3 |
| C7 | **Agent hooks 与状态**：不自建 hook，读 Orca 已有状态。 | 想知道工人 working/waiting/done。 | `worker-list` 的 `projection.liveness/attention/nextAction`；`worktree ps`；`agent hooks status`[3][5] | 本机已装 hooks（claude、codex、amp、grok、copilot(partial)、hermes、kimi、zcode）[5]；Codex 工人状态可用。 | 状态陈旧阈值：`AGENT_STATUS_STALE_AFTER_MS = 30 分钟`（源码）[21]；心跳每 5 分钟（`preamble.ts:44`）；协调器「陈旧心跳阈值 10 分钟」仅见于源码注释，未见强制代码，按推测处理。`DISPATCH_STALE_THRESHOLD = 20`（`coordinator-stale-base-flag.ts:4`）与心跳无关，不要混用。 | P2（并入 C3） |
| C8 | **SSH / 远程服务器隔离工人**：`worker-start --on <environment> --worktree new-top-level --repo <exact remote selector>`；服务器上只登录受限账号。 | `README.md` 安全须知：工人继承本机全部凭证，lane 3 靠 glob 与流程，没有系统级隔离。 | `orca serve`、`environment add`、`worker-start --on`、`orca account add`[1][3][15] | 凭证物理隔离；工人崩溃不影响本机。 | **模型冲突**：`COORDINATOR.md:57-61` 与 `:41` 假设 coordinator 能在本机路径上 `git -C <wt> fetch && gate.sh <wt>`、commit、push；远程 worktree 在服务器上。需要改成「工人 push 分支，coordinator 在本机干净 worktree 里 fetch 复跑 gate」。远程 `current` 与 `new-child` 无效；所有后续操作按 Dispatch ID，不重复 `--on`；`worker-list` 须 `--include-remote`，否则全部 `unverifiable`[3]；服务器须保持唤醒；Beta；SSH 目标能否直接做 `worker-start` 放置：文档未写（skill 的 placement 参考提到 SSH，但文中只给出 `--on <environment>` 例子），未核实；Antigravity 不支持 SSH 监督[6]。 | P2（先实验） |
| C9 | **每工作区环境配方**（`environmentRecipes` + `vm recipe doctor`）：每个工作区一台一次性环境。 | 同 C8，且隔离更强。 | `orca.yaml` `environmentRecipes`；Cloud VM 实验开关[5][17] | 一次性环境，销毁即清。 | 实验；付费云资源；`worker-start` 能否选择配方：未核实（skill 说「Run on」选择器在 composer 里，需要主分支上的 `orca.yaml`）。 | P3 |
| C10 | **skills / MCP**：把 intake、coordinator、worker、reviewer 做成可安装技能。 | 角色 prompt 现在由 `cat roles/*.md` 拼进 task spec（`COORDINATOR.md:24`），由 `bin/orca-factory check` 防漂移。 | `npx skills add`、`orca skills install --local`、`skills share`[13] | intake 是人手动开的会话，装成技能可免去「按 `roles/intake.md` 工作」的口令。 | worker 的 spec 要求逐字注入以保证约束（宪法不可覆盖）；技能自动发现会引入第二条加载路径；`skills share` 需 Orca 账号与默认关闭的发布权限；本机 `skills install` 只装 Orca 自带技能，自有技能走 `npx skills add <repo> --skill`[13]。收益低。 | P3 |
| C11 | **通知与收件箱把「需要人」推给用户**。 | 转人靠 Linear 评论与 `auto-failed` 标签（`handoffs.md:59`）。 | 通知、Agents feed、Agent Dashboard「Needs You」[11][12][17] | — | 文档与本机命令表都没有让 agent 或 CLI 发通知的接口；通知只在 agent 完成或阻塞时由 Orca 触发。因此「推给人」只能继续走 Linear + 标签，Orca 侧最多用 C1 的 `workspace-status` 与 `agentWait`。不要设计新推送通道。 | P3（不做） |
| C12 | **Agent Dashboard / Agents feed 与 swarm-board 分工**。 | 两个看板重叠。 | Dashboard（实验）、Agents feed[11][17] | 分工：Orca 看板看 agent 状态；swarm-board 看票的泳道、tier、成本、ledger（Linear、gh、`runs.jsonl`，Orca 里没有这些）。 | Agents feed 没有 CLI 或 API[11]；Dashboard 实验。swarm-board 仍应保留，只把 agent 状态那部分换成 `worktree ps`（C1）。 | P3 |
| C13 | **`gate-create` / `send --to dispatch:<id>` / `task-list --ready --brief`**。 | 诚实评估：`gate-create` 由 coordinator 自己 `gate-resolve`，是 Task DAG 内的决策，不是异步人类审批，对无人值守流水线没有增量[3]。`send --to dispatch:` 可在合并冲突或简报勘误时给在跑的工人追加指令（工人已在检查点读 `check --terminal`，`roles/worker.md:27`）；`task-list --ready --brief` 只在 DAG 波次大时有用。 | 同左 | `send --to dispatch:` 可减少「杀掉重来」。 | 发送成功只证明入队，不证明工人读到[3]。 | P2（`send`）/ P3（gate） |

「用 orchestration 的 Dispatch / worker_done / heartbeat / 决策门替换 coordinator 自管等待与派工」这一方向：**已经基本做完。** 流水线现状见 §2；剩余缺口集中在 C2、C3、C13。

## 5. 研究问题清单

每条标注拟用的核实方式。「实验」指 §6 的编号。

### C1 checkpoints 与 `worktree ps`
- R1.1 `worktree set --comment` 是整体替换还是追加？并发两个写入（工人与 coordinator）谁赢？（实验 E1）
- R1.2 `--workspace-status` 接受哪些自定义 id？`worktree show`/`ps` 的 JSON 字段名是否稳定（本机已见 `workspaceStatus`、`comment`、`linkedLinearIssue`）？（E1、源码 `worktree ps` 的 schema）
- R1.3 `--linear-issue <ID>` 只是链接，还是会双向同步 Linear 状态？（E1，对照 Linear 票是否变化）
- R1.4 `worktree ps` 有无行数上限；`worktree rm` 后行消失还是保留 `isArchived`？（E3）
- R1.5 并发调用下 `ps` 是否也出现 `runtime_unavailable`？（E3）

### C2 幂等
- R2.1 `--retry-request` 对 `worker-start` 在 `failed` 与 `outcome_unknown` 两种回执下分别怎么重放？（E2）
- R2.2 `request-show` 的记录保留多久？Orca 重启后还在吗？（源码 `request` 存储；E2）
- R2.3 `runtime_unavailable` 的真实触发条件是什么？是否只在并发 CLI 调用时出现（`linear-precheck.sh:9` 的说法）？（E3，压测）

### C3 结算与异常
- R3.1 `worker-show` 的 `observation.agentWait` 在哪些提示上命中（信任提示、权限提示、AskUserQuestion）？（E8）
- R3.2 心跳超时的判定阈值：skill 说每 5 分钟一次（`preamble.ts:44`），源码注释说协调器「stale-heartbeat 阈值 10 分钟」。是否真有强制代码、触发什么动作？（源码：`lifecycle-reconciliation.ts`、`db-heartbeat-straggler-guard`）
- R3.3 `worker-release` 对 correction 复用 `--worktree path:` 起的新终端是否各自独立计账？（E2）
- R3.4 `worker-abandon` 在 `1.4.219` 之后的语义与 `worker-stop` 的差别，对本流水线「3 次空等后」的处置有何影响？（读发布说明 + E2）

### C4 自动化
- R4.1 `missedRunGraceMinutes` 的默认值是多少？（`automations create` 后 `show`；源码 `grep`）
- R4.2 应用关闭或机器休眠后，错过的 tick 是补跑一次还是全部补跑？（源码：`dispatch-refusal.ts` 的 `missedBeyondGrace` 只给出判定公式；E6 在测试任务上实证）
- R4.3 precheck 的工作目录、环境变量：源码显示 `cwd` 来自 `resolveAutomationRunTarget`（`new-per-run` 时是仓库主检出，existing 时是工作区路径），`env: process.env`（Orca 主进程环境）、`shell: true`、输出尾部取 4000 字符、超时后对进程组 `SIGKILL`[21]。Orca 主进程的 `PATH` 是否包含 `gh`、`jq`、`orca`？（现状能跑，说明包含；E6 记录 `PATH`）
- R4.4 `skipped_precheck` 行是否被合并、保留多久？（E6）
- R4.5 `--reuse-session` 只对 `existing` 工作区模式有效（help 原文）[1]，流水线是 `new-per-run`。切换是模式变更，不是加一个选项。是否值得？预期不值得（隔离）。

### C5 仓库配置
- R5.1 `worker-start --setup run` 是否执行 `orca.yaml` 里的 `scripts.setup`？还是只执行 Settings 里的本地脚本？（`commandSourcePolicy` 决定共享脚本与本地脚本的取舍[21]；E4）
- R5.2 提交的 `scripts.setup` 在无人值守的 CLI 路径上会弹信任确认吗？内容变化后呢？（E4。`ensure-hooks-confirmed.ts` 在渲染进程，主进程里没有对应的哈希校验；CLI 路径是否经过它：未核实）
- R5.3 `setupRunPolicy` 默认 `run-by-default`（`src/shared/constants.ts:199`）；设为 `ask` 时 `shouldRunSetupForCreate` 抛 `Setup decision required for this repository`[21]。`worker-start` 默认 `--setup run` 是否绕过它？（E4）
- R5.4 setup 脚本的失败退出码如何影响 `worker-start` 回执（help：回执含 `setup`、`failedStage`）？（E4）
- R5.5 setup 的环境变量：源码确认有 `ORCA_ROOT_PATH`、`ORCA_WORKTREE_PATH`、`ORCA_WORKSPACE_NAME`，另有 `CONDUCTOR_ROOT_PATH`、`GHOSTX_ROOT_PATH`，以及强制 `credential guard`（Setup 终端视为无人值守）[21]。`credential guard` 对 `git fetch` 私有仓库的影响？（E4）

### C6 hibernation
- R6.1 已结算未释放的工人休眠后，`worker-read` 与 `worker-show` 还能读到什么？
- R6.2 coordinator 终端在自动化运行结束后休眠，是否影响 `.omc/agent-run-finished` 与 prune？（预期无影响）
- R6.3 自动化运行的终端是否被视为「后台」？（文档未写）

### C7 hooks
- R7.1 Codex 工人的状态是否同样进入 `worker-list` 的 `projection`？（本机 hooks 已装，E8 实证）
- R7.2 `agent hooks off` 时 `worker-list` 与 `agentWait` 退化到什么？

### C8 远程隔离
- R8.1 `worker-start --on` 的工人能否 `git push` 到 GitHub 而不暴露本机凭证？服务器上用什么最小权限凭证？（E5）
- R8.2 coordinator 如何在本机复跑 gate？「工人 push 分支 + 本机 fetch」的延迟与失败面？（E5）
- R8.3 服务器断线时 `worker-list --include-remote` 的 `unverifiable` 如何处置，是否会卡住 Run？（E5）
- R8.4 SSH 目标（而非 Orca 服务器）能否作为 `worker-start` 的放置？（`orca host list` 与 `--host ssh:<id>`；文档未写，E5 前先读 `placement-and-remote.md` 全文与源码）
- R8.5 一台服务器同时跑多个工人（`AGENT_MAX_IN_FLIGHT × 3`）的资源上限？

### C10-C13
- R10.1 `npx skills add` 装到哪里；与 Claude Code 的 `.claude/` 配置是否冲突？
- R11.1 是否存在未公开的通知接口？以 `orca agent-context` 的 239 条命令为准：无。
- R12.1 `worktree ps` 是否足以覆盖 swarm-board 的 agent 视角（时长、token、成本）？Token 与成本来自 `orca-claude-usage.json`（`register.tsx:15`），`ps` 没有。

## 6. 验证实验设计

只写方案，不执行。所有实验在**非生产沙箱仓库**里做：新建一个只含 `README.md` 和一个微小测试的仓库，在 Orca 注册；不用 `zhiyun-ai-dashboard`，也不用 orca-factory 自己的 Linear project。每个实验前后记录 `orca worktree list` 与 `orca orchestration run-list`，便于对账。

### E1 checkpoints 写回（验证 C1；P1）
- **前置**：沙箱仓库已注册；一张沙箱 Linear 票（专用 team/project，不带 `ready-for-agent`）。
- **步骤**：
  1. `orca worktree create --repo path:<沙箱> --name exp-ckpt --linear-issue <票号> --json`
  2. `orca worktree set --worktree name:exp-ckpt --comment "phase=implementing" --workspace-status in-progress --json`
  3. `orca worktree show --worktree name:exp-ckpt --json`、`orca worktree ps --json`，记录字段。
  4. 并发两次 `worktree set --comment A` 与 `--comment B`，再读。
  5. `--workspace-status custom-x`（不存在的 id）。
  6. 到 Linear 看票状态与评论有无变化。
- **通过**：`comment` 与 `workspaceStatus` 出现在 `ps`/`show`；最后写入者胜出且无损坏；未知状态 id 给出明确错误；Linear 无意外变化（若有，记录为「双向同步」，改写 C1 的风险）。
- **失败**：`ps` 缺字段；并发写入丢失或崩溃。
- **风险与回滚**：只改沙箱 worktree 元数据；`orca worktree set --comment "" ...` 后 `orca worktree rm --worktree name:exp-ckpt --force`。

### E1 实测结果（2026-10-04，Orca 1.4.218，本机）
证据强度：本机实测。沙箱为临时 worktree `exp-ckpt` 加一张沙箱票，实验后已删除和取消，无残留。

| 判据 | 结果 | 说明 |
| --- | --- | --- |
| `comment`、`workspaceStatus`、`linkedLinearIssue` 出现在 `worktree show` 与 `worktree ps` | 通过 | 两处字段一致；`linkedPR` 无 PR 时为 `null` |
| 并发两次 `--comment` | 通过 | 最后写入者胜出（A、B 同时写，读到 A），无损坏、无报错 |
| 未知状态 id（`custom-x`）给出明确错误 | **未通过** | 命令返回 `ok:true`，状态被存成 `custom-x`。写入方必须自己限定在 `todo / in-progress / in-review / completed` |
| Linear 无意外变化 | 通过 | 关联、写评论、改状态（in-progress、in-review、completed）后，Linear 票仍为 Backlog、无评论、无附件：没有观察到双向同步 |
| `liveTerminalCount` | 差异 | `show` 返回 `null`，`ps` 返回 `1`；终端计数只能从 `ps` 取 |

对候选 C1 的影响：写回可行且无副作用；风险项「`--linear-issue` 是否反向同步 Linear」可关闭（本版本未观察到）；新增一条约束：状态 id 由调用方校验。

### E2 编排幂等、correction 与结算（验证 C2、C3、I2、I3；P1）
- **前置**：沙箱仓库；Orca 已启用 `codex` 与 `claude`。
- **步骤**：
  1. `orca orchestration run-create --objective "exp: idempotency" --retry-request exp-r1 --json`，同一命令再执行一次。
  2. `orca orchestration request-show --request exp-r1 --json`。
  3. 建一个只做「在 `README.md` 加一行」的 Task，`worker-start --retry-request exp-w1` 执行两次，数 Dispatch。
  4. 模拟 correction：让工人 `worker_done --outcome succeeded`，coordinator 侧按 `handoffs.md:45-48` 走 `task-update --status failed`、`task-create --parent`、`worker-start --worktree path:<wt>`，最后对父 Task `task-update --status completed`，观察是否被拒绝以及最终 `task-list` 状态。
  5. 收尾读 `worker-list --run <run> --terminal-state reclaimable --json`，记录泄漏的终端。
  6. 逐个 `worker-release`，再读一次。
- **通过**：步骤 1、3 无重复 Run、Dispatch；`request-show` 返回 `completed` 与回执；步骤 4 的状态与 `handoffs.md` 一致或明确被 Orca 拒绝（据此修 I2）；步骤 5 能列出遗留终端。
- **失败**：同一 `retry-request` 产生第二个 Dispatch。
- **风险与回滚**：只在沙箱 Run 内操作；失败的 Run 用 `worker-release`；**不要**用 `orchestration reset`（运行时全局，会影响真实运行中的 coordinator）。若真实流水线正在运行，则推迟实验。

### E3 `worktree ps` 作看板源与并发（验证 C1、R2.3；P1）
- **前置**：E1 通过；真实运行时可观察的低峰窗口。
- **步骤**：
  1. 脚本对 `orca worktree ps --json` 与 `orca terminal list --json` 各循环 50 次、并发 4 个，记录 `error.code` 分布与耗时。
  2. 对比同一时刻 `ps` 的 `liveTerminalCount`/`workspaceStatus` 与 swarm-board 当前显示。
- **通过**：`runtime_unavailable` 率不高于 `terminal list`；`ps` 字段覆盖 swarm-board 泳道需要的状态。
- **失败**：`ps` 慢于 `terminal list` + `run-list` 的组合，或字段缺失。
- **风险与回滚**：只读命令，无回滚。

### E4 `orca.yaml` setup 与信任确认（验证 C5、I4、R5.1-R5.5；P1，最重要的未核实项）
- **前置**：沙箱仓库根有 `orca.yaml`：`scripts.setup` 写入标记文件 `.omc/setup-ran`，并 `exit 0`；`setupAgentStartupPolicy: wait-for-setup`。该文件已提交到主分支。Settings → Repository → Hooks 里本地脚本留空。
- **步骤**：
  1. 全新状态（没有该仓库的信任记录）：从 CLI `orca orchestration worker-start --spec "在 README.md 加一行" --worktree new-top-level --repo path:<沙箱> --name exp-setup --agent codex --setup run --json`。
  2. 观察：是否弹出确认对话框；回执 `setup` 字段；标记文件是否出现；工人收到 prompt 的时间点（对比 `start-immediately`）。
  3. 改 `scripts.setup` 内容并提交，再跑一次，看是否再次要求确认。
  4. 让 `scripts.setup` 以非 0 退出，看回执与工人是否仍启动。
  5. 在 Settings 里把 `setupRunPolicy` 设为 `ask`，重复步骤 1。
- **通过**：能明确回答「无人值守是否被信任对话框阻塞」；记录退出码语义。若被阻塞：C5 降级，ONBOARD 里保留「由用户在 UI 里批准一次」，并写明内容变化需重新批准。
- **失败**：CLI 路径卡死且无超时（此时在 `ONBOARD.md` 和 `workflow.md` 里禁止通过 `orca.yaml` 提交 setup 脚本）。
- **风险与回滚**：脚本只写标记文件；不要在沙箱里放任何凭证；还原 `orca.yaml` 与 Settings。

### E5 远程隔离工人（验证 C8；P2，高收益）
- **前置**：一台干净的机器（Linux VM 即可）；`orca serve --pairing-address <tailscale ip>`；服务器上 `orca account add --agent codex`；服务器上**只配置**一个仅对沙箱仓库有推送权限的 deploy key，不登录个人 `gh`；本机 `orca environment add --name exp-remote --pairing-code <码>`。
- **步骤**：
  1. `orca host list --json` 找到环境 id。
  2. `orca orchestration worker-start --spec "在 README.md 加一行并 push 到分支 exp/remote-1" --on exp-remote --worktree new-top-level --repo <远程仓库的精确 selector> --name exp-remote-1 --agent codex --setup run --json`
  3. 让工人试 `gh auth status`、`env | grep -i token`、读 `~/.ssh`，确认看不到本机凭证。
  4. 本机 `git fetch` 该分支，在干净的本机 worktree 里跑 gate。
  5. 中途断开网络 2 分钟再恢复，读 `worker-list --run <run> --include-remote --json` 的 `liveness`。
- **通过**：工人看不到本机任何凭证；推送成功；本机能独立复跑 gate；断线时标为 `unverifiable`，恢复后自动变回 `live`，不触发误停。
- **失败**：coordinator 无法在不改 `COORDINATOR.md` 第 3、6 节的前提下完成复核（此时 C8 变成需要改流水线协议的独立设计任务，写成单独的 Linear 票，不在本方案内做）。
- **风险与回滚**：服务器配对 URL 等同密码，不要写入仓库或日志；实验后在服务器 Settings → Shared Server Access 里撤销授权，`orca environment rm --environment exp-remote`；删除 deploy key。

### E6 自动化语义与卫生（验证 C4、I1；P1）
- **前置**：沙箱仓库；**新建**一个测试自动化 `exp-automation`，`--disabled` 创建，`--precheck` 用 `sleep 5; exit 1`（先验证跳过）与 `sleep 70`（验证超时，默认 60 秒）。
- **步骤**：
  1. `orca automations create --name exp-automation --trigger '*/5 * * * *' --provider claude --repo path:<沙箱> --workspace-mode new-per-run --precheck "echo \$PATH; pwd; exit 1" --prompt "noop" --disabled --json`，随后 `orca automations show <id> --json`：记录默认 `missedRunGraceMinutes`、`precheck.timeoutSeconds`。
  2. `orca automations run <id> --json`（手动触发一次），`orca automations runs --id <id> --json`：读 `precheckResult` 的 `stdout`（含 `PATH`、`pwd`）。
  3. 把 precheck 改为 `sleep 70`，再手动触发，读状态与 `error`。
  4. 对 orca-factory 的真实任务，只做只读：`automations show`，并对比 `install.sh` 的预期值，输出差异表给仓库所有者；**是否修改由所有者决定**。
- **通过**：能回答 R4.1、R4.3、R4.4；I1 的差异表交付。
- **失败**：无。
- **风险与回滚**：测试自动化保持禁用；结束后 `orca automations remove <id>`。不要碰真实任务的 `--enabled` 状态。

### E7 hibernation 相容性（验证 C6；P3，等 Orca 稳定后再做）
- **前置**：沙箱仓库；Settings → Experimental → Agent hibernation 打开，`Hibernate after` 设 1 分钟。
- **步骤**：跑 E2 的小任务；让一个已结算工人闲置 2 分钟；再 `worker-read --dispatch` 与 `worker-show`；跑一个 `check --wait` 中的 coordinator，看是否被休眠。
- **通过**：未结算 Dispatch 不休眠；已结算的休眠后 `worker-read` 仍可读。
- **失败**：`check --wait` 中的 coordinator 被休眠，或已结算工人休眠后 `worker-read` 失败。
- **风险与回滚**：关闭开关；实验后恢复 30 分钟默认值。

### E8 `agentWait` 与 hook 状态（验证 C3、C7；P1）
- **前置**：沙箱仓库；`orca agent hooks status --json` 显示 `claude`、`codex` 已装。
- **步骤**：派一个会触发权限或信任提示的工人（例如让它执行需要批准的命令，且不预批准），在其等待期间读 `worker-show --dispatch <id> --json` 的 `observation.agentWait` 与 `worker-list` 的 `projection.attention`；分别用 codex 与 claude；再 `orca agent hooks off`，重读（需所有者同意，实验后 `on`）。
- **通过**：`agentWait` 命中并给出证据源（hook、提示文本或标题）；关闭 hooks 后退化为 `unverifiable` 而不是误报。
- **失败**：codex 工人没有状态。
- **风险与回滚**：`orca agent hooks on` 恢复；关闭 hooks 会影响本机所有 Orca 会话，须在无真实运行时做。

## 7. 分阶段路线与风险登记

### 7.1 路线

| 阶段 | 内容 | 前置 | 产出 |
| --- | --- | --- | --- |
| 0（立即，不改代码） | I1、I2、I3、I6、I7 的只读核对；读 `v1.4.219`、`v1.4.220` 完整发布说明 | 无 | 差异表；是否升级的建议 |
| 1（实验，沙箱） | E1、E2、E3、E6、E8 | 阶段 0 | 回答 R1.x、R2.x、R3.x、R4.x、R7.x |
| 1b（实验，最重要的未核实项） | E4 | 阶段 0 | 决定 C5 做不做、怎么写进 `ONBOARD.md` |
| 2（实现，各自一张票，走 lane 2 的 PR） | C2 + C3（`handoffs.md`、`workflow.md §5`、`COORDINATOR.md` 收尾）；C1（`COORDINATOR.md` 加一行 `worktree set`；`register.tsx` 换数据源）；C4（`install.sh` 对齐更新路径、检查结果）；I7（prompt 用紧凑版） | 阶段 1 | `core/` 改动，`bin/orca-factory update .` 同步 |
| 3（隔离，单独立项） | E5；通过后把 C8 写成设计文档，改 `COORDINATOR.md` 第 3、6 节的复核模型 | 阶段 1 与 2 | 工人隔离方案 |
| 等待 Orca 稳定 | C6 hibernation（实验 + 默认关）；C9 环境配方（实验 + 付费）；C12 Agent Dashboard（实验）；远程服务器（Beta） | Orca 稳定标注 | 复审 |
| 不做 | C11 通知推送；`--reuse-session`；`gate-create` 作异步人工审批 | — | — |

阶段 2 的改动都属于流水线本体（`core/`、`mods/`、`ONBOARD.md`），按本仓库规则走 PR，并在改完 `core/` 后跑 `bin/orca-factory update .` 与 `bin/orca-factory selftest`（`ONBOARD.md` 自接入一节）。本研究方案本身不改任何这些文件。

### 7.2 风险登记

| 编号 | 风险 | 可能性 | 影响 | 缓解 |
| --- | --- | --- | --- | --- |
| K1 | 编排被官网标为实验，命令或语义会变（已有 `coordinator-start/stop` 退役先例） | 中 | 流水线整体失效 | 每次升级前读发布说明；`orca skills get orchestration --full` 作为协议真相；`selftest` 加一条「命令仍存在」的检查（`orca agent-context` 对照） |
| K2 | precheck 超时静默跳过，表现为不领票 | 中（票多时） | 流水线停摆无告警 | I1 修复；监控 `skipped_precheck` 的 `error` 含 `timed out` |
| K3 | 盲重试造成重复 Task 或重复工人 | 低到中 | 重复实现、冲突 PR | C2 |
| K4 | 提交的 `orca.yaml` setup 脚本在无人值守时卡在信任确认 | 未知 | 工人起不来 | E4 先行；失败时保留 UI 手动配置 |
| K5 | `sharedDirectories` 的 symlink 让并行工人互相覆盖 `node_modules` | 中 | 测试不稳定 | 不默认启用；E4 之后单独评估 |
| K6 | 远程隔离后 coordinator 复核模型失效 | 高（若直接上） | 复核链断 | E5 先验证「工人 push + 本机 fetch 复跑」 |
| K7 | 工人继承本机凭证（现状） | 已存在 | lane 3 仅靠 glob 与流程 | C8；接入初期只放小票（`README.md` 安全须知） |
| K8 | 休眠或关闭应用导致错过 tick | 中 | 漏领延迟 | Keep computer awake；R4.2 实证；precheck 每 5 分钟重新评估，漏跑无害 |
| K9 | 本机落后 Orca 最新版两个小版本，已知的信任提示卡死问题（`v1.4.219`）仍在 | 中 | 无人值守卡住 | I6 |
| K10 | `worktree set --comment` 覆盖了人写的目标或约束 | 中 | 人写的上下文丢失 | 先 `worktree current/show` 再合并写入；只写以 `factory:` 开头的首行，保留其余行 |
| K11 | swarm-board 依赖 `worktree ps` 的未文档化字段 | 中 | 看板空白 | 字段读不到时回退到现有逻辑；E3 |

## 8. 来源列表

本机（Orca `1.4.218`，只读命令）：

- [1] 本机 `orca --version`、`orca --help`，以及 `orca automations|worktree|orchestration|skills|environment <子命令> --help`（`automations create/edit/run/runs`、`worktree create/set/ps`、`orchestration worker-start/check/send/run-create/task-create/task-update/worker-list/worker-show/gate-create/worker-release`、`skills install`、`environment add`）。
- [2] 本机 `orca agent-context --json`：239 条命令，schema v1。
- [3] 本机 `orca skills get orchestration --full`，及 `--references` 列出的七份参考：`coordinator-loop`、`legacy-contract-migration`、`low-level-topology`、`messaging-and-gates`、`placement-and-remote`、`recovery-and-cleanup`、`worker-contract`。
- [4] 本机 `orca skills get orca-cli --full`。
- [5] 本机只读状态查询：`orca automations list --json`、`orca automations runs --id <orca-factory 任务> --json`、`orca agent hooks status --json`、`orca worktree ps --json`、`orca status --json`、`orca skills list`、`orca skills get orca-per-workspace-env`。

官方文档（`https://www.onorca.dev`，无版本号，读于 2026-10-04）：

- [6] `/docs/cli/orchestration`
- [7] `/docs/cli/automations`
- [8] `/docs/cli/worktree-checkpoints`
- [9] `/docs/agents/hibernation`
- [10] `/docs/agents/hooks-memory`
- [11] `/docs/activity`（页面标题为 Agents feed；页面没有出现「Activity feed」或「Agent Dashboard」字样）
- [12] `/docs/notifications`
- [13] `/docs/cli/skills`
- [14] `/docs/ssh`
- [15] `/docs/remote-servers`
- [16] `/docs/model/worktrees`
- [17] `/docs/settings`
- [18] `/docs/cli/reference`
- [19] `/docs`（导航目录）

官方发布与源码：

- [20] `https://github.com/stablyai/orca/releases`：`v1.4.214` 至 `v1.4.220` 的发布说明（页面被截断，部分未读）。
- [21] `https://github.com/stablyai/orca` 源码，tag `v1.4.218`（与本机一致）。引用文件：`src/shared/automation-precheck.ts`（默认超时 60、上限 600、输出尾部 4000 字符）、`src/main/automations/precheck-runner.ts`（`cwd`、`env: process.env`、`shell: true`、超时后 `SIGKILL`）、`src/main/automations/dispatch-refusal.ts`（`missedBeyondGrace`）、`src/shared/automations-types.ts`（运行状态含 `skipped_precheck`、`skipped_missed`）、`src/main/automations/run-target-resolution.ts`、`src/shared/orca-yaml-hook-types.ts`、`src/main/setup-hook-env-vars.ts`、`src/main/effective-hook-config.ts`、`src/shared/constants.ts:199`、`src/renderer/src/lib/ensure-hooks-confirmed.ts`、`src/main/runtime/orchestration/preamble.ts:44`、`src/main/runtime/orchestration/coordinator-stale-base-flag.ts:4`、`src/shared/agent-status-freshness.ts:7`、`docs/reference/agent-status-store.md`。

本仓库：

- [22] `core/install.sh`、`core/linear-precheck.sh`、`core/prune-run-worktrees.sh`、`core/COORDINATOR.md`、`core/constitution/workflow.md`、`core/constitution/handoffs.md`、`core/roles/{worker,intake}.md`、`core/roles/README.md`、`mods/swarm-board/hooks/register.tsx`、`ONBOARD.md`、`README.md`。正文用「文件:行」直接标注。

## 附：本方案未能核实的事项

- 提交到 git 的 `orca.yaml` `scripts.setup` 在无人值守的 CLI 路径上是否触发信任确认（R5.2）。
- `missedRunGraceMinutes` 的默认值与休眠后补跑行为（R4.1、R4.2）。
- `worker-start` 能否直接以 SSH 目标作放置，以及远程隔离下 coordinator 的复核模型能否保持（R8.4、C8）。
