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
| Linear `issueUpdate` 移除票上没有的标签时拒绝整个更新（`Label not on issue`，INPUT_ERROR）；`UpdateIssue` 先读当前标签，只加缺少的、只删存在的 | `linear-remove-absent-label.json` | `linear.TestRecordedRemoveAbsentLabel` |

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
- #141 仅接通不需合并结果 gate、无部署要求的单 PR 路径。重叠与部署配置保持等待。#142 可在 `Runtime.advanceMerge` 的 Channel 组装处接合并结果验证与 correction；#143 可扩展 Progress 阶段，但必须从 Linear 原始项目快照/评论独立扫描，不能仅使用 Runtime 活动票过滤结果，否则 Done/转人后的部署记录会消失。
- `Executor.Start` 对已有 PR 仅复用匹配的完整冻结交接，保留原 FIFO 与预算，不新写旧三行归属。恢复/correction 实现应在该短路前处理待恢复派工；独立 helper 位于 `executor/merge_handoff.go`。

验证：`TestExecuteFreezesMergeHandoff`、`TestStartExistingPR*`、`TestRuntimeMerge*`、`TestServeRuntimeActuallyMergesAndDryRunIsReadOnly` 使用外部替身、HTTP 和临时 Git 仓库；这些是合成行为测试，不冒充线上录制样本。
