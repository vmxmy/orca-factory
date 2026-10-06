# 执行账本选择与切换

适用于安装、票据命令、看板和一次性迁移。运行中的仓库以 `scripts/agent-gates/agent-graph.env` 为准；新模板用 `AGENT_ISSUE_BACKEND=github`，已发布配置缺省或显式 `linear` 保持原契约。未知值直接失败。`bin/orca-factory update` 刷新公共脚本，不替用户切换项目配置。

## GitHub 模式

GitHub Issues 是唯一执行账本，统一 driver CLI 负责票据身份、状态、评论与 ledger 的编码。shell 和看板调用该接口，不自行解析或生成元数据。缺失、截断、权限错误和超时均是未知，保留旧展示并提示错误，调度与清理不把未知当作空队列或完成。缓存失败不访问 Linear，没有后台同步或双写。

历史 ZII 标识仍作为执行身份；GitHub issue URL/number 是存储地址。保留原 UUID、Run/Task/Dispatch、checkpoint 路径、worktree、分支、PR、owner、correction/infra/catchup 计数。PR 与 worktree 的旧关联保持可读，新票按 driver 返回的 canonical identifier 关联。`auto-failed` 即使同时带 ready 标签，也不允许自动领取；人工处理保留原预算与证据。

本仓库 feature 分支的已提交 profile 是发布目标（GitHub + 标签名称），原 team/project IDs 保留为迁移来源。它不代表生产已经切换：只有完整导入、身份/预算回读核对通过后，部署步骤才把二进制、安装副本和该 profile 一起同步到生产主检出。仅升级二进制不完成迁移，也不得先部署目标 profile 后补数据。

## 一次性切换

1. 在隔离工作树验证 driver、shell、看板和安装副本，所有 GitHub 路径零 Linear 调用，旧配置回归保持通过。新 driver 未可用前不得发布依赖它的脚本。
2. 制作带来源时间、哈希、原身份、逐字段完整性和目标映射的导入清单。盘点配置、checkpoint（含 released 历史）、brief/gate/correction 证据、PR/worktree 关联和暂停状态。离线快照只证明已含数据；缺票、缺评论、缺依赖单独标记待人工补齐。
3. 切换时阻止新领取，核对活跃执行器并完成或明确移交；状态投影过期不是退出证据。保留程序和状态备份，保留既有 quota/部署暂停保护。
4. 幂等导入先建票及稳定 source marker，再按映射补依赖、评论和 PR 关联；逐事件防重复。保留原状态、标签、原作者/时间的来源说明，未完整导入的票保持 hold；不得把导入更新时间当成新执行心跳。
5. 回读核对 ID 映射、完整性、预算与归属，所有 checkpoint 均可找到唯一票。一次性切换 driver、shell、看板、配置与原 automation ID；旧写入方停止后才启用 GitHub 写入方，不并行双写。
6. 只恢复已验证且可领取的工作。既有 auto-failed 不重排。回滚先停新领取并核对 GitHub 新增事实，不能直接重启旧 Linear 写入方丢失新进度。

## SQLite 本地快照（GH-178）

GitHub 模式的普通发现与展示采用有界缓存；`Read` / `ListComments` 及领取、恢复、合并前的远端复核保持强制读取。SQLite 只保存可重建投影，不替代 GitHub 账本或 executor checkpoint，也不触发 Linear 回退。

缓存按 API 地址、仓库和实际凭据范围隔离，凭据不以明文写入磁盘。数据库使用 WAL，数据与刷新完成信息在同一事务提交；评论独立过期。权限错误、损坏、分页失败或取消保留为未知，不能转换为空列表或完整快照。写操作前后失效缓存，旧刷新不得越过 generation 屏障发布。

缓存默认位于用户缓存目录内，支持测试与运行环境覆盖；具体路径、TTL 和恢复步骤见 [SQLite 设计与运行说明](../research/sqlite-cache-design.md)。保持原有 JSON 输出及快照文件配置契约；看板无需依赖 SQL 表结构。数据库删除或数据重置仍须显式要求。

## 验证

`bin/orca-factory selftest`、`bin/orca-factory check .`、`claude plugin validate mods/swarm-board`、`claude plugin test mods/swarm-board` 检查脚本和看板；driver 入口见 [开发检查](../../driver/DEVELOPMENT.md)。单元还须运行原有 project gate 的 scope、固定基线与契约检查。源码搜索无 Linear 调用不足以验收：用拒绝 Linear 调用的夹具执行新安装、读写、缓存失败和失败票路径。
