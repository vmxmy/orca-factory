# 票据主账本

本项目的统一目标是 GitHub Issues：`vmxmy/orca-factory`。#171 跟踪生产切换，#172 跟踪 driver 与导入，#173 跟踪脚本、看板与规范。代码合并或程序部署不等于生产账本已切换；实际后端以当前项目配置和完整验收证据为准。

## 统一入口

加载 `scripts/agent-gates/agent-graph.env` 后，通过 `scripts/agent-gates/agent-graph.sh issues` 读取或写入。GitHub 候选配置使用 `AGENT_ISSUE_BACKEND=github` 与 `AGENT_GH_REPO=vmxmy/orca-factory`，不要求 Linear team、project 或 key。GitHub 模式不调用 `orca linear`，不使用 `--linear-issue`，不双写、不自动回退。已交付的旧 Linear 配置仍可运行，但不能作为本仓库继续新增 Linear 事实的理由。

- `issues snapshot --json`：完整发现，包括未纳管、关闭和 held 票；发现缓存不授权执行。
- `issues read --id <canonical ID|票号|GitHub number> --json`：新鲜读取票与完整评论。
- `issues create --input <JSON>`：建票；正常原生票由 GitHub node ID 建立 canonical ID。
- `issues adopt --id <ID> --input <JSON>`：显式纳管；输入必须有完整 `blockers`、`related` 数组，正文的 `DEPENDENCIES` 必须与输入一致。
- `issues update` / `issues comment`：管理状态、标签和评论。未纳管票不能经管理更新入口写入。

票面须包含「要做」、`CHECK`、`SCOPE`。依赖使用 canonical ID，不拿 GitHub number 替换旧 UUID/ZII 身份。原生未纳管票可见但不领取。`auto-failed` 只由合法 resolver 处理，不清空预算后普通重试。任务 PR 首行引用 Issue URL，不用 `Closes` / `Fixes` 提前关闭业务票；部署与 smoke 确认后才 Done。

## 迁移与切换

1. 明确授权只读源导出，使用 `AGENT_ISSUE_BACKEND=linear` 的 `driver issues export`；不提取凭据绕过现有客户端。
2. 使用 `driver issues preview --input <manifest>` 离线验证目标、身份、依赖闭包、摘要与凭据形态；命中凭据形态时只报告票号与评论序号，须先处理再导入。历史快照不证明当前完整性。
3. 单独确认真实导入的数据范围、目标仓库可见性和敏感内容。
4. 使用 `driver maintenance drain` 停止新认领/派工，允许已有写者自然收敛。draining 仍允许合并、smoke、release 和评论写入；核对实际 worker、resolver 与旧 coordinator，不把 drain 当成停写证明。
5. 核实所有旧写者后执行 `driver maintenance freeze`，停止 scheduler，并再次确认执行器、resolver、自动化及其他机器不再写入，再完成最终导出与差异核对。旧二进制可能不持维护锁，锁空闲不能代替进程、Run 和完整派工回执核对；未知状态阻止切换。
6. scheduler 保持停止、旧写者保持隔离，将维护状态显式改回 `draining`，再执行 `driver issues import --input <manifest> --apply`。frozen 禁止所有 GitHub 写入，不能在 frozen 下导入，也没有导入绕过开关。导入票保持 held，完整回读后才允许继续。`--apply` 在 serve 持有单实例锁或维护状态不是 draining 时直接拒绝。
7. 单独确认配置切换；保持 scheduler 停止，在 draining 下用 `driver issues release-hold --input <manifest> --apply` 核验完整历史后释放。切到唯一 GitHub 后端，核对其他写者仍隔离，最后恢复 running 并启动唯一调度器。

### 切换证据清单

每项证据在切换窗口内重新读取，写入 #171 评论；未知或不一致即停在 held/draining，不绕过。运维命令须带服务 drop-in 中的 `HTTPS_PROXY`、`HTTP_PROXY`、`NO_PROXY`，凭据只列变量名。

| 项 | 证据与来源 | 写入 |
| --- | --- | --- |
| 程序与检出 | `orca-factory-driver version` 等于 main 部署提交；生产检出在 `main` 且 `git diff origin/main -- scripts/agent-gates core` 为空 | 否 |
| 配额 | `driver status --json` 中 `globalPause.active=false`、无 pauses；有配额暂停时推迟，不靠重启清除 | 否 |
| 旧 writer | `orca automations list` 中同仓库 `agent-linear-scan` 为 disabled；`systemctl --user list-timers`、`crontab -l` 无 coordinator；无 `driver exec`/`driver resolve` 进程；`maintenance status` 中 writers、sessions 为 false；其他机器与 Linear 集成人工确认 | 否 |
| 在途对象 | `status --json` 逐票列出 owner、phase、blockedReason；合并通道票核对 `merge-handoff-v1`/`merge-progress-v1` 与 PR 状态；resolver 票核对 resolution 与 `resolver.lock` 空闲；`executor.json` 的 Corrections、Completed 只读记录 | 否 |
| checkpoint 与预算 | 状态根下 `<repo>/<Identifier>/` 文件清单与 sha256，切换前后一致；导入与释放不读写这些文件 | 否 |
| 部署屏障 | 未完成部署验收的票保持屏障；ZII-899 等缺失交接单独结算，不靠导入解除 | 否 |
| drain / freeze | `maintenance drain` 后无新派工；`maintenance freeze` 成功，随后停止 `orca-factory-driver.service` 并确认 `driver-serve.lock` 可取得 | 本地状态 |
| 最终导出 | freeze 后 `issues export` 退出 0；`issues preview` 通过身份、依赖、编码与凭据形态检查；与预演清单比对票数、评论数与身份差异 | 本地清单 |
| held import | 回到 draining 后 `issues import --apply` 退出 0，失败按同一清单续传；随后不带 `--apply` 重跑，`created=0` 且无冲突 | GitHub |
| readback 与 release | `issues snapshot --json` 中导入票全部 held；`issues release-hold --apply` 完整回读通过后释放 | GitHub |
| 唯一调度器 | 合并 `AGENT_ISSUE_BACKEND=github` 后生产检出 pull；`issues backend` 输出 github；`serve --dry-run` 无异常候选；只启动一个 serve | 配置、进程 |
| 切换后验收 | 两个完整周期：status 票行 `backend=github`；Linear 请求计数增量为 0，journal 无 `"Linear request"`；票数、canonical ID、owner 与切换前一致；无重复派工 | 否 |

回滚边界：held import 之前可直接恢复旧程序与配置。import 开始后是单向门：GitHub 上的 held 票不删除，Linear 端保持 draining，由用户决定重来或放弃；GitHub 已产生新事实后不直接回到 Linear。

导出校验完整源 schema、分页和依赖，不因目标评论编码限制丢弃源成果。preview 另校验目标编码并报告 `fragmentedComments`、`destinationComments`；单条大评论使用带来源身份、顺序和完整摘要的 v2 分片，GitHub 每条正文不超过 50,000 字节。普通读取缺片、重排或冲突均失败，不将半条历史暴露给归属解析；仅相同 manifest 摘要的 held 导入可按已确认目标前缀续传。既有 v1 批评论保持可读。

manifest 摘要绑定整个来源集合。不要先导入任意子集，再用不同全量清单续传；已有目标的来源摘要冲突会阻止导入。导入预检保留已有原生 GitHub 票，但仍拒绝清单外的导入身份。

保留 checkpoint、receipts、correction/infra 预算、冻结 head、resolver 身份和 FIFO 屏障。关闭或 ImportHold 阻止新的执行/合并，不隐藏历史部署屏障。ZII-899 等历史归属须按新鲜事实单独结算，不能靠导入解除。GitHub 已产生新事实后，不直接回滚到陈旧 Linear。

历史协调恢复 Spec 位于 `docs/specs/driver-coordinator-recovery.md`，原父票 ZII-914、子票 ZII-915/ZII-916/ZII-917 保留身份。生产切换未验收前，不关闭 #171 或把整份迁移标为完成。
