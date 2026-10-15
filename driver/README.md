# orca-factory driver

这是用于取代 LLM coordinator 的 Go 驱动器，包含调度器、单票执行器、合并通道、部署屏障、用量账本和处置 Agent；整体演进计划见地图 [ZII-826](https://linear.app/ziikoo/issue/ZII-826)。

调度器推进已验证的队首 PR：head 落后 main 时先对固定 head/main 的合并结果跑完整 gate，未落后时直接以 head CI 为准；GitHub 回读确认 squash 后按该 SHA 验证部署及 smoke，全部通过才写回 Done、放行下一 PR。合并通道自己保证合并结果基于最新 main，因此 main 的分支保护不得开启“合并前须与 main 同步”（`required_status_checks.strict`），否则落后的 PR 永远不会变成 `clean`。交接、合并 intent 和部署进度存在配置选定的票据主账本评论，重启按冻结身份恢复；缺验证证据、CI 未明确成功或 manual 路径继续等待。合并结果 gate/correction 的 #142 接点与持久格式见 `CONTRACTS.md`。

`driver serve --dry-run` 不合并、不写回、不 fetch；scope 使用已读取的声明。旧票检查若需要 fetch 会打印省略原因并报告等待，不据此推断可以释放或重派。

## GitHub Issues 主账本

项目配置设置 `AGENT_ISSUE_BACKEND=github` 与 `AGENT_GH_REPO=owner/repo`。GitHub 模式不加载 Linear team/project/key、不调用 `orca linear` 或 `--linear-issue`，无双写或自动 fallback。旧 Linear 配置继续遵守已交付契约，不能据此静默切回。

`driver issues snapshot/read/create/adopt/update/comment` 是统一 GitHub CLI。原生未纳管票可见但不领取；adopt 校验「要做」、CHECK、SCOPE 和正文 `DEPENDENCIES` 的完整 canonical ID 数组。旧 UUID/ZII 与 checkpoint/receipts 身份不变，GitHub repo/number/URL 独立。发现缓存与历史来源时间不授权关键动作；认领、恢复、resolver、proposal、squash 和 smoke 都使用完整 fresh 证据。

关闭或 ImportHold 禁止新执行/合并，历史部署屏障仍在 FIFO 中观察对账。auto-failed 不普通重试。任务 PR 引用 Issue URL，不用 Closes/Fixes 提前关闭票。看板与 status 的 GitHub request 额度独立于 Linear complexity。

迁移及生产切换见 [票据主账本](../docs/agents/issue-tracker.md)。`driver maintenance drain|freeze|resume|status` 保存本机维护状态；drain 禁新派工，freeze 需本机合作 sessions/writes 空闲并阻外部写入。这不证明远端写者已退出。真实导出、held import、release-hold 和配置切换分别授权，不因为部署二进制就切 backend。`driver issues backend` 只读输出当前进程选定的后端与 `AGENT_GH_REPO`。

GitHub 配额保护读取 rate-limit 响应头：低额度暂停调度，reset 或无 reset 的退避期限后允许下一次发现刷新；403 权限错误不伪装成 quota。Linear 专用 viewer ProbeQuota 只在旧 Linear 路径运行，GitHub 不触发该探测。

## 飞书账本后端

飞书后端把票据账本放在一个飞书多维表格（Base）中。代码托管、PR 与合并通道仍走 GitHub。设计与字段定义见 [飞书多维表格票据账本](../docs/specs/feishu-bitable-ledger.md)。

### 初始化 Base

1. 新建一个 Base。用人的身份建表即可，可以用 `lark-cli`，应用不需要 `base:app:create`。
2. 建三张表：`项目`、`票据`、`事件`。字段名、类型和选项以规格第 4 节为准；driver 实际校验的清单在 `internal/issues/feishu.go` 的 `feishuTables`。
3. 在 `项目` 表加一行：`仓库远端` 填 `AGENT_FEISHU_PROJECT` 的值（如 `github.com/vmxmy/orca-factory`），填写 `编号前缀`，勾选 `启用`。

### 应用权限与协作者

1. 应用管理员在开发者后台为应用申请并发布规格第 9 节列出的 `base:*` 权限。
2. 把应用以 **edit** 权限加为该 Base 的协作者。只授予 scope 不够：未加协作者时读取成功，写入返回 `91403`，读记录历史返回 `code 1`。
3. Base 开启高级权限时，不要给应用分配受限角色。未分配角色的协作者应用读写全部行与字段（规格 Q4）。

### 配置

| 位置 | 配置项 | 说明 |
| --- | --- | --- |
| 仓库 `agent-graph.env` | `AGENT_ISSUE_BACKEND=feishu` | 选飞书后端 |
| 仓库 `agent-graph.env` | `AGENT_FEISHU_BASE` | Base URL（含 `/base/<token>`）或 base token |
| 仓库 `agent-graph.env` | `AGENT_FEISHU_PROJECT` | 本仓库在 `项目` 表的 `仓库远端` |
| 仓库 `agent-graph.env` | `AGENT_GH_REPO` | 仍用于代码托管、PR 与合并 |
| 机器 `driver.env`（服务的 `EnvironmentFile`） | `AGENT_FEISHU_APP_ID`、`AGENT_FEISHU_APP_SECRET` | 应用凭据。不进仓库，不写日志 |
| 机器 `driver.env` | `AGENT_FEISHU_ENDPOINT` | 可选。默认 `https://open.feishu.cn`；Lark 或私有化部署时改 |

启动时 driver 读取三张表的字段并校验。缺字段、类型不符或缺选项时启动失败，不领票。

### 可选群通知

1. 在应用开发者后台启用机器人能力，申请并发布 `im:message:send_as_bot` 权限，参见[发送消息接口](https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/message/create)。
2. 建立接收异常通知的飞书群，在群设置中添加该应用机器人。取得此群的 `chat_id`（`oc_...`），不要使用群名称、邀请链接或用户 ID。
3. 在机器 `~/.config/orca-factory/driver.env` 设置 `AGENT_FEISHU_NOTIFY_CHAT=oc_...`，复用已有应用凭据。单个仓库可在 `agent-graph.env` 设置同名值覆盖机器默认，设置空值可关闭该仓库通知。进程环境覆盖机器文件；仓库配置优先。`AGENT_FEISHU_BASE` 使用完整 Base URL，使消息带可点击的记录链接。
4. 按现有服务管理流程重新加载配置。此次功能提交不自动建群、加机器人、发测试消息或切换生产配置。

调度器观察到转人（含处置 Agent）、证据冲突或部署失败时发送票号、标题、类别、原因摘要及记录链接。未配置群时保持原行为。每个票据的同类别、同原因只尝试发送一次，发送前在事件表写去重标记；发送失败也保留标记，重启不会补发。事件读写失败不发送，下轮重新检查。消息发送失败只记 warn，不阻断调度。

暂停期间不通知。通知使用本轮快照已读取的评论判断原因和去重，不额外读取事件。快照因票级归属或处置凭证的证据冲突失败时，仍通知本轮已完整读取评论的异常票和其他待通知票，整轮快照继续报错并停止调度。合并证据冲突也会通知，包括没有执行归属的纳管票；底层事件读取失败时不绕过证据校验发送通知。

日志中的 `230001` 表示请求参数无效，应核对 chat_id；`230002` 表示机器人不在群内；`99991672` 表示缺少权限，应按错误列出的 scope 申请并发布。修复配置不会重发已有标记的通知。`serve --dry-run` 不发通知。

### 真实环境测试

`TestFeishuLive` 以应用身份在测试 Base 上跑一遍：建票并纳管、快照可领取、认领读回、写事件并逐字节读回、直接改 SCOPE 后票变为 held、直接改事件后读取报证据冲突，测试结束先删除本次创建的事件，再删除票；事件未完整发现或删除失败时报告错误并保留票，避免留下失去关联票的事件。

```sh
cd driver
set -a; . <机器 driver.env>; set +a
AGENT_FEISHU_LIVE=1 AGENT_FEISHU_BASE=<测试 Base> AGENT_FEISHU_PROJECT=<仓库远端> \
  go test ./internal/issues -run TestFeishuLive -count=1 -v
```

凭据只经环境变量传入，不写进命令行或日志。未设置 `AGENT_FEISHU_LIVE=1` 或缺任一变量时测试跳过。只对测试 Base 运行。

### 排查

| 现象 | 原因与处理 |
| --- | --- |
| `feishu API code 91403` | 应用未加为 Base 协作者，或权限低于 edit。按“应用权限与协作者”第 2 步添加。该错误不重试 |
| `feishu API code 99991672: missing scopes ...` | 应用缺 scope。按错误列出的 scope 申请并发布。该错误按全局故障处理，不重试 |
| `feishu table <表> schema mismatch: ...` | 字段缺失、类型不符或缺选项。按错误逐项修正 Base，再重启 driver |
| `feishu table <表> missing or duplicated` | 表名不是 `项目`、`票据`、`事件`，或有重名表 |
| `feishu ignored fields ...` | 写入了只读字段，说明字段类型被改过。核对表结构 |
| `feishu read-back not settled ...`、`write not confirmed` | 写入结果未知。飞书写后读有延迟，driver 不派工，下一轮再对账。持续出现时检查网络和飞书服务状态 |
| `feishu event ... evidence conflict` | 事件被人改过或关联到多张票。合并通道停下等待，需人工核对记录历史 |

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

上线时先完成检查与审查，保存旧二进制及其提交信息。部署顺序：

1. 运行 `orca-factory-driver maintenance drain`。
2. 用 `status --json` 等在途票归零。
3. 替换二进制并重启服务。
4. 运行 `orca-factory-driver maintenance resume`。

drain 成功后 serve 不再认领或派工，已有执行器与处置 Agent 继续收敛。drain 与 resume 只等待已准入的认领与派工结束，不等待普通外部写入；默认最多等 2 分钟，用 `--wait 30s` 等时长调整。等待期间新认领与派工被拒绝；超时报错且状态不变。保留 driver.toml、凭据、账本和在途执行器；失败时恢复旧二进制，不删除数据库、不清空 checkpoint、不强行解除配额暂停。验证服务存活、旧 Automation 关闭、连续调度周期和配额探测日志；单次 `status` 成功不代表端到端验收通过。

## 启动检查

- `~/.local/bin/orca-factory-driver status --json`：读取观察缓存并输出调度器状态快照，不修改任何状态；结果只是观察，不证明服务进程存活。
- `~/.local/bin/orca-factory-driver serve --dry-run`：按真实配置运行一次调度推演，不合并、不写回、不 fetch。

## Orca 看板

每张票的 worktree 在 Orca workspace 看板上有一张卡片，driver 在阶段变化时更新它：执行器转移、转处置、合并通道的等待/correction/转处置、部署屏障暂停、处置 Agent 派工与结论、合并完成。卡片显示 `<票号> <标题>`，GitHub 模式下链接 Issue 与 PR；评论为 `driver: <阶段>[ · 原因]` 加一行 `issue: <票号>`，prune 依赖这一行把 worktree 对回票。

看板每个 driver 阶段一列，列名即阶段名：

| 列 id | 列名 | 说明 |
| --- | --- | --- |
| `in-progress` | 手动工作区 | Orca 默认列，放非 driver 的 worktree；driver 不写 |
| `driver-unclaimed` | 待领 | 归属释放后仍保留 worktree 的票 |
| `driver-worker` | worker 运行中 | worker 在独立 worktree 中实现；worker 启动回执不带路径，driver 观察到 worktree 后才写卡片 |
| `driver-branch-check` | 分支检查 | |
| `driver-gate` | gate | |
| `driver-review` | 审查 | |
| `driver-correction` | 写 correction | 含合并通道要求的 correction |
| `driver-open-pr` | 开 PR | |
| `driver-merge-queue` | 排队合并 | 含合并通道等待 |
| `driver-resolver` | Agent处置（未读红点） | 含部署屏障暂停、执行器转处置 |
| `driver-await-human` | 等人批 | 历史回执中的“转人”也在此列 |
| `completed` | 已合并 | Orca 默认 Done 列；也放被处置 Agent 接替的旧 worktree |

`bin/orca-factory board-columns` 写入本机 Orca 客户端配置（`ORCA_DATA_FILE` 可指定 profile 文件）：列定义存在各客户端自己的 profile 中，不随服务端同步，因此每台打开该看板的 Orca 客户端（包括连接本机的远程客户端）都要各自执行一次。命令先备份，删除 `todo`、`in-review` 和旧的 `agent-*` 列，并把仍在这些列的卡片移到“手动工作区”。运行中的 Orca 把设置留在内存并写回文件，所以写入前必须停止 Orca 应用与 `orca serve`（终端 daemon 可以保留），命令检测到 Orca 进程时拒绝写入。Orca 以同目录的 `profile-state.db`（SQLite）为权威、JSON 只是副本，直接改 JSON 会让 Orca 拒绝启动；因此存在该数据库时，命令写完会执行 `orca profile state rollback --current-json`（`ORCA_CLI_COMMAND` 可指定 CLI，默认 `orca-ide`），由 Orca 归档旧数据库并在下次启动时导入 JSON。写完再启动 Orca，`--check` 只读核对。旧定时 coordinator 链路会写 `in-review`，不要在运行该链路的机器上执行此命令。Orca 接受未配置的列 id 而不报错；缺列的客户端把这些卡片显示在默认的 in progress 列，服务端记录的阶段仍然正确，可用 `orca-ide worktree list --json` 核对。看板只是镜像：拖动卡片不改变票状态，下一次阶段变化会覆盖它；写入失败只记 warn，不影响票流转。尚未领取的票没有 worktree，只在 GitHub Issues 中可见。

### 核对卡片

用下面的命令查看每个 worktree 的路径、`workspaceStatus` 和评论：

```sh
orca-ide worktree list --json |
  jq -r '.result.worktrees[] | [.path, .workspaceStatus, (.comment // "")] | @tsv'
```

driver 票的评论包含单独一行 `issue: <票号>`，用这行把卡片对应回票据。

## 隔离 CI runner 检测

本机存在 `~/.local/state/orca-go-isolated-runner` 且配置了 `vmxmy/orca-factory` 的 GitHub 主账本时，调度器每轮巡检读取 4 个 `orca-go-isolated-runner@N` 槽位：服务 failed、NRestarts 自上轮增加、租约停在 registering 超过 10 分钟，都算异常。任一异常持续 10 分钟，调度器按 intake 模板开一张带 `ready-for-agent` 的纳管票，写明证据和 `scripts/local-runner/README.md` 的维护步骤；同一故障只开一张，全部槽位恢复后在该票评论一次。建票失败时不重试，直到槽位恢复，避免重复开票。检测只读 systemd 与租约文件，不改 runner；状态记在 `~/.local/state/orca-factory/runner-health.json`。检测失败只记 warn，不影响票流转；`serve --dry-run` 不检测。

## Linear 实时批量发现

每轮仍读取全部配置项目及历史评论，按 client 隔离，每个 GraphQL 请求最多五个 issue alias、每票每页 100 条评论，串行完成每票独立分页。缺 alias、null、分页证据不全、循环 cursor、部分错误或取消都使整轮失败，不发布空队列或部分快照；移除标签的在途票和 Done 后的部署屏障仍保留。100 票、五页 issue、每票一页评论的 HTTP 夹具从每轮 105 次降至 25 次（不含初始化、探测与关键复核），不是线上节省率或复杂度下降证明。

普通发现每页先检查最近响应头中的 request/complexity 余量，保留 `pause_fraction` 配额底线，并为下一页预留保守成本（issue 页按每次查询上限预留 10000、连接页 1000、每个评论 alias 200 点）。即使余量仍高于百分比暂停/恢复阈值，页前拒绝也进入既有配额暂停和探测退避；只有成功的便宜探测才能解除这次页前拒绝，随后仍须满足原恢复阈值，探测失败或取消不触发完整扫描。这是进程内已知余量检查，不是跨进程预算协调；响应头中的实际 `X-Complexity` 继续记录。低成本探测与关键单票读取独立保留，轮次结果不授权合并、Done 或清理。

认领前重新读取单票状态、项目、完整标签/关系/阻塞连接及完整评论；候选资格变化、归属冲突、交接/部署屏障或未知证据都会阻止认领写入。此复核沿用单调度器领票约束，不提供 Linear 原子比较交换。旧适配器可继续使用实时 Snapshot 和逐票评论回退；生产 client 使用批量发现和定向单票复核。未引入持久缓存、数据库或 Webhook；不变轮次仍扫描历史，SQLite/Webhook 与跨进程预算验收仍属后续工作。

## 部署屏障与 Agent 处置

`AGENT_DEPLOY_WORKFLOW` 接受现有 workflow 显示名（含空格），也接受 workflow ID 或文件名。显示名必须唯一；GitHub 读取失败、空集合、截断或未知状态都等待。只观察既有 workflow，不触发部署或发布。`AGENT_SMOKE_CMD` 由 `bash -c` 在仓库目录执行，超时 30 秒；环境提供真实 `AGENT_MERGE_SHA` 与 `AGENT_GH_REPO`。只有 workflow 时仅等待 workflow；只有 smoke 时在确认 squash 后执行；两者都有时 workflow 成功才执行 smoke。无效配置不放行。

部署或 smoke 失败通过票据主账本评论把归属转为处置 Agent、暂停该仓库合并通道，其他仓库继续。配置在合并 intent 中冻结，移除配置、标记 Done/auto-failed、重启或恢复凭据均不解除已有屏障。smoke 执行前先写 `smoke-intent`；如果结果写回不能确认，后续 tick 调用 resolver 对账，不自动重放。

先用 `driver status --json` 或 `driver serve --dry-run` 查看票号、queue ID、squash SHA、状态及原因。前者是可过期的观察缓存，后者读取当前票据主账本的持久证据。两者均不写状态、不运行 smoke；dry-run 也不创建日志。配置错误或证据冲突在巡检错误中报告。

尚无 PR 时，处置 Agent 返回 `open-pr`，调度器核对远端 head、按分支复用或创建 PR，再开启下一次处置等待 CI 并提交 `merge`；分支或 PR 操作失败写证据并转人，不自动重试。

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

## 飞书需求入口网关

`orca-factory-driver feishu-gateway` 独立运行官方 Go SDK 长连接，订阅
`im.message.receive_v1`，断线自动重连，SIGTERM/SIGINT 取消连接并退出。
它只处理指定群内 @ 本机器人发起的新话题。名单外回复“无权限”；白名单内的消息交给
需求整理 Agent（角色 `scripts/agent-gates/roles/intake-chat.md`），Agent 的回复原文
作为话题回复。当前版本不创建票据（OF-15）。
已开通 `im:message.group_msg` 后，话题内无需 @ 也能继续对话：该话题已有网关自己开的
会话、会话未关闭、发送者在名单内，三者同时成立才续接；否则静默丢弃，不回复、不写
任何状态。开新话题仍须 @ 机器人；只按 `root_id` 是否存在判断话题是不正确的，
事件中的 `thread_id` 才是既有话题身份。对收到的 message_id 使用 `reply_in_thread=true`
即可创建或延续该话题。

每个话题是一次会话，以话题根消息 ID 为键：不带 `thread_id` 的消息自己成为根，
带 `thread_id` 的消息的 `root_id` 指向根。会话状态保存在状态目录的
`intake/<根消息ID>.json`（claude 会话 ID、需求人 open_id、状态、最后消息时间）。
同一话题串行处理；全局同时运行的 Agent 不超过 `AGENT_INTAKE_CONCURRENCY`，
其余排队并先回复“排队中”。7 天无消息的话题关闭，再 @ 时提示重新发起。
Agent 失败或超过 5 分钟时回复“处理失败，请重试”并在 stderr 记录，会话保留。

每轮执行一次 `claude -p --output-format json`：首轮 `--session-id <新ID>`，之后
`--resume <ID>`；消息正文经 stdin 传入。Agent 的安全边界：

- 工作目录是只读检出 `~/.cache/orca-factory/intake-checkout`，为来源仓库的分离 HEAD
  worktree；每个新会话开始前 fetch 并切到 `origin/main`。
- `--tools Read,Grep,Glob,Skill`，`--restricted`（文件工具限于工作目录，忽略用户、
  项目和本地 settings 及其 hooks），`--permission-mode dontAsk`（需授权的调用直接拒绝），
  `--strict-mcp-config`（不加载 MCP），`--settings` 再拒绝读取 `~/.claude`、`~/.config`、
  `~/.local/state`、`~/.ssh`。
- 环境只含 `PATH` 与 `HOME`；模型与 effort 通过 `--model`、`--effort` 传入。
  飞书、GitHub 凭据不进入 Agent 进程。
- `--restricted` 不加载用户技能目录，grilling 技能因此不可用；角色说明自带追问规则。

在飞书开发者后台启用应用机器人，将事件订阅方式设为“使用长连接接收事件”，添加
`im.message.receive_v1`，申请 `im:message.group_at_msg:readonly` 和
`im:message:send_as_bot` 权限，发布应用版本并完成管理员审批，再把机器人加入需求群。
无需配置公网回调地址，也无需申请读取全部群消息权限。参考
[官方 SDK](https://github.com/larksuite/oapi-sdk-go) 的长连接接口。

在 `~/.config/orca-factory/driver.env` 配置以下变量，文件权限设为 `0600`。
进程环境中的非空值优先于该文件；缺失必填项时仅打印变量名并退出。

| 变量 | 含义 |
| --- | --- |
| `AGENT_FEISHU_APP_ID` | 应用 ID，沿用 driver 凭据 |
| `AGENT_FEISHU_APP_SECRET` | 应用密钥，沿用 driver 凭据 |
| `AGENT_FEISHU_BOT_OPEN_ID` | 本机器人的 open_id，必须与事件 mentions 中的机器人身份一致，不是 app_id |
| `AGENT_FEISHU_INTAKE_CHAT` | 唯一允许的需求群 chat_id |
| `AGENT_FEISHU_INTAKE_USERS` | 可对话成员 open_id，逗号分隔 |
| `AGENT_FEISHU_ENDPOINT` | 可选，默认 `https://open.feishu.cn`，同时用于 HTTP 与长连接发现 |
| `AGENT_INTAKE_CLAUDE` | 可选，claude 可执行文件的绝对路径；默认在网关 `PATH` 中查找 `claude`。服务 `PATH` 不含 npm 全局目录时必须设置（取 bash 中 `command -v claude` 的结果，不要用 shell 别名） |

Agent 的来源仓库是 `driver.toml` 中唯一一个 `AGENT_ISSUE_BACKEND=feishu` 的仓库；
不是恰好一个时网关拒绝启动。该仓库 `agent-graph.env` 的以下变量生效：

| 变量 | 含义 |
| --- | --- |
| `AGENT_ROLE_INTAKE` | Agent 档位，格式 `claude:<model>:<effort>`；为空时用 `AGENT_TIER_STRONG` |
| `AGENT_INTAKE_CONCURRENCY` | 同时运行的 Agent 数，默认 2 |

机器人 open_id 可从已确认 @ 本应用的消息事件 `mentions` 中取得，核对
`mentioned_type=bot`；不要从任意用户提交的字符串配置该身份。
状态默认保存在 `~/.local/state/orca-factory/intake/`，设置
`AGENT_DRIVER_STATE_ROOT` 可改状态根目录。网关独占 `gateway.lock`，保留最近
10,000 个 message_id 到 `recent.json`，并在发回复前原子保存。重启不重发这些消息；
发送前崩溃或结果不确定时可能漏回一条，网关不会自动重发。保留状态文件，不要为重试清空状态。
SDK 日志不输出连接 URL、凭据或消息正文。

安装及启停：

```sh
orca-factory-driver install --dry-run
orca-factory-driver install
systemctl --user enable --now orca-factory-feishu-gateway.service
systemctl --user status orca-factory-feishu-gateway.service
journalctl --user -u orca-factory-feishu-gateway.service -f
systemctl --user stop orca-factory-feishu-gateway.service
```

`install` 仅安装二进制、写服务并 daemon-reload，不启动网关。网关通过
`EnvironmentFile` 读取上述机器配置。代理与 serve 一样使用 systemd drop-in：
`~/.config/systemd/user/orca-factory-feishu-gateway.service.d/proxy.conf`。
首次安装时，若已有 `orca-factory-driver.service.d/proxy.conf`，安装器复制其内容；
网关已有的代理文件不会被覆盖。后续代理变更分别维护两个服务的 drop-in，执行
`systemctl --user daemon-reload`，再重启需要应用变更的服务。网关服务不依赖 serve 启停。
