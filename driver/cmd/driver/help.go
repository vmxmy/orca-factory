package main

// topLevelHelp is written for agents as well as people: one command per entry,
// its side effect in brackets, then one line of purpose.
const topLevelHelp = `orca-factory-driver: orca-factory 流水线驱动器。常驻调度器从票据主账本领票，
派 worker 实现，经 gate 与审查后由合并通道 squash，部署与 smoke 通过后写回 Done。

Usage:
  orca-factory-driver <command> [args]
  orca-factory-driver --help | -h | help

Effects:
  [只读]  不写票据、GitHub、Orca 或本地状态
  [写入]  可能写票据评论、GitHub、Orca 或本地状态
  [常驻]  长期运行，直到被停止

Commands:
  serve [--dry-run]
      [常驻][写入] 唯一调度器：发现、认领、恢复执行器、推进合并与发布；持有单实例锁。
      生产由 systemd 服务 orca-factory-driver.service 运行，不要再启动第二个。
      --dry-run 只读推演一次：不合并、不写回、不 fetch。
  status [--json]
      [只读] 输出调度器观察快照：在途票、候选票、阻塞原因、配额。快照可能过期，不授权任何动作。
  doctor [--json]
      [只读] 运行态健康自检：服务 PATH、运行版本、维护状态、项目检出、core 同步 PR、
      在途票账本、近 30 分钟重复启动、凭证变量名。任一 fail 时退出 1。不输出凭证值。
  watch [--dry-run]
      [写入] 跑一次 doctor，同一检查项连续 2 次 fail 时向飞书告警一次，恢复 ok 后可再告警。
      只写本机 watch.json 与发告警，不修复、不建票。由 orca-factory-driver-watch.timer 每 30 分钟运行。
      --dry-run 只读：不发送、不写状态。
  logs <票号> [--follow] [--since 时长] [--json]
      [只读] 查看单票的执行器与处置日志。
  usage <票号> --json [--reviewer-worktree <path>] [--run-worktree <path> ...]
      [只读] 汇总单票的模型用量。
  maintenance status|freeze|drain [--wait 2m]|resume [--wait 2m]
      [写入] 查看或切换本机维护状态。drain 停止新派工；freeze 停止全部外部写入；
      resume 恢复 running。drain/resume 等待已准入的认领与派工结束（默认 2 分钟），
      等待期间拒绝新认领与派工，超时不切换状态。freeze 仍需先 drain 且无活动会话。
      status 只读。
  issues <subcommand> [--id <ID>] [--input <file|->] [--body-file <file|->] [--state <状态>]
         [--add-label <名>] [--remove-label <名>] [--apply] [--json]
      [写入] 票据主账本 CLI，需要先加载 scripts/agent-gates/agent-graph.env。
      只读：snapshot、read、validate、backend、preview。
      写入：create、adopt、update、comment；迁移专用：import、release-hold。
  exec [--merging] <票号>
      [写入] 单票执行器。由 serve 派发，通常不手动运行。
  resolve <票号>
      [写入] 处置 Agent 入口。由 serve 派发，通常不手动运行。
  resume-deployment --issue <票号> --queue <ID> --sha <SHA> --action <动作>
                    --operator <名字> --reason <依据> [--dry-run]
      [写入] 部署屏障的人工恢复，动作为 confirm-passed、retry-workflow 或 retry-smoke。
      需先停止 serve；先带 --dry-run 核对。
  ledger append '<json>' | ledger stats [--all]
      [写入] 用量账本。stats 只读。
  ledger-shell append '<json>' | ledger-shell stats
      [写入] core/ledger.sh 的兼容入口。stats 只读。
  prune-worktrees [--contract-version]
      [写入] 清理已结束的定时任务 worktree，需要 AGENT_REPO_PATH 与 AGENT_RUN_WORKTREE_PREFIX。
  prune-workspaces [--dry-run]
      [写入] 按 serve 的回收规则一次性删除可接管项目中已结束票的 Orca 工作区：
      Done/Canceled 票的全部 executor、resolver 工作区，及其他票被新一轮取代的旧 resolver。
      有未提交改动、未推送提交、运行中终端或 Orca 置顶时保留。--dry-run 只列出不删除。
  feishu-gateway
      [写入] 飞书长连接需求入口，按群与白名单过滤并回复话题。
  projects --json
      [只读] 发现本机可接管项目：联立 Orca 仓库清单、飞书项目表与流水线 check，
      输出可接管与不接管（附原因）两个列表，不读 driver.toml 的 repositories。
  onboard --repo <path> [--dry-run]
      [写入] 接入一个「可接入」仓库：在其独立 worktree 运行接入 Agent（init、填项目文件、
      ONBOARD 5a～5d、开接入 PR，不合并），再由 driver 在飞书项目表加「启用」=false 的行。
      同一仓库已有进行中的接入时拒绝。--dry-run 只打印步骤与默认值。
  install [--dry-run]
      [写入] 把当前二进制装到 ~/.local/bin，写 systemd user unit 并 daemon-reload。
      --dry-run 只打印步骤。
  selftest --live
      [只读] 对真实系统复核 driver/CONTRACTS.md 中的外部契约，含一次审查员调用。
  version
      [只读] 输出构建版本。

Environment:
  AGENT_ISSUE_BACKEND       票据后端；本仓库为 github
  AGENT_GH_REPO             GitHub 仓库，形如 owner/repo
  AGENT_DRIVER_STATE_ROOT   状态根目录，默认 ~/.local/state/orca-factory
  配置文件                  ~/.config/orca-factory/driver.toml

Exit codes:
  0  成功
  2  用法错误或未知命令
  其他非零值  命令执行失败，原因见 stderr

Examples:
  orca-factory-driver status --json | jq '.tickets'
  orca-factory-driver serve --dry-run
  (set -a; . scripts/agent-gates/agent-graph.env; set +a; orca-factory-driver issues read --id <票号> --json)

Docs:
  driver/README.md            服务排查、部署屏障、driver.toml 参数
  driver/CONTRACTS.md         交接、合并进度等持久格式
  docs/agents/issue-tracker.md  票据入口、迁移与切换
`
