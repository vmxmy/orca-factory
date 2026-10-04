# 流水线 worktree 与分支清理调研（ZII-815）

## 1. 清单

结论：需要分别管理七类对象；删除 worktree 不等于删除分支，更不等于注销插件记录。当前数量是“存在量”，不是可删除量。本报告只读调研，没有执行清理或修改配置；报告存于仓库 `docs/research/worktree-cleanup.md`。

证据基线：2026-10-05（Asia/Shanghai）现场快照，当前报告 worktree 基线为 `2eaaf9f7f7c7fcbef72e560ab1ee5e85f2da4dee`；以下仓库文件行号均相对此版本。GitHub 数据来自 `vmxmy/orca-factory`，命令在报告 worktree 执行，未检查其他 worktree 的文件内容。并发运行会改变数量。

| 类别 | 谁创建、证据 | 何时应删（建议生命周期） |
| --- | --- | --- |
| 运行 worktree | Orca 自动运行创建；`scripts/agent-gates/constitution/workflow.md:8`，Orca 输出 `automationProvenance.kind=created-by-automation` | 运行已结算、资源释放、内容已保存后由下一次运行回收，不能只凭年龄 |
| 单元工人 worktree | coordinator 的 `worker-start`；`scripts/agent-gates/COORDINATOR.md:52` | PR 确认合并且工作已保存后；转人保留，人工合并后补收尾 |
| 实施代理 worktree | `.claude/worktrees/agent-*` 路径及 `locked claude agent ...` 表明 Claude 管理；具体创建会话未核实 | 实施代理结算、对应 PR 合并、无独有工作后，由其管理方回收 |
| 本地分支 | worktree 创建或实施过程中建分支；Git 输出包含 `worktree-agent-*`、`feat/*`、运行同名分支 | 不再被任何 worktree 使用且提交已保存、合并确认后 |
| 远端 `agent/*` 备份分支 | worker 显式 push；`scripts/agent-gates/roles/worker.md:14` | 本单元及该票相关 PR 合并后逐一核对；仍在途、转人或备份唯一时保留 |
| 远端 `feat/*`、`fix/*` | 实施者推送；GitHub PR 的 `headRefName` 与远端列表对应 | 对应 PR 合并、分支头没有合并后新提交时 |
| Claude Code 插件的 project 作用域记录 | Claude project 安装登记；`claude plugin list --json` 有 `projectPath`、`installedAt`；`ONBOARD.md:213` 提醒安装插件 | 对应项目 worktree 已确认退役后注销该路径记录；保留主检出与仍使用中的作用域 |

## 2. 现状

结论：已有运行清理、成功合并收尾和带标记单元补清理；并没有覆盖所有创建入口的统一回收。实测 Git 有 34 个 worktree 登记，而 Orca 本仓库只列 21 个，不能只扫 Orca 就声称清完。

以下是已执行的只读命令及结果摘要，均成功退出；未执行 `prune-run-worktrees`，因为它会删除目录。

```sh
git worktree list --porcelain
git branch --format='%(refname:short)'
git ls-remote --heads origin
orca worktree list --repo path:/home/laoxu/github/orca-factory --json
claude plugin list
claude plugin list --json
gh pr list --repo vmxmy/orca-factory --state all --limit 100 --json number,state,headRefName,mergedAt
gh api repos/vmxmy/orca-factory --jq '{delete_branch_on_merge}'
```

| 类别 | 实测数量、口径与输出证据 | 当前清理机制 |
| --- | --- | --- |
| 运行 worktree | 11 个，按路径末段 `auto-agent-linear-scan-` 前缀；run 编号 10、24、26、28、35、36、37、55、77、78、79；Git 与 Orca 一致 | `prune-run-worktrees.sh:14-19` 扫 Orca、排除自己，结束标记存在或超过默认 24 小时即 `rm --force`；`COORDINATOR.md:7` 每次领票前触发 |
| 单元工人 worktree | Orca 9 个：`board-3`、`board-4`、`impl`、`lane2`、`lane2-reviewer`、`main-2`、`report`、`skill`、`zii-808-takeover`；Git 另有 run-36 下 `.omc/wt-807`，按历史单元候选共 10 个；`report` 是本次在途任务 | `COORDINATOR.md:92` 合并后 release、rm；`:98` 转人且已有 PR 才写 `.omc/agent-unit`；`prune-run-worktrees.sh:24-67` 对带标记、PR 为 MERGED 或 CLOSED、状态干净者删除 |
| 实施代理 worktree | Git 中 `.claude/worktrees/agent-*` 11 个；其中 3 个带 locked，Orca 21 条本仓库记录中无这些路径 | 本次读取的流水线规则没有 Claude 实施目录专项回收；前述脚本只枚举 Orca |
| 本地分支 | 49 个（含 main）；其中 `worktree-agent-*` 10 个；不能用 49 减 worktree 数直接推算孤儿，因为存在 detached 登记 | `COORDINATOR.md:92` 没有显式本地分支清理命令；Orca rm 是否连带删分支本次未通过破坏性实验验证 |
| 远端 `agent/*` | 8 个：ZII-803/skill、804/board、805/lane2-reviewer、806/fmt、807/board、808/lane2、810/impl、813/main | `COORDINATOR.md:92` 成功自合后逐一删除本票备份；未自合 lane 2 留给人工或后续清理 |
| 远端 `feat/*`、`fix/*` | 分别 10、2 个；远端所有 heads 共 43 个；`delete_branch_on_merge=false` | GitHub 自动删头分支关闭；第 6 步只明确要求删除 `agent/<票号>/*` |
| 插件 project 记录 | `swarm-board@orca-factory` 共 26 条：project 25、user 1；project 包括主检出 1、运行路径 17、单元路径 7；同一插件不是 26 个独立插件 | `ONBOARD.md:213` 描述安装，`prune-run-worktrees.sh` 与 `COORDINATOR.md` 无 project 安装记录注销动作 |

Git 的 34 个登记可复算为：主检出 1 + 运行 11 + 单元候选 10 + Claude 实施 11 + `/Users/xumingyang/orca/workspaces/.orca-preparing/...` 的 locked detached 登记 1。Orca 返回 `totalCount=21, truncated=false`，为主检出 1 + 运行 11 + 单元 9。旧 macOS 路径只是登记，是否仍存在、是否属于活跃远程环境未核实，不能直接删除。

PR 证据：远端 `agent/ZII-803/skill`、`804/board`、`805/lane2-reviewer`、`806/fmt` 分别对应已 MERGED 的 #28、#26、#27、#25；12 个 `feat/*`、`fix/*` 都有同名已 MERGED PR（#1、#2、#29–#38）。例如 `feat/zii-814-scope-avoid` 为 #38，`mergedAt=2026-10-04T21:02:10Z`；`fix/swarm-lane-stage` 为 #29。这里只证明有合并记录，尚未逐一比对现在的分支头与合并时头 SHA，不能直接认定 12 个都能删。

插件证据：project 记录中 run-2、4、12、13、14、21 六条路径不在 Git 与 Orca 的当前列表中；如 run-2 的 `installedAt=2026-10-04T17:31:01.192Z`、版本 `0.2.4`。这是“未登记路径”候选，不等于已证明磁盘路径不存在；其余运行记录与现有 11 个运行路径对应。所有记录 `enabled=true` 也不能证明目录仍在使用。

## 3. 缺口

结论：残留来自入口覆盖不足、成功路径之外缺少重试、生命周期状态没有联动；同时现有运行强制清理与 CLOSED 单元清理存在保护缺口。以下区分可证实机制与尚未验证的现场原因。

| 类别 | 缺口与原因 | 证据 |
| --- | --- | --- |
| 运行 worktree | 只有下一次运行才触发；缺标记且未满 24 小时会保留。不能据 11 个存在量断言清理失败；本次未读标记。超时也不能证明代理已退出，且删除前未检查脏内容、子 worktree 或未推送提交 | `prune-run-worktrees.sh:10-19`；`workflow.md:38`；Git 输出 run-36 内还有 `.omc/wt-807` |
| 单元工人 worktree | 成功收尾中断后无可靠补偿；转人无 PR 不写标记，历史无标记目录不被扫描。脚本接受 CLOSED，而共享规则要求 merged，且 clean 不代表已推送 | `COORDINATOR.md:92,98`；`prune-run-worktrees.sh:29,57-63`；`workflow.md:10`。Orca 输出所有 9 个单元的 `linkedPR=null`，`board-3` 仍写“Codex 未登录” |
| 实施代理 worktree | Claude 与 Orca 的管理清单不同；流水线脚本不会枚举 Git 独有的 11 条。locked 三条的生命周期不明，不能推断崩溃 | `prune-run-worktrees.sh:14,24`；Git/Orca 输出差集；同名分支的 PR 已合并也不证明工作区干净 |
| 本地分支 | 缺少分支与 worktree 解绑后单独对账的步骤；Claude 默认分支改名/另建业务分支后，默认命名分支仍存在是候选现象，创建历史未核实 | `COORDINATOR.md:92`；`git branch` 中 10 个 `worktree-agent-*`，但实施 worktree 大多使用 `feat/*` 或 `fix/*` |
| 远端 `agent/*` | 本票成功路径有删除要求，但没有全仓周期补偿；人工合并、旧流程或中断可能绕过。剩余 4 条不能仅凭票号与另一个 feat PR 合并就删除 | `COORDINATOR.md:92`；#25–#28 已合并但备份仍在，另 4 条没有在本次结果中找到同名 PR |
| 远端 `feat/*`、`fix/*` | GitHub 自动删分支关闭，而手册收尾仅涵盖 agent 命名空间 | `gh api` 返回 false；`COORDINATOR.md:92`；12 个同名已合并 PR |
| 插件 project 记录 | 安装身份按 projectPath 登记，目录生命周期与插件登记没有清理联动；共享缓存和 project 记录不是同一对象 | `claude plugin list --json` 的 25 个 projectPath 多对一指向版本缓存；六个路径未在两份 worktree 清单中出现；`ONBOARD.md:213` 仅安装指引 |

## 4. 方案

结论：推荐“合并后立即收尾 + 每次运行开头补偿 + 定时只读对账”，以明确归属、已合并、无独有工作为共同门槛。以下是待实施方案，本次未运行其中任何删除操作，也未修改 GitHub 设置。

共同判据应由命令输出确定，并在删除前重新检查：

- PR：`gh api repos/vmxmy/orca-factory/pulls/<N>` 必须返回 `merged=true`、预期 `base.ref` 和 `head.repo.full_name`；记录 `head.sha`，用 `git ls-remote --heads origin refs/heads/<branch>` 比较当前远端 SHA。CLOSED 未合并不通过；开放 PR、复用分支或 SHA 变化均保留。
- 工作区：`git -C <wt> status --porcelain=v1 --untracked-files=all` 成功且输出为空；另用 `git -C <wt> ls-files --others --ignored --exclude-standard` 列出被忽略文件，检查 `.omc/`、环境文件、会话产物是否已保存。Git 干净不覆盖这些数据。
- 提交：先确定可信备份 ref；在缓存 ref 已与 `git ls-remote` SHA 对齐的前提下，`git rev-list --count <backup-ref>..<local-branch>` 必须为 0，且无分叉遗漏。缓存不新或对象不可用时暂停，由后续实现显式刷新再核验。没有备份则要求提交可达已合并 main（`git merge-base --is-ancestor <branch> <verified-main>` 退出 0）或先保留。
- squash 合并：`workflow.md:20` 使用 squash，不能只用 `git branch --merged` 判断；改以已合并 PR 的原始 `head.sha` 对照当前头，额外提交必须先独立保存和评审。agent 备份不是 PR 头时须建立精确 SHA/产物对应关系，不能只匹配票号。
- 归属：用 `git worktree list --porcelain` 与 `orca worktree list --repo ... --json` 双向对账，排除主检出、当前目录、锁定路径、其他主机、仍在途任务及包含子 worktree 的父目录；运行超时只能触发调查。Orca 的结算与 release 回执作为资源释放证据，unknown 保留。

| 类别 | 推荐做法与触发时机 | 命令判据及失败处理 |
| --- | --- | --- |
| 运行 worktree | 每次运行开头补偿；定时巡检陈旧项；按子目录先于父目录回收 | 结束标记 + 运行已结算 + 共同工作区/提交/归属判据；有 PR 的产物须合并，无 PR 的无票运行须证明无独有内容。超过 24 小时但无结算证据则保留并告警，去掉仅凭年龄强删的策略 |
| 单元工人 worktree | 合并后立即；每次开头重试上次未完成的收尾 | 从 PR 与任务映射确定路径，满足所有共同条件后经 Orca 管理入口回收；标记缺失、PR 查询失败、CLOSED 未合并或仍有工作则保留，登记明确阻塞原因 |
| 实施代理 worktree | 实施 PR 合并后立即；定时对 Git 独有路径补偿 | Git 登记关联 Claude 管理者、合并 PR 和头 SHA；共同条件全满足且锁持有者确认退出后交原管理方清理。无法证明进程结束、归属或内容安全则不解锁、不强删 |
| 本地分支 | worktree 回收后立即；定时扫未被占用分支 | `git worktree list --porcelain` 无任何占用，提交已备份且 PR 已合并；运行无产物分支须可达已核验 main。先用保守删除语义，squash 导致拒绝时保留并记录证据，不自动升级强删 |
| 远端 `agent/*` | 该单元 PR 合并后立即；每次开头扫描遗漏 | 严格解析票号/unit，检查对应 PR 和备份头 SHA、在途任务、未推送本地提交；缺一项保留。删除失败仅重试该精确 ref，不扩大到整票通配删除 |
| 远端 `feat/*`、`fix/*` | 合并后立即，定时补偿；评估开启 GitHub 自动删头分支 | 同名同仓 PR 已合并，当前远端 SHA 等于合并头且无新开放 PR/依赖；检查关联本地工作已保存。自动设置仅在确认共享分支策略后由维护者变更；API 失败或头变化时保留 |
| 插件 project 记录 | worktree 退役后立即；定时对账历史记录 | `claude plugin list --json` 精确匹配 scope、id、projectPath；两份 worktree 清单均不再登记，且 `test ! -e <path>` 并核对非离线挂载/远程路径，方可列为注销候选。先核实当前 Claude CLI 对指定 project 的注销能力；不支持已消失路径则交人工，不手改全局登记库、不删共享缓存；失败保留记录并单独重试 |

建议把重试状态放到既有 Orca 任务/元数据或票据收尾记录，保存路径、分支、PR、检查时头 SHA 和失败原因。一次对象删除失败不应被包装成整次清理成功；网络失败、分页不全、身份不明均默认保留。实施时单独验证退出码和 `.ok`，不能只打印 `ok=false` 后继续当成功（现脚本 `prune-run-worktrees.sh:19`）。

## 5. 风险

结论：优先保护未提交内容和没有独立副本的提交；“已合并”“工作区干净”“已推送”分别证明不同事实，必须组合检查。现存量不能变成无条件删除清单。

| 风险对象 | 恢复边界 | 推荐保护条件 |
| --- | --- | --- |
| 未提交改动、未跟踪文件及被忽略文件 | Git 没有提交副本时，目录强删可能不可恢复；不能依赖 reflog 恢复未记录内容 | 完整 status + ignored 清单；有价值内容先由所有者保存；不对脏目录使用 force |
| 未推送提交、未合并分支 | 删 worktree 不必然立刻删 Git 对象，但同时删唯一分支后只剩临时 reflog/悬空对象，GC 后可能永久丢失 | 可信远端备份与 SHA 可达性确认；未合并/CLOSED 默认保留；不能把本地缓存 remote ref 当实时备份 |
| squash 后的原始提交历史 | 合并后的代码不等于原分支全部历史；若需要保留逐次提交，删最后 ref 后不可保证恢复 | PR 合并证据 + 原头 SHA；按保留策略保存独立归档后再清理，额外提交不随已合并 PR 一起删 |
| 活跃或锁定 worktree、父目录下子 worktree | 强制移除可能破坏运行中的工作和子任务；24 小时只是时间阈值 | 明确任务结算与管理者退出证据；子目录先处理；锁、远程状态不明均保留 |
| 插件 project 记录与共享缓存 | 误删 user/其他 project 作用域会影响仍活跃项目；共享缓存不是孤儿 project 记录 | 只注销已退役精确路径与 scope，保留主检出及 user 记录；不据版本旧就删缓存 |
| 合并与删除之间的并发写入 | 检查后分支可能新增提交；旧 PR 合并信息不能授权删除新头 | 删除前重新读取 PR、远端头、工作区与任务状态；检测变化即取消；不能保证原子校验时先停止相关写入并交维护者处理 |

本次没有删除任何路径，没有运行清理脚本。尚待后续实施核验：历史目录真实内容与标记、Claude 锁持有者状态、全部候选当前头与 PR 原头的匹配、已退役路径的插件注销方式，以及 Orca rm 的本地分支连带行为。
