# 外部契约

驱动器依赖外部系统的输出格式。下面每一条都曾在上线后造成故障，因为单元测试的手写假数据与真实格式不同。改动对应代码时，先看这里，测试读取 `testdata/recorded/` 中录制的真实样本；格式有新发现时，录制新样本并在此补一行。gate 要求 `TestRecorded*` 契约测试数量不低于基线（`scripts/agent-gates/gate.project.sh`）。

| 契约 | 真实样本 | 测试 |
|---|---|---|
| `orca orchestration task-list` 的任务**没有** `dispatch_id` 字段 | `orca-task-list.json` | `executor.TestRecordedTaskList` |
| Orca 变更请求重用 request-id 但输入（含调用终端）不同 → `request_mismatch`，不可重试 | `orca-run-create-request-mismatch.json` | `orca.TestRecordedRequestMismatch` |
| `orca terminal list` 的完整枚举字段（totalCount、truncated、hostScope） | `orca-terminal-list.json` | `scheduler.TestRecordedTerminalList` |
| Linear 存储正文时转义 CommonMark 字符（`*` 存为 `\*`，`\` 存为 `\\`） | `linear-description-escaped.md` | `executor.TestRecordedTicketDescriptions` |
| 票面「CHECK:」的下一行是「断言:」，不属于命令 | `linear-description-check-assertion.md` | 同上 |
| 小节标题可能带说明，如「## 要做（接口票）」 | `linear-description-heading-note.md` | 同上 |
| 「A 被 B 阻塞」存在 B 上；查 A 的 `inverseRelations`（type `blocks`）及阻塞者 `state.type` | `linear-inverse-relations.json` | `linear.TestRecordedInverseRelations` |
| 调度器认领时写 `term=- run=-`，表示执行器尚未登记 | `linear-comment-ownership-unregistered.json` | `scheduler.TestRecordedOwnershipUnregistered` |
| 审查员常把结尾的 verdict JSON 包在 Markdown 代码块里 | `reviewer-output-fenced.txt` | `executor.TestRecordedReviewerOutput` |

录制样本前删掉终端预览等可能含他人会话内容的字段，并确认不含任何凭据。
