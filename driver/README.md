# orca-factory driver

这是用于取代 LLM coordinator 的 Go 驱动器，包含调度器、单票执行器、合并通道、部署屏障、用量账本和处置 Agent；整体演进计划见地图 [ZII-826](https://linear.app/ziikoo/issue/ZII-826)。

调度器推进无需合并结果 gate 的已验证队首 PR，GitHub 回读确认 squash 后按该 SHA 验证部署及 smoke，全部通过才写回 Done、放行下一 PR。交接、合并 intent 和部署进度存在配置选定的票据主账本评论，重启按冻结身份恢复；缺验证证据、CI 未明确成功、manual 路径或重叠继续等待。合并结果 gate/correction 的 #142 接点与持久格式见 `CONTRACTS.md`。

`driver serve --dry-run` 不合并、不写回、不 fetch；scope 使用已读取的声明。旧票检查若需要 fetch 会打印省略原因并报告等待，不据此推断可以释放或重派。

## GitHub Issues 主账本

项目配置设置 `AGENT_ISSUE_BACKEND=github` 与 `AGENT_GH_REPO=owner/repo`。GitHub 模式不加载 Linear team/project/key、不调用 `orca linear` 或 `--linear-issue`，无双写或自动 fallback。旧 Linear 配置继续遵守已交付契约，不能据此静默切回。

`driver issues snapshot/read/create/adopt/update/comment` 是统一 GitHub CLI。原生未纳管票可见但不领取；adopt 校验「要做」、CHECK、SCOPE 和正文 `DEPENDENCIES` 的完整 canonical ID 数组。旧 UUID/ZII 与 checkpoint/receipts 身份不变，GitHub repo/number/URL 独立。发现缓存与历史来源时间不授权关键动作；认领、恢复、resolver、proposal、squash 和 smoke 都使用完整 fresh 证据。

关闭或 ImportHold 禁止新执行/合并，历史部署屏障仍在 FIFO 中观察对账。auto-failed 不普通重试。任务 PR 引用 Issue URL，不用 Closes/Fixes 提前关闭票。看板与 status 的 GitHub request 额度独立于 Linear complexity。

迁移及生产切换见 [票据主账本](../docs/agents/issue-tracker.md)。`driver maintenance drain|freeze|resume|status` 保存本机维护状态；drain 禁新派工，freeze 需本机合作 sessions/writes 空闲并阻外部写入。这不证明远端写者已退出。真实导出、held import、release-hold 和配置切换分别授权，不因为部署二进制就切 backend。

GitHub 配额保护读取 rate-limit 响应头：低额度暂停调度，reset 或无 reset 的退避期限后允许下一次发现刷新；403 权限错误不伪装成 quota。Linear 专用 viewer ProbeQuota 只在旧 Linear 路径运行，GitHub 不触发该探测。

## 内置定时协调与恢复

`driver serve` 持有单实例锁并持续执行调度循环，负责发现、认领、执行器恢复和合并推进。它不需要 Orca Automation 定时启动协调 Agent；同仓库的旧 `agent-linear-scan` 必须保持关闭，开启或无法核实其状态都会阻止驱动器认领，避免两个协调者竞争。

排查停止推进时，先区分服务进程、调度周期和任务状态：

```sh
systemctl --user status orca-factory-driver.service --no-pager
journalctl --user -u orca-factory-driver.service -n 50 --no-pager
orca-factory-driver status --json
```

`status` 读取观察缓存，`ok` 不能单独证明服务进程存活；`stale` 表示上次完整观察已过期，配额暂停期间也可能出现。`globalPause.durationSeconds` 是已经暂停的时长，不是恢复倒计时。票行的 `backend` 是所在仓库选定的账本；`url` 与 `held` 只由 GitHub 后端提供；`blockedReason` 说明票为何不能推进（导入保留、合并证据问题、依赖证据不完整或未完成的 blocker），只是观察，不授权任何动作。日志中的 `snapshot_available=false` 表示本轮没有完整快照，不能把同时显示的 `candidates=0` 当成 Linear 没有任务。

配额低于暂停阈值时，调度器按退避间隔调用低成本 `ProbeQuota` 刷新响应头，达到恢复阈值后才继续完整发现；暂停期间不应通过重启或完整评论扫描来试探额度。Linear reset 时间不是固定整点恢复时刻。无候选票、仍有在途归属或合并屏障时，服务在线但不新派发可以是正常状态。

上线时先完成检查与审查，保存旧二进制及其提交信息，再替换程序并重启服务。保留 driver.toml、凭据、账本和在途执行器；失败时恢复旧二进制，不删除数据库、不清空 checkpoint、不强行解除配额暂停。验证服务存活、旧 Automation 关闭、连续调度周期和配额探测日志；单次 `status` 成功不代表端到端验收通过。

## Linear 实时批量发现

每轮仍读取全部配置项目及历史评论，按 client 隔离，每个 GraphQL 请求最多五个 issue alias、每票每页 100 条评论，串行完成每票独立分页。缺 alias、null、分页证据不全、循环 cursor、部分错误或取消都使整轮失败，不发布空队列或部分快照；移除标签的在途票和 Done 后的部署屏障仍保留。100 票、五页 issue、每票一页评论的 HTTP 夹具从每轮 105 次降至 25 次（不含初始化、探测与关键复核），不是线上节省率或复杂度下降证明。

普通发现每页先检查最近响应头中的 request/complexity 余量，保留 `pause_fraction` 配额底线，并为下一页预留保守成本（issue 页按每次查询上限预留 10000、连接页 1000、每个评论 alias 200 点）。即使余量仍高于百分比暂停/恢复阈值，页前拒绝也进入既有配额暂停和探测退避；只有成功的便宜探测才能解除这次页前拒绝，随后仍须满足原恢复阈值，探测失败或取消不触发完整扫描。这是进程内已知余量检查，不是跨进程预算协调；响应头中的实际 `X-Complexity` 继续记录。低成本探测与关键单票读取独立保留，轮次结果不授权合并、Done 或清理。

认领前重新读取单票状态、项目、完整标签/关系/阻塞连接及完整评论；候选资格变化、归属冲突、交接/部署屏障或未知证据都会阻止认领写入。此复核沿用单调度器领票约束，不提供 Linear 原子比较交换。旧适配器可继续使用实时 Snapshot 和逐票评论回退；生产 client 使用批量发现和定向单票复核。未引入持久缓存、数据库或 Webhook；不变轮次仍扫描历史，SQLite/Webhook 与跨进程预算验收仍属后续工作。

## 部署屏障与 Agent 处置

`AGENT_DEPLOY_WORKFLOW` 接受现有 workflow 显示名（含空格），也接受 workflow ID 或文件名。显示名必须唯一；GitHub 读取失败、空集合、截断或未知状态都等待。只观察既有 workflow，不触发部署或发布。`AGENT_SMOKE_CMD` 由 `bash -c` 在仓库目录执行，超时 30 秒；环境提供真实 `AGENT_MERGE_SHA` 与 `AGENT_GH_REPO`。只有 workflow 时仅等待 workflow；只有 smoke 时在确认 squash 后执行；两者都有时 workflow 成功才执行 smoke。无效配置不放行。

部署或 smoke 失败通过票据主账本评论把归属转为处置 Agent、暂停该仓库合并通道，其他仓库继续。配置在合并 intent 中冻结，移除配置、标记 Done/auto-failed、重启或恢复凭据均不解除已有屏障。smoke 执行前先写 `smoke-intent`；如果结果写回不能确认，后续 tick 调用 resolver 对账，不自动重放。

先用 `driver status --json` 或 `driver serve --dry-run` 查看票号、queue ID、squash SHA、状态及原因。前者是可过期的观察缓存，后者读取当前票据主账本的持久证据。两者均不写状态、不运行 smoke；dry-run 也不创建日志。配置错误或证据冲突在巡检错误中报告。

默认由 `driver resolve` 派发处置 Agent，提交绑定 queue/SHA 的结果后，常驻调度器在单实例锁内应用，无需人工重启。`AGENT_ROLE_RESOLVER` 可覆盖 strong 档；角色规范见 `scripts/agent-gates/roles/resolver.md`。

需要操作员主动恢复时，保留原命令：先停止 `driver serve` 服务/进程；真实恢复命令使用同一单实例锁，若服务仍在运行则拒绝写入。核对生产 workflow 的 SHA 和 smoke 的实际执行结果后，先只读验证（替换示例值）：

```sh
driver resume-deployment --issue ENG-1 --queue 'QUEUE_ID' \
  --sha SQUASH_SHA --action confirm-passed \
  --operator 'Alice' --reason '已核对 workflow run 及 smoke 记录，均通过' --dry-run
```

去掉 `--dry-run` 写入人工决定，然后重启 `driver serve`。命令只追加带操作者、理由、时间、动作的主账本 Progress 证据，不直接运行 smoke、合并或标记 Done；调度器在后续 tick 对账并推进。

- `confirm-passed`：人工确认该 squash 的所有部署与 smoke 验证都已通过；适用于 smoke 已执行但结果未持久确认。不会重跑 smoke。
- `retry-workflow`：人工处理部署失败后，重新查询同一 squash 的 workflow；若 smoke 可能执行过则拒绝此动作。
- `retry-smoke`：完成副作用对账、确认可安全重新执行后，明确授权再执行一次 smoke；配置了 workflow 时仍先检查它。

缺少操作者/理由、错误 queue/SHA、非待人工恢复状态或冲突证据均拒绝。响应丢失时先用 dry-run/票评论核对已保存的决定，不将命令退出失败当成尚未写入。原始矛盾/损坏证据需要人工核对，恢复命令不会覆写历史。

## 可调参数（driver.toml）

`~/.config/orca-factory/driver.toml` 除了 `max_in_flight`、`repositories`、`[token_bucket]` 之外，还可以在 `[scheduler]`、`[linear]`、`[github]`、`[orca]`、`[ledger]`、`[status]`、`[runx]` 各节里覆盖驱动器的硬参数。每节只写要改的字典，其余保留内置默认值。时长用 TOML 字符串（如 `"5m"`、`"30s"`），比例用 0 到 1 的小数。

```toml
max_in_flight = 20
repositories = ["~/github/orca-factory"]

[scheduler]
pause_fraction = 0.05       # 低于该比例暂停 Linear 相关调度
resume_fraction = 0.10      # 回到该比例恢复
quota_probe_backoff = "5m"  # 暂停后探测 Linear 额度的间隔
unregistered_grace = "3m"   # 认领后等执行器登记的宽限
adaptive_min_interval = "1m"
adaptive_max_interval = "3m"

[linear]
http_timeout = "30s"

[github]
http_timeout = "30s"

[orca]
worker_start_wait = "5m"    # worker-start 的 agent 就绪窗口
call_timeout = "30s"        # 其他 orca 调用的默认超时

[ledger]
flush_interval = "10m"

[status]
stale_after = "5m"          # 状态快照视为过期的年龄

[runx]
wait_delay = "100ms"        # 子进程退出的收尾等待
```

不写 `[tuning]` 各节时，所有值取上表默认值，行为与改动前一致。
