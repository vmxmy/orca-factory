# 外部契约

驱动器依赖外部系统的输出格式。下面每一条都曾在上线后造成故障，因为单元测试的手写假数据与真实格式不同。改动对应代码时，先看这里；排查调度器为什么领或不领、为什么重拉，先运行 `driver serve --dry-run`（只读一轮巡检，打印计划动作），测试读取 `testdata/recorded/` 中录制的真实样本；格式有新发现时，录制新样本并在此补一行。gate 要求通过的 `TestRecorded*` 契约测试数量不低于分支与 `origin/main` 分叉点上的定义数（`scripts/agent-gates/gate.project.sh`）。

| 契约 | 真实样本 | 测试 |
|---|---|---|
| `orca orchestration task-list` 的任务**没有** `dispatch_id` 字段 | `orca-task-list.json` | `executor.TestRecordedTaskList`、`executor.TestRecordedStartWithoutDispatchID` |
| Orca 变更请求重用 request-id 但输入（含调用终端）不同 → `request_mismatch`，不可重试 | `orca-run-create-request-mismatch.json` | `orca.TestRecordedRequestMismatch` |
| `orca terminal list` 的完整枚举字段（totalCount、truncated、hostScope） | `orca-terminal-list.json` | `scheduler.TestRecordedTerminalList` |
| Linear 存储正文时转义 CommonMark 字符（`*` 存为 `\*`，`\` 存为 `\\`） | `linear-description-escaped.md` | `executor.TestRecordedTicketDescriptions` |
| 票面「CHECK:」的下一行是「断言:」，不属于命令 | `linear-description-check-assertion.md` | 同上 |
| 小节标题可能带说明，如「## 要做（接口票）」 | `linear-description-heading-note.md` | 同上 |
| 「A 被 B 阻塞」存在 B 上；查 A 的 `inverseRelations`（type `blocks`）及阻塞者 `state.type` | `linear-inverse-relations.json` | `linear.TestRecordedInverseRelations` |
| 调度器认领时写 `term=- run=-`，表示执行器尚未登记 | `linear-comment-ownership-unregistered.json` | `scheduler.TestRecordedOwnershipUnregistered` |
| 审查员常把结尾的 verdict JSON 包在 Markdown 代码块里 | `reviewer-output-fenced.txt` | `executor.TestRecordedReviewerOutput` |
| 执行器转人评论固定三行（转人、证据、需要你），不含归属行；调度器以「转人:」开头识别归属转为人 | `linear-comments-executor-handoff.json` | `scheduler.TestRecordedExecutorHandoff` |
| git 的网络失败（TLS 握手、超时、解析失败）在 stderr 中报告，可重试；HTTP 403、404 不可重试 | `git-stderr-tls-failure.txt`、`git-stderr-auth-failure.txt` | `infra.TestRecordedGitNetworkFailureIsTransient` |
| worker worktree 名为 `<票号>-<单元>`，本地分支是 Orca 起的（如 `vmxmy/ZII-893-executor`），`agent/<票号>/<单元>` 只是推送目标 | `git-worktree-list-porcelain.bin` | `executor.TestRecordedWorkerWorktree` |
| `claude -p` 的 `--allowedTools` 是可变参数，会吞掉后面的位置参数；审查员的说明与 diff 必须一起走 stdin | — | `executor.TestReviewSendsPromptAndDiffOnStdin`，`driver selftest --live` |
| `terminal send --wait-submit` 的回执 `send.prompt.stages` 含 `turn_started` 才证明 agent 开始了一轮；shell 等提供方只给 `input_accepted`（`observation: unsupported`） | `orca-prompt-receipt-codex-turn-started.json`、`orca-terminal-send-wait-submit-shell.json` | `orca.TestRecordedTerminalSendTurnStarted` |
| `orca worktree set` 成功回执带 `result.worktree`；选择器不匹配时拒绝码为 `selector_not_found` | `orca-worktree-set.json`、`orca-worktree-set-not-found.json` | `orca.TestRecordedWorktreeSet` |
| 在已有 worktree 上 `worker-start` 等 agent 就绪可超过 30 秒（实测 38 秒）；驱动器的调用超时必须长于传给 Orca 的 `--timeout-ms` | `request-show` 实测（ZII-897 第 3 次 correction） | `orca.TestCommands`、`orca.TestFailedStartRetry` 断言 `--timeout-ms` |
| `orca orchestration worker-list` 的 `projection.liveness.verdict` 为 `exited` / `unverifiable`；`unverifiable` 不代表进程死亡；`resource.worktreeId` 形如 `<repoId>::<绝对路径>` | `orca-worker-list-liveness.json`（2026-10-06 从 Run `run_9714ea9b8576` 重录；仅保留契约字段并替换 worktree 路径） | `orca.TestRecordedWorkerListLiveness` |
| `orca orchestration worker-stop --dispatch` 对未结算的 dispatch 返回 `state: stopped`、`alreadySettled: false`、`processAction: closed_agent_terminal`；停后该 dispatch 在 `worker-list` 里变为 `failed` / `process_stopped` | `orca-worker-stop.json`（真实回执，2026-10-06） | `orca.TestRecordedWorkerStop`、`TestCommands` |
| Codex 启动崩溃（`account/read failed … workspace routing discovery timed out`）时 worker 的 `stage.detail` 为 `turn_start_unobserved`、`liveness.verdict` 为 `unverifiable`。**尚无录制文件**：2026-10-06 在 ZII-901 现场观察到，停掉后状态已变；下次再现时必须录下并补测试 | — | `executor.TestWaitStopsWorkerThatNeverStarted`（合成值） |
| Linear `issueUpdate` 移除票上没有的标签时拒绝整个更新（`Label not on issue`，INPUT_ERROR）；`UpdateIssue` 先读当前标签，只加缺少的、只删存在的 | `linear-remove-absent-label.json` | `linear.TestRecordedRemoveAbsentLabel` |
| 探测额度只需一次 `query { viewer { id } }`：响应头 `x-ratelimit-requests-limit/remaining/reset` 反映当前预算（Reset 是滑动窗口，约为现在加一小时，不是预算回满的时刻）；样本里 viewer id 已替换为全零 | `linear-viewer-probe.json`、`linear-response-headers.txt` | `linear.TestRecordedQuotaProbe`、`scheduler.TestQuotaPauseProbesWithoutSnapshotAndResumesWithHysteresis` |
| GitHub PR：`state`、`merged`、`mergeable_state`（合并后为 `unknown`）、`merge_commit_sha`（squash 后即 main 上的提交）；`compare/<base>...<head>` 的 `behind_by`、`merge_base_commit.sha` 与 `files`（最多 300，不标截断）；本仓库 CI 只有 GitGuardian 一个 check-run，旧式 commit status 为空且显示 `pending`，CI 只看 check-runs | `github-pr-open-clean.json`、`github-pr-merged.json`、`github-compare-*.json`、`github-commit-main.json`、`github-check-runs-gitguardian.json` | `github.TestRecordedMergeLaneReads`，`driver selftest --live` |
| squash 时 `sha` 与 PR head 不符返回 409 `Head branch was modified`，不合并；草稿 PR 返回 405 | `github-merge-head-modified-409.json` | `github.TestRecordedMergeHeadModified` |
| Claude 会话每条 `assistant` 记录带 `sessionId`、`cwd`、`message.id`、`message.model`、`message.usage`（`input_tokens`、`output_tokens`、`cache_read_input_tokens`、`cache_creation_input_tokens`，含 `cache_creation.ephemeral_5m/1h_input_tokens`）；同一 API 消息会在多条记录里重复出现，按 `message.id` 只计一次 | `claude-session-assistant.jsonl` | `usage.TestRecordedSessionFormats` |
| Claude 子代理的转录在 `<session>/subagents/agent-*.jsonl`，`sessionId` 与父会话相同，`isSidechain` 为真，`cwd` 为子代理所在目录 | `claude-session-assistant.jsonl`（第 2 行） | `usage.TestRecordedSessionFormats`、`usage.TestCollectCountsSubagentTranscripts` |
| Codex rollout 以 `session_meta`（`payload.id`、`payload.cwd`）开头，`turn_context` 给 `payload.model`，`token_count` 的 `total_token_usage` 按会话累计（含 `cached_input_tokens`、`cache_write_input_tokens`、`reasoning_output_tokens`），一个会话只计一次；`input_tokens` 含缓存读写，需先减去 | `codex-session-token-count.jsonl` | `usage.TestRecordedSessionFormats` |
| 价格表里没有的模型按 `unknown` 记录，不估价 | — | `usage.TestCollect` |

部署新二进制前运行 `driver selftest --live`：对真实的 Orca、Linear、git 与审查员命令只读地复核上述契约（只调用一次审查员）。

录制样本前删掉终端预览等可能含他人会话内容的字段，并确认不含任何凭据。

执行器派工恢复保留旧 `executor.json` 字段及请求 UUID 的终端作用域；新增可选 `PendingParent` 记录未确认的 correction 子任务创建。首轮与 correction 都在变更前写入 pending，每次新 dispatch 清除上一轮的完成与释放资格；回执伴随错误返回时仍保留可用的 task、dispatch 和 worktree 身份。

当前 task 的权威报告优先于旧提交的 `agent-status: complete`。pending 派工不能由旧 complete 或已有 PR 放行；缺失、歧义或显式截断的任务证据只允许停止对账，不授权重放旧终端请求。`task-list` 缺失 `dispatch_id` 时仍可根据 task 报告恢复，但不会臆造 dispatch 来释放 worker。子任务通过真实 `parent_id` 与 pending parent 对账；尚无权威报告的未确认派工保持 pending，待后续重新启动对账。

## 合并交接（#141）

持久事实保存在已有 Linear 票评论与 GitHub PR；本地没有新增合并数据库。类型定义见 `internal/merge/evidence.go`。

- `merge-handoff-v1: <JSON>` 与 `归属: 合并通道` 写在同一评论。`version=1`，冻结 `ticket_id/identifier/repository/unit/pr/head/baseline`、完整 `gate_spec/gate_output/review_output/lane`、`run/task/dispatch/worktree`、`attempt/corrections/infra/catchup_rounds`、`enqueued_at/queue_id`。`baseline` 是本轮验证的 merge-base；`head` 是实际验证提交。`queue_id` 由仓库、票、单元、head 和 Orca 派工关联生成，重放使用原记录；correction 的新派工产生新身份，排到队尾。
- `merge-progress-v1: <JSON>` 关联 `version/queue_id/ticket_id/repository/pr/head/main/merge_sha/phase`。`phase=merge-intent` 在 PUT 前持久记录固定 main，写完再核对远端证据；`phase=merged` 只在 GitHub 回读 merged 后记录真实 squash SHA。若恢复时 PR 已合并且没有 intent，`main_unknown=true`、`main=""` 明确表示历史 main 未知，不能用当前 main 伪造；该记录可反复对账并完成票面写回。
- FIFO 按冻结 `enqueued_at`、`queue_id` 排序，不依赖 issue UpdatedAt。缺失、冲突或不可解析交接阻塞该仓库；同仓库每 Tick 只处理队首。完成评论或 Done 写入失败保留归属；Done 但仍属合并通道的票继续参与对账。已确认完成后的名额在下一快照释放。
- 合并前读完整分页 PR 候选、固定 head 的 checks/statuses 与 Git 提交对象。用固定 merge-base 的 `git diff --no-renames -z` 读取全部路径（重命名表现为删除与新增），不使用 GitHub compare 的 300 文件截断列表。空/未知/缺字段 CI 均不绿。lane 2 需显式 `AGENT_LANE2_AUTOMERGE=1`；manual、lane 3 路径等待。旧 PR 或旧三行归属不能替代验证证据。
- 合并结果验证由 `Runtime.advanceMerge` 组装真实 `merge.CommandGit` 和冻结 gate adapter。#143 部署屏障见下节，读取 Linear 原始项目快照/评论后保留 Done/转人票的未完成屏障。
- `Executor.Start` 对已有 PR 仅复用匹配的完整冻结交接，保留原 FIFO 与预算，不新写旧三行归属。恢复/correction 实现应在该短路前处理待恢复派工；独立 helper 位于 `executor/merge_handoff.go`。

验证：`TestExecuteFreezesMergeHandoff`、`TestStartExistingPR*`、`TestRuntimeMerge*`、`TestServeRuntimeActuallyMergesAndDryRunIsReadOnly` 使用外部替身、HTTP 和临时 Git 仓库；这些是合成行为测试，不冒充线上录制样本。


## 部署进度增量（#143）

冻结 `merge-handoff-v1` 格式不变；`merge-progress-v1` 仅增加可选 `deployment` 对象，外层 `phase` 仍为 `merge-intent` / `merged`。不占用 #142 的 catchup/correction 阶段，也不修改 #144/#145 的派工恢复或预算。

- `deployment.workflow/smoke` 是 intent 时冻结的配置。`state` 为 `pending`、`smoke-intent`、`paused`、`passed`；`reason` 为暂停原因；`smoke_started=true` 表示已持久预留执行，不能凭重启推断安全重试；`smoke_passed=true` 仅在命令确认成功后写入，自动通过回执必须承接同身份的 smoke-intent。
- `deployment.recovery` 的 `action/operator/reason/at` 是人工决定，动作仅为 `retry-workflow`、`retry-smoke`、`confirm-passed`。恢复时精确匹配票、queue、squash SHA，追加完整 Progress，不覆写旧评论。恢复记录随后续状态一同保留。
- 每条历史 Progress 都校验状态转换。清除执行标记、从暂停恢复或人工确认通过必须有新的 `recovery`，操作者/理由/动作/时间均有效且时间晚于上次恢复；旧授权不能再次放行。无授权的 pending/passed 记录会保留冲突屏障，后续记录不能掩盖它。
- 同 queue 的重试复用既存冻结配置（包括无配置），不从更新后的 env 重建 intent；合法配置变更仅影响新 queue。
- 新部署字段在 squash intent 之前持久；实际 SHA 只来自 GitHub 已合并 PR 回读。smoke 必须在其 intent 写回确认后执行，执行成功但通过回执无法确认时保留屏障，重建调度器要求人工对账。
- 从项目全量分页 Snapshot 及 ListComments 重建屏障，先读取持久证据再筛选活动票。Done、auto-failed、归属为人或无的票，只要仍有未完成/冲突的部署证据便继续参与该仓库 FIFO。部署字段消失、配置不一致或身份冲突不会静默放行。
- GitHub 使用 [workflow runs REST API](https://docs.github.com/en/rest/actions/workflow-runs)：固定 workflow 与 `head_sha=<实际 squash SHA>`，显式核对响应身份和完整分页。显示名从 workflows 集合唯一解析。空/缺失/未知/仍运行的回执等待，已完成的明确失败转人；成功才 smoke。多个历史 run 以最新 run 的完成结论为准，但仍运行的旧 run 保守等待。
- status 与 dry-run 仅展示 queue/SHA/workflow/state/reason，不暴露 smoke 命令。status 是观察缓存，不是恢复授权；人工恢复命令与 serve 共用锁，dry-run 不取锁、不写日志或评论、不执行 smoke。

合成验证：`TestDeployment*`（scheduler/GitHub/command）、`TestResumeDeployment*`、`TestServeDryRunDoesNotWriteLogs`。使用已有 HTTP、命令和 Linear 替身，无真实部署或票面变更。

## 重叠验证与 correction（#142）

- 保留 `merge-handoff-v1` / `merge-progress-v1` 格式。Progress 增加可选 `corrections/catchup_rounds/infra/attempt/reason`，增加 `waiting`、`correction`、`human` phase。`waiting` 包含移动 main 后的 catchup；写入的 correction 数是累计值（Channel.Result.Corrections 仍是增量）。`merge-intent` 和 `merged` 同样保留累计预算。未知 phase 停止处理，不覆盖为 legacy；部署状态保持 #143 的嵌套 `deployment` 对象；后续 Progress 写入保留它。
- 对固定 head/main 执行真实 `merge-tree --write-tree`，以两者为 parent 建立临时提交，在隔离 worktree 调用完整 gate。gate.sh 与项目 hook 从固定 main 读取，`GATE_BASE/GATE_PROJECT_SRC` 都固定该 main；spec 使用交接冻结字节，不从 PR 描述猜测，也不只跑 CHECK。spec 和脚本位于 worktree 旁，不进入验证 diff。
- 文本冲突即使 GitHub `mergeable=false` 仍可验证并交回 correction；这个 false 不授权 squash。完整 gate 红才计 correction，非完整输出、启动/读取/提交/worktree 失败及取消不计。绿 gate 的清理错误通过 `ErrWorktreeCleanup` 报告，同时保留合并、等待或 correction 决策；取消会使用独立清理上下文；读取 gate 脚本失败保留安全取消/超时与基础设施分类链。
- 每次 main 变化后保存 catchup 轮数，再次启动从外部 Progress 恢复；两轮转人。correction 沿用状态机三次上限，第四次失败转人。返回 correction 的 Progress 与执行器归属同一评论写入并确认，然后调度器启动终端；不确定启动保留归属，由既有巡检恢复。
- `Executor.Start` 在既有 PR 短路前消费当前 handoff 的 correction。重用原 run/task/worktree 和累计预算，旧 task 没有 dispatch 时不虚构它；修复 worker 的新 dispatch 正常保存。旧 checkpoint 增加可选 `MergeQueue/MergeHead/MergeCause/MergeSpec/CatchupRounds/Attempt/Infra`，仅用于已有派工恢复，不成为新的合并事实数据库。开始子任务前清除 MergeCause 并持久 pending，重启不能重复创建修复任务；缺 checkpoint 时从外部 handoff/progress 重建，并先沿 Orca task-list 的 parent_id 对账 correction 子任务；运行中、缺失或歧义证据停止，不能以新终端 UUID 重派。
- 合并结果 gate 使用本次实际 lane 验证，允许 main 已吸收 lane 2 改动后降至 lane 1；旧交接不改写，lane 3、lane 升级和未批准的仓库策略仍不放行。
- 修复必须产生不同 head；重跑冻结完整 gate 与 review 后生成新 handoff/FIFO 身份，保留 catchup 与 correction/Infra 预算，进入队尾。相同 queue_id 的不同冻结 spec 拒绝使用。

验证：`TestRuntimeTextConflictStartsCorrection`、`TestRuntimeOverlap*`、`TestRuntimeCatchupReceiptSurvivesCleanupAndCompletion`、`TestRuntimeCorrectionRequeueGoesBehindExistingTicket`、`TestRuntimeUnknownProgressWaitsWithoutOverwriting`、`TestStartMergeCorrection*`。临时 Git 仓库覆盖实际合并树、临时提交、worktree 与原版 gate.sh；GitHub/Linear/Orca 使用外部边界替身，不触碰线上资源。

## `driver usage` 会话用量

`driver usage <票号> --json` 的 JSON 保留账本使用的 `TicketUsage` 字段（`input`、`output`、`cache_read`、`cache_write`、`reasoning`、`cost_usd`、`ticket`、`by_role`、`by_model`、`price_version`、`apportioned`），新增 `items`、`skipped`；汇总 token 为整数，分项保留均分后的小数。`items` 按角色、模型和 `apportioned` 分项；每项含 `role`、`model`、`input_tokens`、`output_tokens`、`cache_read_tokens`、`cache_write_tokens`、`cache_write_hour_tokens`、`reasoning_tokens`、`cost_usd`、`unknown_model`、`apportioned`。金额是内嵌 `prices.json` 对应版本的标准 API 美元估算，不含订阅、工具和服务等级附加费；未知模型金额为 0，且 `unknown_model=true`。

Claude 读取 assistant 的 `message.usage` 并累加不同消息；同一消息 ID 的流式快照保留最后值。Codex 每个会话只取最后一个有效 `event_msg.payload.type=token_count` 的 `info.total_token_usage`，模型由 `turn_context.payload.model` 给出，缓存读写从总输入中扣除后单列；reasoning 是 output 的子集，单独记录但不重复计费。会话归属固定使用 session_meta 的 cwd，后续 turn_context 不改变累计用量归属。样例为 `usage-claude-session.jsonl`、`usage-codex-session.jsonl`、`usage-corrupt.jsonl`；字段对照本机真实会话，内容已脱敏。

会话 cwd 等于单元 worktree 或位于其子目录时归属该单元；使用 `filepath.Rel` 排除相似前缀目录。Codex 的单元会话记 `worker`，Claude 记 `session`。只有显式 `--reviewer-worktree <绝对路径>` 内的 Claude 会话记 `reviewer`，不自动发现 reviewer。多个目录嵌套时使用最接近 cwd 的目录。

coordinator 为可选归属：调用方同时给出 `--run-worktree <绝对路径>`、`--coordinator-session <提供方会话号>`、`--claimed-ticket-count <本次运行领取票数>`，仅匹配该目录和会话号的会话按领取票数均分，且 `apportioned=true`；未指定时只统计单元和显式 reviewer。读取只用只读打开与流式读行，超过 64KB 的行照常处理；某文件打开、读取、关闭或解析失败就舍弃该文件的全部用量，在 `skipped` 中给出 `path` 和 `error`，其他文件继续。结果仅写 stdout，不落盘。

`usage.Collect(ctx, ticket, worktrees, attribution...)` 保留既有调用签名与账本契约，最后一个可选 `Options` 仅补充 reviewer/coordinator 归属和会话根目录；不再保留旧 `CollectItems` 路径。

命令在当前仓库执行 `git worktree list --porcelain -z`，按 NUL 分隔读取 `worktree <路径>` 字段，保留路径中的空格、换行与非 ASCII 字符，仅选目录名以 `<票号>-` 开头的单元 worktree（例如 `ZII-894-executor`）；沿用命令的 30 秒 context 超时，不依赖 Orca API。Git 枚举失败时保留错误原因并退出 1；没有匹配且未显式指定 reviewer/coordinator 时返回带票号、价格版本和空分项的结果。`TestDiscoverUsageWorktrees` 使用临时 Git 仓库及两个不同票号的 worktree 验证筛选，JSON 命令测试继续注入 worktree 列表验证输出协议。
