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
2. 使用 `driver issues preview --input <manifest>` 离线验证目标、身份、依赖闭包与摘要；历史快照不证明当前完整性。
3. 单独确认真实导入的数据范围、目标仓库可见性和敏感内容。
4. 使用 `driver maintenance drain` 停止新认领/派工，允许已有写者自然收敛；核对实际 worker、resolver 与旧 coordinator。
5. 无旧写者后 `driver maintenance freeze`，完成最终导出和差异核对。维护状态持久于本机；本机锁不证明跨机器无写者。
6. `driver issues import --input <manifest> --apply` 写入 held 票，按稳定标记完整回读；保留作者来源、历史时间、顺序与摘要，不伪造原作者。
7. 单独确认配置切换；在 draining 状态用 `driver issues release-hold --input <manifest> --apply` 完整核验后释放，确认唯一调度器再恢复 running。

保留 checkpoint、receipts、correction/infra 预算、冻结 head、resolver 身份和 FIFO 屏障。关闭或 ImportHold 阻止新的执行/合并，不隐藏历史部署屏障。ZII-899 等历史归属须按新鲜事实单独结算，不能靠导入解除。GitHub 已产生新事实后，不直接回滚到陈旧 Linear。

历史协调恢复 Spec 位于 `docs/specs/driver-coordinator-recovery.md`，原父票 ZII-914、子票 ZII-915/ZII-916/ZII-917 保留身份。生产切换未验收前，不关闭 #171 或把整份迁移标为完成。
