# Worktree 清理契约

基线为 `7b4df2c566a6f4977bb6cbf4609fed6a57ea10ba`，旧 `core/prune-run-worktrees.sh` blob 为 `9838bcaf96c526f7f0c9f1c08e1a16beb8d72825`，SHA-256 为 `fad5d41c10182d251b390f13f07df91fa16a0ab3fb84981047aa19e9ebb95bfa`。历史夹具保存完整字节，测试不依赖旧 Git 对象或 CI 深 clone。

## 公开边界与调用链

- `core/agent-graph.sh` 加载项目 env，保留调用方 AGENT 覆盖值，推导主检出，然后 `exec bash <目录>/prune-run-worktrees.sh`。coordinator 与 `core/constitution/workflow.md` 使用此命令。安装副本同样调用。
- 旧 `core/install.sh` 的定时 precheck 曾把 `git show origin/main:scripts/agent-gates/prune-run-worktrees.sh` 管道交给 `bash` 执行。定时 coordinator 已随 Linear 退役删除（OF-28），该入口不再存在；薄入口仍不依赖 BASH_SOURCE、相邻文件或本地 checkout 的源码。
- prune 行为由 driver 测试覆盖（`driver/cmd/driver/prune_worktrees_test.go`、`prune_install_test.go`）。`bin/orca-factory selftest` 中依赖 Linear 替身的 shell prune 用例已删除（OF-28）。
- README 描述分发内容；模板及项目 env 定义运行前缀。研究文档中的命令是历史观察，不是额外生产入口。
- 新公开命令为 `driver prune-worktrees`；唯一额外参数 `--contract-version` 输出 `prune-worktrees-v1\n`，退出 0，不读配置或访问服务。旧脚本忽略位置参数；其他参数继续忽略。薄入口探测同一 driver 后 exec，不调用旧业务，不提供 fallback。

## 输入、路径和状态

必需非空环境变量：`AGENT_REPO_PATH`、`AGENT_RUN_WORKTREE_PREFIX`、`AGENT_GH_REPO`；`AGENT_ISSUE_BACKEND` 必须为 `github` 或 `feishu`。默认 `AGENT_RUN_MAX_AGE_HOURS=24`、`AGENT_PRUNE_BUDGET_SECONDS=60`、`AGENT_PRUNE_MAX_PER_RUN=5`。数值边界接受十进制整数；错误拒绝执行。运行目录以物理路径排除自身，主检出同样解析物理路径。路径作为单个命令参数，不经 shell 拆词；Git 使用已有 NUL porcelain 格式，避免旧换行枚举的歧义。

票状态由所选账本一次完整列举读取。Linear 快照文件与调用计数文件已随 Linear 退役删除（OF-28）。没有新增账本、锁、数据库、checkpoint、预算重置或状态迁移。

## 原算法及保留规则

执行顺序固定为补偿、legacy run、legacy unit。补偿保留原 Git/Orca 清单用于票关联，PR 分页只读一次，remote heads 只读一次。Git 枚举或 Orca 清单失败使候选保留。票据主账本不可用停止补偿；legacy 中有关票状态也不能据失败删除。

1. Orca linked worker：排除自己和主检出；当前分支与路径中的第一个 `[A-Z][A-Z0-9]*-[0-9]+` 及清单关联票全部非 started；linked 票必须 completed；Git 全量未跟踪状态干净；HEAD 等于该分支 remote SHA，或匹配同仓库 merged PR 的 head SHA。非 force `orca worktree rm --worktree path:<路径> --json`，命令成功且 ok=true 才计数。
2. Git-only Claude worktree：路径仅限主检出 `.claude/worktrees/agent-*`，排除自己、Orca 管理路径与 locked；关联票非 started、全量状态干净、HEAD 匹配同仓库 merged PR。`git worktree remove` 成功计数。
3. 本地 branch：重新枚举占用，包含 locked 和主检出；仅不被工作树占用、关联票非 started、SHA 匹配 merged PR 的分支，执行 `git branch -D -- <分支>`。
4. 远端 branch：仅 `refs/heads/agent/<票号>/<单段单元>`，关联票非 started 且票 completed，merged PR 同时匹配 SHA 和 ref，执行 `git push origin --delete <分支>`。
5. legacy run：Orca 路径末段以前缀开始；排除 linked worker，避免绕过补偿失败；关联票非 started；有 `.omc/agent-run-finished` 或 createdAt 毫秒早于年龄阈值。原实现直接 force 删除，不检查 dirty 或主检出。
6. legacy unit：排除自己/主检出/原 AGENT_REPO_PATH、linked worker；关联票非 started；`.omc/agent-unit` 必须严格 `pr=<正整数>`，尾部换行被去除。缓存 CLOSED/MERGED 或 fallback `gh pr view <N> --repo ... --json state -q .state`；Git 状态干净；非 force Orca 删除命令成功后打印并计数。CLOSED 仍是既有有效条件，不擅自改为仅 MERGED。

## 此次明确修正的安全差异

S3 明确要求活跃、未知、脏或证据不足的工作树不能误删。旧 legacy run 的年龄/finished 并不足以跳过安全门。所有实际 worktree 清理路径共用主检出/自身物理路径排除、已登记 Git 归属、locked 保留和关联票非 started 判定；读取失败或不明路径保留。legacy 在每次重新枚举后使用该次 Orca 快照核对 linked 与票关联，不从旧清单授权。finished 标记保持 shell `-f` 的普通文件语义，Stat 跟随指向普通文件的符号链接，但同名目录不算完成。legacy 删除移除 `--force`，让删除端继续保护检查后变脏的目录，不增加强制 fallback。2026-10-08 只读 `orca worktree rm --help` 确认 `--force` 是可选强制 worktree 删除选项；本次不执行真实删除。不扩大候选集合。此差异是按安全验收修正，不宣称新旧全行为无差异。

## 输出、错误、预算和取消

- 普通补偿成功没有业务 stdout；Git branch/push/remove 输出继承。保留原因向 stderr 写 `skip <对象>: <单行原因>`。
- run 输出 `pruned <路径> ok=<ok>`；unit 输出 `pruned <路径> (PR <N> <STATE>)`。补偿失败保留并继续，整体通常退出 0；legacy 删除命令/JSON失败保留真实非零退出，unit 删除失败仅保留并继续。未知/活跃不计数，已失败删除不得计成成功。
- 数量上限覆盖全部阶段，包含分支删除。达到上限输出一次 `prune: 达到单次回收上限，剩余对象留到下次`。
- 时间预算只在补偿每个候选前检查，启动时以整数 Unix 秒记时；初始化/外部命令不受该软预算中断，legacy 阶段没有额外预算。超限输出一次 `prune: 超过时间预算，剩余对象留到下次`。SIGINT/SIGTERM/SIGALRM 取消 Go 命令及子进程组并等待退出，不继续删除。
- Git、Orca、gh 为既有外部依赖；JSON 验证、时间与文件读取进入 Go，prune 不再调用 jq、date、awk、grep、cat、mktemp。未新增依赖。已有 executor/merge/usage 的 Git 命令模式保持原样，复用 runx 子进程组取消能力；仓库原来没有 internal/worktree 包，不迁移其已交付调用者。

## 分发边界

`init/update` 在任何 core 写入前分别预检 ledger 和 prune driver 契约及入口归属。prune 仅接受缺失、上述原 blob、当前 core 薄入口；自定义、符号链接、不兼容程序拒绝切换。同目录暂存、0755、语法检查和 rename 只扩展到 prune，其他文件仍沿用原复制，不承诺整批回滚。开发以显式候选 `AGENT_DRIVER_BIN` 同步受版本管理副本；正式程序和客户发布只在合并后进行。所有删除验收仅在隔离临时 Git/local bare origin，加 fake Orca/GitHub/票据；不执行真实客户/生产 prune。
