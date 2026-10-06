# orca-factory driver

这是用于取代 LLM coordinator 的 Go 驱动器，目前提供子命令注册表和 `version` 命令；整体演进计划见地图 [ZII-826](https://linear.app/ziikoo/issue/ZII-826)。

调度器推进无需合并结果 gate 的已验证队首 PR，GitHub 回读确认 squash 后按该 SHA 验证部署及 smoke，全部通过才写回 Done、放行下一 PR。交接、合并 intent 和部署进度存在 Linear 评论，重启按冻结身份恢复；缺验证证据、CI 未明确成功、manual 路径或重叠继续等待。合并结果 gate/correction 的 #142 接点与持久格式见 `CONTRACTS.md`。

`driver serve --dry-run` 不合并、不写回、不 fetch；scope 使用已读取的声明。旧票检查若需要 fetch 会打印省略原因并报告等待，不据此推断可以释放或重派。

## 部署屏障与人工恢复（#143）

`AGENT_DEPLOY_WORKFLOW` 接受现有 workflow 显示名（含空格），也接受 workflow ID 或文件名。显示名必须唯一；GitHub 读取失败、空集合、截断或未知状态都等待。只观察既有 workflow，不触发部署或发布。`AGENT_SMOKE_CMD` 由 `bash -c` 在仓库目录执行，超时 30 秒；环境提供真实 `AGENT_MERGE_SHA` 与 `AGENT_GH_REPO`。只有 workflow 时仅等待 workflow；只有 smoke 时在确认 squash 后执行；两者都有时 workflow 成功才执行 smoke。无效配置不放行。

部署或 smoke 失败通过 Linear 评论把归属转为人、暂停该仓库合并通道，其他仓库继续。配置在合并 intent 中冻结，移除配置、标记 Done/auto-failed、重启或恢复凭据均不解除已有屏障。smoke 执行前先写 `smoke-intent`；如果结果写回不能确认，后续 tick 转人对账，不自动重放。

先用 `driver status --json` 或 `driver serve --dry-run` 查看票号、queue ID、squash SHA、状态及原因。前者是可过期的观察缓存，后者读取 Linear 持久证据。两者均不写状态、不运行 smoke；dry-run 也不创建日志。配置错误或证据冲突在巡检错误中报告。

恢复前停止 `driver serve` 服务/进程；真实恢复命令使用同一单实例锁，若服务仍在运行则拒绝写入。核对生产 workflow 的 SHA 和 smoke 的实际执行结果后，先只读验证（替换示例值）：

```sh
driver resume-deployment --issue ENG-1 --queue 'QUEUE_ID' \
  --sha SQUASH_SHA --action confirm-passed \
  --operator 'Alice' --reason '已核对 workflow run 及 smoke 记录，均通过' --dry-run
```

去掉 `--dry-run` 写入人工决定，然后重启 `driver serve`。命令只追加带操作者、理由、时间、动作的 Linear Progress 证据，不直接运行 smoke、合并或标记 Done；调度器在后续 tick 对账并推进。

- `confirm-passed`：人工确认该 squash 的所有部署与 smoke 验证都已通过；适用于 smoke 已执行但结果未持久确认。不会重跑 smoke。
- `retry-workflow`：人工处理部署失败后，重新查询同一 squash 的 workflow；若 smoke 可能执行过则拒绝此动作。
- `retry-smoke`：完成副作用对账、确认可安全重新执行后，明确授权再执行一次 smoke；配置了 workflow 时仍先检查它。

缺少操作者/理由、错误 queue/SHA、非待人工恢复状态或冲突证据均拒绝。响应丢失时先用 dry-run/票评论核对已保存的决定，不将命令退出失败当成尚未写入。原始矛盾/损坏证据需要人工核对，恢复命令不会覆写历史。

## GitHub Issues task backend

Set `AGENT_ISSUE_BACKEND=github` and `AGENT_GH_REPO=owner/repository` in the
repository profile. Authentication uses the existing `gh auth token` path
(including `GH_TOKEN` support). An absent selector retains the shipped Linear
profile contract; an unknown selector fails. GitHub mode never initializes,
queries, writes, or synchronizes Linear. `AGENT_GH_API_URL` is an optional trusted
REST origin override for enterprise installations and offline integration tests.

The `issues` CLI reads these variables from its environment and writes one
`{"ok":true,"result":...}` JSON envelope to stdout; errors go to stderr with a
nonzero exit status. Files may be `-` to read stdin.

```sh
driver issues validate
driver issues snapshot
driver issues read --id ZII-42
driver issues create --input task.json
driver issues update --id ZII-42 --state 'In Progress' --add-label auto-impl
driver issues update --id ZII-42 --input patch.json
driver issues comment --id ZII-42 --body-file comment.txt
driver issues import --input complete-export.json          # read-only preview
driver issues import --input complete-export.json --apply  # explicit writes
driver ledger append '{"outcome":"no_ticket"}'
driver ledger stats --all
```

Snapshot/list returns `result.issues` and `result.complete=true` only after
complete issue pagination. Its comments connection is deliberately unread:
`commentsComplete=false` and `comments.pageInfo.hasNextPage=true`. Read returns
`result.issue` with complete logical comments and reconstructed owner/phase.
Both expose UUID `id`, canonical `identifier`, separate GitHub `number`/`url`,
title, description, estimate, priority, state `{id,name,type}`, Linear-shaped
labels/relations/inverseRelations connections, metadata completeness, parent,
source dates and `nativeUpdatedAt`. Missing dependencies and incomplete imports
never become claimable; failed labels do not automatically requeue work.

Create accepts `id` (optional UUID), `identifier` (optional), `title`,
`description`, `state`, `labels` (names), `priority`, `estimate`, `blockers`,
`related`, and `parent` (canonical UUIDs). Native identifiers are `GH-<number>`;
the UUID is assigned before POST and recovered by a readback after an uncertain
response, without repeating the write. Imported UUIDs and identifiers stay
unchanged. Update patches allow title, description, priority, estimate, blockers,
related and parent; identity and import provenance are immutable.

Managed issue bodies carry a versioned `orca-task-v1` JSON comment, fenced to
one repository. Native labels remain labels. Logical history preserves original
source comment IDs, bodies, timestamps and order, even when source history is
stored in bounded GitHub comment batches. Ordinary GitHub discussion remains
ordinary comments. REST calls use deadlines, ETags, pagination, write throttling,
rate-limit reset/Retry-After pauses, and no automatic write retries.

The import manifest is version 1 with `repository`, `complete:true`, and `issues`.
Each issue includes the complete create fields, original createdAt/updatedAt,
`importHold`, `comments`, `commentsComplete:true`, and `relationsComplete:true`;
empty arrays and nullable estimate must be explicit. Unknown fields, duplicate
keys/identities, missing references or incomplete history reject the entire
manifest before any write. Dry-run is default. Import has a bounded 30-minute command deadline while individual
REST requests retain their 30-second deadline. Apply validates source digests,
creates held issues, copies/verifies all histories, then releases holds; retries
reconcile the same source identities and comment batches. It never deletes source
data, resets execution budgets, changes frozen merge handoffs, or deploys services.

Runtime discovery reuses logical comment histories while native issue timestamps
are unchanged, with a five-minute repair limit. For N issues, an unchanged scan
uses ceil(N/100) issue-list requests (plus the terminal empty page when N is an
exact multiple of 100); changed histories add their paginated reads. Scope reads
reuse the snapshot index. Claim/ownership and merge decisions force fresh issue
and comment reads, so the discovery cache cannot authorize work. CLI snapshot
has no comment fanout; display consumers can fetch only the visible tickets.
