# 外部契约

驱动器依赖外部系统的输出格式。下面每一条都曾在上线后造成故障，因为单元测试的手写假数据与真实格式不同。改动对应代码时，先看这里；排查调度器为什么领或不领、为什么重拉，先运行 `driver serve --dry-run`（只读一轮巡检，打印计划动作），测试读取 `testdata/recorded/` 中录制的真实样本；格式有新发现时，录制新样本并在此补一行。gate 要求通过的 `TestRecorded*` 契约测试数量不低于分支与 `origin/main` 分叉点上的定义数（`scripts/agent-gates/gate.project.sh`）。

| 契约 | 真实样本 | 测试 |
|---|---|---|
| `orca orchestration task-list` 的任务**没有** `dispatch_id` 字段 | `orca-task-list.json` | `executor.TestRecordedTaskList`、`executor.TestRecordedStartWithoutDispatchID` |
| Resolver `open-pr` 提案经 Orca `worker_report` 信封的 `body` 送达，必须是单个 JSON 对象；真实自由文本报告被拒绝。调度器核对分支 head 与 PR head，并在证据中保留 PR 号 | `orca-task-list.json`、`github-pr-open-clean.json`、`github-commit-main.json` | `resolver.TestResolverOpenPRResult`、`scheduler.TestResolverOpenPR` |
| Orca 变更请求重用 request-id 但输入（含调用终端）不同 → `request_mismatch`，不可重试 | `orca-run-create-request-mismatch.json` | `orca.TestRecordedRequestMismatch` |
| `orca terminal list` 的完整枚举字段（totalCount、truncated、hostScope） | `orca-terminal-list.json` | `scheduler.TestRecordedTerminalList` |
| Linear 存储正文时转义 CommonMark 字符（`*` 存为 `\*`，`\` 存为 `\\`） | `linear-description-escaped.md` | `executor.TestRecordedTicketDescriptions` |
| 票面「CHECK:」的下一行是「断言:」，不属于命令 | `linear-description-check-assertion.md` | 同上 |
| 小节标题可能带说明，如「## 要做（接口票）」 | `linear-description-heading-note.md` | 同上 |
| 「A 被 B 阻塞」存在 B 上；查 A 的 `inverseRelations`（type `blocks`）及阻塞者 `state.type` | `linear-inverse-relations.json` | `linear.TestRecordedInverseRelations` |
| 调度器认领时写 `term=- run=-`，表示执行器尚未登记 | `linear-comment-ownership-unregistered.json` | `scheduler.TestRecordedOwnershipUnregistered` |
| 审查员常把结尾的 verdict JSON 包在 Markdown 代码块里 | `reviewer-output-fenced.txt` | `executor.TestRecordedReviewerOutput` |
| 历史执行器转人评论固定三行；调度器仍以「转人:」识别旧归属，并按容量接管为 resolver；新回执含 Agent处置、证据、处置要求、阶段与归属 | `linear-comments-executor-handoff.json` | `scheduler.TestRecordedExecutorHandoff` |
| git 的网络失败（TLS 握手、超时、解析失败）在 stderr 中报告，可重试；HTTP 403、404 不可重试 | `git-stderr-tls-failure.txt`、`git-stderr-auth-failure.txt` | `infra.TestRecordedGitNetworkFailureIsTransient` |
| worker worktree 名为 `<票号>-<单元>`，本地分支是 Orca 起的（如 `vmxmy/ZII-893-executor`），`agent/<票号>/<单元>` 只是推送目标 | `git-worktree-list-porcelain.bin` | `executor.TestRecordedWorkerWorktree` |
| `claude -p` 的 `--allowedTools` 是可变参数，会吞掉后面的位置参数；审查员的说明与 diff 必须一起走 stdin | — | `executor.TestReviewSendsPromptAndDiffOnStdin`，`driver selftest --live` |
| `terminal send --wait-submit` 的回执 `send.prompt.stages` 含 `turn_started` 才证明 agent 开始了一轮；shell 等提供方只给 `input_accepted`（`observation: unsupported`） | `orca-prompt-receipt-codex-turn-started.json`、`orca-terminal-send-wait-submit-shell.json` | `orca.TestRecordedTerminalSendTurnStarted` |
| `orca worktree set` 成功回执带 `result.worktree`；选择器不匹配时拒绝码为 `selector_not_found`。看板卡片字段 `--display-name`、`--issue`、`--pr`、`--parent-worktree`、`--unread/--read` 回显为 `displayName`、`linkedIssue`、`linkedPR`、`parentWorktreeId`、`isUnread`；`--comment` 整体替换，各链接字段独立保留；未配置的 `--workspace-status` id 被原样接受、不报错（2026-10-12 实测） | `orca-worktree-set.json`、`orca-worktree-set-not-found.json`、`orca-worktree-set-card.json` | `orca.TestRecordedWorktreeSet`、`orca.TestRecordedWorktreeSetCard` |
| `worker-start --task --terminal <handle> --worktree path:` 复用已结算 worker 的终端：受理回执带 dispatchId、`stage=input_accepted`；终端忙时排队而非拒绝；handle 失效时拒绝码 `terminal_handle_stale`（回退起新 agent） | `orca-worker-start-reuse-terminal.json`、`orca-worker-start-reuse-busy-queued.json`、`orca-worker-start-reuse-rejected.json` | `executor.TestRecordedWorkerStartReuseTerminal` |
| 在已有 worktree 上 `worker-start` 等 agent 就绪可超过 30 秒（实测 38 秒）；驱动器的调用超时必须长于传给 Orca 的 `--timeout-ms` | `request-show` 实测（ZII-897 第 3 次 correction） | `orca.TestCommands`、`orca.TestFailedStartRetry` 断言 `--timeout-ms` |
| `orca orchestration worker-list` 的 `projection.liveness.verdict` 为 `exited` / `unverifiable`；`unverifiable` 不代表进程死亡；`resource.worktreeId` 形如 `<repoId>::<绝对路径>` | `orca-worker-list-liveness.json`（2026-10-06 从 Run `run_9714ea9b8576` 重录；仅保留契约字段并替换 worktree 路径） | `orca.TestRecordedWorkerListLiveness` |
| `orca orchestration worker-stop --dispatch` 对未结算的 dispatch 返回 `state: stopped`、`alreadySettled: false`、`processAction: closed_agent_terminal`；停后该 dispatch 在 `worker-list` 里变为 `failed` / `process_stopped` | `orca-worker-stop.json`（真实回执，2026-10-06） | `orca.TestRecordedWorkerStop`、`TestCommands` |
| Codex 启动崩溃（`account/read failed … workspace routing discovery timed out`）时 worker 的 `stage.detail` 为 `turn_start_unobserved`、`liveness.verdict` 为 `unverifiable`。**尚无录制文件**：2026-10-06 在 ZII-901 现场观察到，该观察本身不授权停止 worker；取消等待保留 checkpoint，重启后继续观察；下次再现时必须录下并补测试 | — | `executor.TestWaitUnobservedPreservesCheckpointAcrossCancellation`（合成值） |
| Linear 批量评论真实 GraphQL 别名包含非空与空历史，评论含 createdAt/updatedAt；完整 fresh issue 的空 labels/relations/inverseRelations 带 hasNextPage=false、endCursor=null。2026-10-07T16:44Z 原样捕获后对 ID/票号/游标做一致替换，评论和项目名称脱敏，保留字段、类型、数量和分页标志；request/response/provenance 成组保存。另一个完整 fresh issue 实录含一个 completed 的 inverseRelations 阻塞者；这些实录没有非空 labels/relations，非空标签/正向关系及分页续页边界另由合成测试覆盖 | `linear-batch-{request,response,provenance}.json`、`linear-fresh-{request,response,provenance}.json`、`linear-fresh-populated-{request,response,provenance}.json` | `linear.TestRecordedCommentsBatch`、`linear.TestRecordedFreshIssue`、`linear.TestRecordedFreshIssueWithInverseRelation` |
| Linear `issueUpdate` 移除票上没有的标签时拒绝整个更新（`Label not on issue`，INPUT_ERROR）；`UpdateIssue` 先读当前标签，只加缺少的、只删存在的 | `linear-remove-absent-label.json` | `linear.TestRecordedRemoveAbsentLabel` |
| 探测额度只需一次 `query { viewer { id } }`：响应头 `x-ratelimit-requests-limit/remaining/reset` 反映当前预算（Reset 是滑动窗口，约为现在加一小时，不是预算回满的时刻）；样本里 viewer id 已替换为全零 | `linear-viewer-probe.json`、`linear-response-headers.txt` | `linear.TestRecordedQuotaProbe`、`scheduler.TestQuotaPauseProbesWithoutSnapshotAndResumesWithHysteresis` |
| GitHub PR：`state`、`merged`、`mergeable_state`（合并后为 `unknown`）、`merge_commit_sha`（squash 后即 main 上的提交）；`compare/<base>...<head>` 的 `behind_by`、`merge_base_commit.sha` 与 `files`（最多 300，不标截断）；本仓库 CI 只有 GitGuardian 一个 check-run，旧式 commit status 为空且显示 `pending`，CI 只看 check-runs | `github-pr-open-clean.json`、`github-pr-merged.json`、`github-compare-*.json`、`github-commit-main.json`、`github-check-runs-gitguardian.json` | `github.TestRecordedMergeLaneReads`，`driver selftest --live` |
| squash 时 `sha` 与 PR head 不符返回 409 `Head branch was modified`，不合并；草稿 PR 返回 405 | `github-merge-head-modified-409.json` | `github.TestRecordedMergeHeadModified` |
| Claude 会话每条 `assistant` 记录带 `sessionId`、`cwd`、`message.id`、`message.model`、`message.usage`（`input_tokens`、`output_tokens`、`cache_read_input_tokens`、`cache_creation_input_tokens`，含 `cache_creation.ephemeral_5m/1h_input_tokens`）；同一 API 消息会在多条记录里重复出现，按 `message.id` 只计一次 | `claude-session-assistant.jsonl` | `usage.TestRecordedSessionFormats` |
| Claude 子代理的转录在 `<session>/subagents/agent-*.jsonl`，`sessionId` 与父会话相同，`isSidechain` 为真，`cwd` 为子代理所在目录 | `claude-session-assistant.jsonl`（第 2 行） | `usage.TestRecordedSessionFormats`、`usage.TestCollectCountsSubagentTranscripts` |
| Codex rollout 以 `session_meta`（`payload.id`、`payload.cwd`）开头，`turn_context` 给 `payload.model`，`token_count` 的 `total_token_usage` 按会话累计（含 `cached_input_tokens`、`cache_write_input_tokens`、`reasoning_output_tokens`），一个会话只计一次；`input_tokens` 含缓存读写，需先减去 | `codex-session-token-count.jsonl` | `usage.TestRecordedSessionFormats` |
| 价格表里没有的模型按 `unknown` 记录，不估价 | — | `usage.TestCollect` |
| 飞书 `base/v3` 记录列表按列返回（`fields`、`field_id_list`、`field_type_list`、`record_id_list`、`data`），带 `has_more` 与表级 `rev`；同一轮各页 `rev` 不同则整轮作废 | `feishu/tickets-list-filter-page1.json`、`page2`、`feishu/bot-tickets-list-filter.json` | `feishu.TestRecordedListPagesCarryRev`、`issues.TestRecordedFeishuBotEventsUpdatedAtBackfilled`、`issues.TestFeishuReadSnapshotFailsWhenPageRevDiffers` |
| 飞书写后立即读到旧值（`rev` 3 仍为 Todo），稍后读到新值（`rev` 4）；写响应只有 `record_id_list`，不带 `rev`。认领靠读回确认，超时为结果未知 | `feishu/tickets-batch-get-after-write.json`、`feishu/tickets-batch-get-settled.json`、`feishu/concurrent-update-1.json` | `issues.TestRecordedFeishuWriteThenReadStale`、`issues.TestFeishuWriteReadBackTimeoutUnconfirmed` |
| 飞书写只读字段时请求成功，响应 `ignored_fields` 带字段 ID 与原因；后端把它当作写入失败 | `feishu/tickets-create.json` | `feishu.TestRecordedBatchCreateIgnoredFields`、`issues.TestRecordedFeishuIgnoredFieldsRejected` |
| 飞书应用缺 scope 返回 `99991672` 与 `missing_scopes`；按全局故障处理，不重试 | `feishu/bot-scope-denied.json` | `feishu.TestRecordedMissingScopeNotRetried` |
| 飞书应用有 scope 但未以 edit 加为 Base 协作者：读成功，写返回 `91403`，读记录历史返回 `code 1`；加为协作者后历史操作人显示应用名。`91403` 不重试，保留错误码，按全局错误归类。2026-10-15 录制（临时移除协作者后以应用身份写入） | `feishu/bot-no-collaborator-write.json`、`feishu/bot-history.json`（加为协作者后） | `issues.TestFeishuNoCollaboratorWriteNotRetried`、`feishu.TestNoPermissionIsGlobal` |
| 飞书事件 `修改时间` ≠ `创建时间` 即证据冲突。后加 `修改时间` 字段时，旧行回填为真实修改时间：未改过的行两者相等，被改过的行晚于创建时间 | `feishu/bot-events-list.json`、`feishu/bot-events-list-ticket-b.json` | `issues.TestRecordedFeishuBotEventsUpdatedAtBackfilled`、`issues.TestFeishuReadCommentsRejectEditedEvent` |
| 飞书关联字段筛选一次传多个记录 ID（`intersects`），一次取回多张票的事件 | `feishu/events-list-multi-ticket.json` | `issues.TestRecordedFeishuMultiTicketEventFilter` |
| 飞书 `batch_delete` 总是返回 `ok`：目标已删除时响应只有 `record_id_list`；目标尚未可见（如刚创建）时同一 ID 出现在 `record_not_found`，且没有删除任何行。删除后 `batch_get` 把该 ID 列入 `record_not_found`。删除方必须检查 `record_not_found` 并在可见后重试 | `feishu/bot-tickets-delete.json`、`feishu/bot-tickets-delete-too-early.json`、`feishu/bot-tickets-get-deleted.json` | `feishu.TestBatchDeleteRetriesNotFound` |
| 飞书自动编号是字符串（`NO.004`）；删除最大编号后新建不复用 | `feishu/tickets-get-D.json`、`feishu/bot-tickets-list-advperm.json` | `issues.TestRecordedFeishuAutoNumberNotReused`、`issues.TestRecordedFeishuAdvancedPermissionUnfiltered` |
| 飞书 tables 列表为 `tables[]{id,name,records_count,rev}` 与 `total`；后端按表名定位，字段按 ID 引用并校验类型与选项 | `feishu/bot-tables-list.json`、`feishu/tables-list.json`、`feishu/fields-list.json`、`feishu/bot-fields-list-events.json` | `issues.TestRecordedFeishuBotSchemaAndTables`、`feishu.TestRecordedFieldsAndSchema`、`feishu.TestTablesListsIDsByName` |
| 飞书开启高级权限、未给应用分配受限角色时，应用（edit 协作者）读到全部行与字段（`record_scope: all_records`），写入成功 | `feishu/bot-tickets-list-advperm.json` | `issues.TestRecordedFeishuAdvancedPermissionUnfiltered` |
| 飞书写入以 field ID 为键生效（字段改名不影响）；应用身份可更新票据、追加事件 | `feishu/bot-tickets-update.json`（字段名写入；ID 写入为人工实测）、`feishu/bot-tickets-batch-get.json`、`feishu/bot-events-create.json` | `issues.TestRecordedFeishuBotWritesKeyedByFieldID` |

部署新二进制前运行 `driver selftest --live`：对真实的 Orca、Linear、git 与审查员命令只读地复核上述契约（只调用一次审查员）。

录制样本前删掉终端预览等可能含他人会话内容的字段，并确认不含任何凭据。

执行器派工恢复保留旧 `executor.json` 字段及请求 UUID 的终端作用域；新增可选 `PendingParent` 记录未确认的 correction 子任务创建。首轮与 correction 都在变更前写入 pending，每次新 dispatch 清除上一轮的完成与释放资格；回执伴随错误返回时仍保留可用的 task、dispatch 和 worktree 身份。

当前 task 的权威报告优先于旧提交的 `agent-status: complete`。pending 派工不能由旧 complete 或已有 PR 放行；缺失、歧义或显式截断的任务证据只允许停止对账，不授权重放旧终端请求。`task-list` 缺失 `dispatch_id` 时仍可根据 task 报告恢复，但不会臆造 dispatch 来释放 worker。子任务通过真实 `parent_id` 与 pending parent 对账；尚无权威报告的未确认派工保持 pending，待后续重新启动对账。

## GitHub Issues（#172 / #173）

- `internal/issues.Client` 选择唯一后端，暂复用已交付 `linear.Issue/Comment` 类型，不更改旧 UUID/ZII、checkpoint 或 mutation UUID。GitHub repo/number/URL 是地址，不是 canonical 身份。
- REST Issues/Comments 完整分页，过滤 pull_request，校验分页 Link。合法 Link 可由 `/repos/<owner>/<repo>/issues` 切换到 `/repositories/<id>/issues`；通过仓库元数据 `id/full_name` 验证同一仓库，保留 after 游标及固定查询，拒绝跨 origin、集合、仓库或非递增页码。2026-10-08 只读录制见 `github/issues-pagination-link.json`（仅头、仓库身份与 provenance，无票正文）。发现可缓存，关键动作完整 fresh 回读；评论编辑不因 issue 更新时间不变而被遗漏。
- 评论编辑不保证改变 Issue 的 `updated_at`；发现逐页条件请求评论并校验 ETag，304 保留已验证的分页 Link，不以 Issue 时间戳跳过评论请求。重复 `after` 游标拒绝整个 Issue/评论集合，不发布部分快照；ETag 复用响应正文，不保证减少实际请求或配额计数。
- 正常原生票由稳定 node ID 建立 canonical ID。未纳管票可见但不领取；adopt 校验正文和完整 canonical 依赖，并拒绝 held、重新打开的终态或关系不完整的依赖票。未提供完整依赖清单的原生 create 仅接受默认 Todo，非 Todo 状态在写入前拒绝，不能静默忽略。GitHub 原生更新时间与迁移来源时间独立。
- `LedgerStore` 仅创建精确账本标题、无标签且具有完整元数据的账本票，不伪造任务 CHECK/SCOPE。400 评论轮转保留旧 canonical ID、关闭旧票并将新记录写入下一张 Backlog 账本；普通任务不通过该入口绕过 adopt。
- open/closed/state_reason、工作流标签与元数据确定映射。closed/held 阻执行，不隐藏历史部署屏障；auto-failed 只进入合法 resolver。业务 PR 不使用 Closes/Fixes 提前关闭票。
- 两种后端在 claim、resume、launch、resolver、squash、release 及 executor 派工前都直接读取票据与评论；发现投影只用于选择，决策或启动前丢弃该票投影。Linear auto-failed 同样只进入 resolver。prune 要求分支票号、Orca 关联与评论关联指向同一 canonical 任务，冲突或读取失败即保留 worktree。
- export 只读且验证完整源与凭据形态，preview 离线另验证目标编码，import 默认 held。已有原生票通过 node ID 派生身份识别，不因 GH 编号被误作清单外来源票；真正导入身份冲突仍拒绝。v1 历史批次保持原字节协议；单条超限评论用 `orca-import-comment-v2` 分片携带完整 Comment JSON 的 base64、原 ID、片序/总数及全量 SHA-256，每条目标正文 ≤50,000 UTF-8 字节。完整分页收齐后才还原，缺片/重排/冲突拒绝；同 manifest、held 导入可按精确目标前缀恢复不确定写入。保留来源作者、ID、时间和顺序，不伪造原作者。release-hold 需 manifest、--apply、draining 与完整回读通过；最终导入及释放期间 scheduler 和旧写者必须停止写入，frozen 本身拒绝所有导入写入。`import --apply` 与 `release-hold --apply` 先取得 `driver-serve.lock` 并要求 draining，serve 运行时拒绝且零写入。export 最终校验失败时不返回任何清单；项目外 blocker 不在导出集合内，按依赖不完整 fail-closed 拒绝，不猜测或截断。export、preview、import 对标题、描述与评论扫描凭据形态，命中即拒绝，只报告 identifier 与评论序号，不回显内容。回读不核对预算：checkpoint、correction 与预算在本地状态中按源 UUID/Identifier 延续，导入与释放不读写它们。
- Linear 完整导出逐页读取包含归档票的正文、父票和调度字段，复用 fresh 连接校验补齐 labels/relations/inverseRelations；评论按最多五票一组完整分页并保留来源作者，不再逐票重复读 fresh、正文与评论。无嵌套续页且每票评论不超过100条时，N票使用 `ceil(N/20)+ceil(N/5)` 次数据请求，另加元数据页；额外关系/评论续页按实际增加，任何缺失或失败不返回完整清单。123票合成夹具含一次逆关系续页和一次评论续页，共34次数据请求（不含3次初始化）；不是线上测量或任意历史大小的固定上界。
- `ExportIssues` 缺少 operation 末尾 `}` 时，真实 Linear 返回 HTTP 400 / `GRAPHQL_PARSE_FAILED` / `Expected Name, found <EOF>`。2026-10-08 实录与来源见 `linear-export-parse-error{,.provenance}.json`。公开导出夹具检查 parent、nodes、pageInfo、issues 与 operation 的固定收尾，不能只匹配操作名就接受畸形请求；成功数据仍是合成夹具，不代表修复后线上导出成功。
- maintenance 持久化本机 drain/freeze/running，独立于 quota。freeze 只能证明本机合作锁无活跃写者，不能声明跨机器单写者；生产切换仍须明确授权与全量事实核验。
- `testdata/recorded/github/` 中样本记录来源，合成行为夹具不冒充线上录制。既有 Linear recorded 与 executor/scheduler goleak 保留。

## 合并交接（#141）

持久事实保存在配置选定的票据主账本评论与 GitHub PR；本地没有新增合并数据库。类型定义见 `internal/merge/evidence.go`。

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

## 合并结果验证与 correction（#142）

- head 落后 main 即验证合并结果，不再按两侧改动路径是否重叠决定；路径不相交的改动同样可能合起来出错。

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

Claude 读取 assistant 的 `message.usage` 并累加不同消息；同一消息 ID 的流式快照保留最后值。Codex 对相邻有效 `event_msg.payload.type=token_count` 的 `info.total_token_usage` 累计计数求差，将每段新增用量归到当时 `turn_context.payload.model`；重复快照（含模型切换后的重放）不增加用量，同一模型的多段合并，合计仍等于最终累计值。每段缓存读写从输入中扣除后单列，reasoning 必须是该段 output 的子集且不重复计费；计数回退或分段计数无效时整文件记入 skipped，不产生负数或重复费用。会话归属固定使用 session_meta 的 cwd，后续 turn_context 不改变累计用量归属。样例为 `usage-claude-session.jsonl`、`usage-codex-session.jsonl`、`usage-corrupt.jsonl`；字段对照本机真实会话，内容已脱敏。

会话 cwd 等于单元 worktree 或位于其子目录时归属该单元；使用 `filepath.Rel` 排除相似前缀目录。Codex 的单元会话记 `worker`，Claude 记 `session`。只有显式 `--reviewer-worktree <绝对路径>` 内的 Claude 会话记 `reviewer`，不自动发现 reviewer。多个目录嵌套时使用最接近 cwd 的目录。

coordinator 为可选归属：调用方同时给出 `--run-worktree <绝对路径>`、`--coordinator-session <提供方会话号>`、`--claimed-ticket-count <本次运行领取票数>`，仅匹配该目录和会话号的会话按领取票数均分，且 `apportioned=true`；未指定时只统计单元和显式 reviewer。读取只用只读打开与流式读行，超过 64KB 的行照常处理；某文件打开、读取、关闭或解析失败就舍弃该文件的全部用量，在 `skipped` 中给出 `path` 和 `error`，其他文件继续。结果仅写 stdout，不落盘。

`usage.Collect(ctx, ticket, worktrees, attribution...)` 保留既有调用签名与账本契约，最后一个可选 `Options` 仅补充 reviewer/coordinator 归属和会话根目录；不再保留旧 `CollectItems` 路径。

命令在当前仓库执行 `git worktree list --porcelain -z`，按 NUL 分隔读取 `worktree <路径>` 字段，保留路径中的空格、换行与非 ASCII 字符，仅选目录名以 `<票号>-` 开头的单元 worktree（例如 `ZII-894-executor`）；沿用命令的 30 秒 context 超时，不依赖 Orca API。Git 枚举失败时保留错误原因并退出 1；没有匹配且未显式指定 reviewer/coordinator 时返回带票号、价格版本和空分项的结果。`TestDiscoverUsageWorktrees` 使用临时 Git 仓库及两个不同票号的 worktree 验证筛选，JSON 命令测试继续注入 worktree 列表验证输出协议。
## 外部调用日志（ZII-900）

- `runx.WithLogger(ctx, logger)` 向 runx、Linear、GitHub、Orca 传递可选 logger；所有记录经过 `logx` 脱敏处理器。未传或传 nil 时静默，日志写失败不改变调用结果。serve（非 dry-run）与 exec 将已有日志 sink 接入 context。执行器自身的初始化错误、状态转移、恢复与外部步骤事件也复用该 sink，带票号写入 executor.log，供 driver logs 读取；没有 context logger 时继续使用执行器 Stderr（默认 os.Stderr）。
- runx 每次记录 executable、脱敏 args、exit_code、duration、stderr（脱敏后最多 2048 字节，UTF-8 完整）与 timed_out；成功也记录 stderr，启动失败为 -1。诊断采集保留最多首尾各 32 KiB，与 exec.Output 的退出错误诊断保持一致；Orca 同时记录进程诊断与业务回执。
- 自由文本选项（含 Claude -p/--print prompt，以及 objective/spec/setup/text/comment/command/body/title）、凭据选项、Authorization 头及 URL 用户信息不写入日志。带 -p/--print prompt 的命令，其 stderr 日志整体脱敏，防止大 prompt 被截断或仅回显片段后泄漏；返回给调用方的原始诊断不变，退出码和耗时仍记录。Linear 不记录查询正文、变量或授权头；记录 operation/status/duration/X-Complexity/X-RateLimit-Requests-Remaining，RATELIMITED 为 WARN。GitHub 记录接口（去除 query）、状态、耗时及 X-GitHub-Request-Id；Orca 写操作使用重试 UUID，读操作使用回执 id。
- 敏感参数的值在其他参数或 stderr 中重复出现时同样替换为 `[REDACTED]`；`runx.TestExternalCallLogRepeatedCredential` 重放 `git-stderr-auth-failure.txt`，验证失败退出码、录制诊断与重复凭据脱敏。
- `linear-response-headers.txt` 为已有 viewer 探测的真实响应头；`github-response-headers.txt` 为 2026-10-06 对 `https://api.github.com/repos/vmxmy/orca-factory` 匿名只读 GET 的真实 404 响应头，仅保留状态、日期、类型、请求 ID 和额度字段。测试重放这些头，HTTP 状态变化为合成错误/缓存场景，不将其冒充额外线上录制。
- 验证：runx/Linear `TestExternalCallLog`，Orca `TestGitHubRequestLog`、`TestOrcaReadRequestID`、`TestOrcaFailureProcessLog`，命令入口 `TestExecCompatibility`。真实请求头来自上述录制文件，测试不依赖网络。
## Resolver 处置

- 开 PR 结果格式：`{"action":"open-pr","branch":"agent/OF-5/executor","head":"c4e86ec5465d1e2d1f6aea45053e663b54a1350b","title":"修复标题","body":"修复说明","reason":"gate 已通过"}`。五个载荷字段都必填，head 为 40 位十六进制 SHA，reason 保持单行。`resolver.TestResolverOpenPRResult` 读取 `orca-task-list.json`，保留真实 `worker_report` 信封，仅替换其 `body`，并从 `github-pr-open-clean.json` 读取示例 head；`scheduler.TestResolverOpenPR` 读取该 PR 样本和 `github-commit-main.json` 覆盖 GitHub 响应。
- 调度器核对远端分支 head 后按分支 FindOpenPR，无已有 PR 才 CreatePR。复用已有 PR 时，正文前缀须匹配当前票据链接及 canonical identifier / ID；旧 Linear 配置核对 `Fixes <票号>`。统一票据存储的新 PR 正文首行为票据关联链接，后附 canonical identifier / ID；旧 Linear 配置为 `Fixes <票号>`，与执行器一致。处置证据事件记录 PR 号和 head，并以 waiting 状态开启下一次处置，等待 CI 后提交 merge handoff。
- 分支读取失败、head 不符、PR 查询或创建失败、PR 回执 head 不符均写证据并转人，resolver-state 的 human 状态禁止自动重试；需要人工核实并明确更新处置回执后才能继续。事件写入失败则保留原回执以便对账，已有 PR 按分支复用。`scheduler.TestResolverOpenPR` 覆盖这些路径。

- `resolver-state` 保存派工前意图、冻结角色/模型、原执行 Run、当前 task/dispatch、报告和退避时间。仅在报告持久化后释放 worker。
- mutation 请求按终端作用域生成；替换终端读取完整 task-list/worker-list 对账，不重放旧终端请求。丢失 Run 回执时尚未允许派工，可创建仅属于替换控制器的空 Run。
- task-list 缺 dispatch_id 是合法的；已有身份用于释放，缺身份时须从 worker-list 的 taskId/runId/dispatchId 关联恢复。录制样本 `orca-resolver-worker-list.json` 来自真实复核 worker 的只读列表，保留完整行集合并移除预览、目录等非契约字段；`resolver.TestRecordedWorkerAssociationRecoversReplacement` 覆盖该契约。
- 原执行器退出后允许独立处置 Agent 排查保留的 worker；原 Run 未确认停止推进前，调度器拒绝处置成功提案。失效部署提案退回限时重试，读写不确定则保留原身份继续对账。

`terminal list` 的 `lastOutputAt: null` 表示输出时间未知。驱动器保留该终端，但同一工作区存在未知输出时间时，不依据终端输出执行空闲催促或判定停滞；字段缺失或类型错误仍拒绝解析。真实录制 `orca-terminal-null-output.json` 保留混合时间戳及全部行数，脱敏方式和原始摘要见同名 provenance；`orca.TestRecordedTerminalUnknownOutput` 通过公开客户端重放。回归覆盖还包括 `orca.TestIdleDetectionTerminalList`、`orca.TestTerminalForWorktreeUnknownOutput` 和 `executor.TestIdleUnknownOutputDoesNotNudge`。
