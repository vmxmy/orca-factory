# orca-factory

把 Orca 自驱动开发流水线装进任意仓库：Linear 票自动被实现、检查、合并、上线，人只在 lane 2/3 和失败时介入。

## 怎么用

在 Orca 里开一个 agent（Claude 或 Codex），对它说一句话：

```
读 https://github.com/vmxmy/orca-factory 的 ONBOARD.md，把 ~/github/<repo> 接入流水线
```

agent 会调研目标仓库、向你确认几个无法推导的决定、生成三份项目配置装进 `<repo>/scripts/agent-gates/`、创建一个**禁用**的 Orca 定时任务，并跑一次验收后开 PR。合并后你用一张小票试跑，满意再启用。

建票推荐用 intake：在 Orca 里对该仓库运行 `claude --model claude-sonnet-5-5 --effort medium '按 scripts/agent-gates/roles/intake.md 工作'`。它是你与流水线之间唯一的对话面：只读仓库、只写 Linear，把需求变成带 SCOPE / CHECK / lane 预判 / estimate 的合格票，处理 `auto-failed` 票，给你看 `ledger stats`。

手动安装也可以：`bin/orca-factory init <repo-path>`，然后按 [ONBOARD.md](ONBOARD.md) 填三份项目文件。

看板：接入后在目标仓库里启动 Claude Code，首次会按 `.claude/settings.json` 自动安装 `swarm-board` 插件（私有 marketplace 靠本机 `gh` 登录），输入 `/swarm` 打开；`t` 泳道 / `s` agent / `d` 明细 / `q` 队列。Orca 自带的 Workspace Board（Settings → Shortcuts 里给「Toggle Workspace Board」绑快捷键后打开）会显示 coordinator 写回的进度，状态列为 `in-progress` / `in-review` / `completed`。Workspace Board 只显示现存 worktree（合并后 worktree 会被删除，所以 Done 列通常是 0）；完成历史看 `/swarm` 的「今日完成」和 Linear。

本仓库自己也接入了流水线（自接入），`core/` 是源、`scripts/agent-gates/` 是安装副本，改 `core/` 后要跑 `bin/orca-factory update .`，详见 `ONBOARD.md` 的「附：自接入」。

## 目录

| 路径 | 内容 |
| --- | --- |
| `ONBOARD.md` | 给 agent 读的接入手册（核心交付） |
| `core/` | 通用脚本与手册，原样复制到目标仓库：`gate.sh`（确定性检查门）、`gate.test.sh`、`linear-precheck.sh`（定时任务前置扫描）、`prune-run-worktrees.sh`、`ledger.sh`、`agent-graph.sh`（入口）、`install.sh`（创建定时任务）、`COORDINATOR.md`（coordinator 流程，只引用条款） |
| `core/constitution/` | 宪法：`constitution.md`（入口与优先级）+ 共享条款 `engineering.md` / `workflow.md` / `handoffs.md`（项目不得覆盖，只能加 `local-*.md`） |
| `core/roles/` | 角色 prompt，固定 Owns / Does Not Own / Verification / Handoff 四段：`intake.md`（用户手动开的建票会话）、`coordinator.md`、`worker.md`、`reviewer.md`；`README.md` 写 task spec 拼装与简报模板 |
| `templates/` | 三份项目文件的模板（`agent-graph.env`、`gate.project.sh`、`COORDINATOR.project.md`）和两份可选覆盖层（`local-worker.md`、`local-engineering.md`），带 `{{占位}}` |
| `examples/zhiyun-ai-dashboard/` | 一个真实项目的配置（三份项目文件 + `roles/local-worker.md` + `constitution/local-engineering.md`），照着填 |
| `bin/orca-factory` | `init`（装 core + 项目模板，并在 `<repo>/.claude/settings.json` 登记 marketplace 与 swarm-board）/ `update`（只刷新 core，含 constitution/ roles/）/ `check`（core 是否漂移、共享条款是否被覆盖、mod 是否登记）/ `selftest` |
| `mods/swarm-board/` | Claude Code mod：`/swarm` 看板。泳道视角（待领 / 实现中 / gate·评审 / CI·合并 / 需要人 / 今日完成）与 agent 视角（时长、token、估算成本、最后活动），数据来自 Linear、Orca、gh、ledger |
| `.claude-plugin/marketplace.json` | 本仓库即插件 marketplace；目标仓库 `.claude/settings.json` 的 `extraKnownMarketplaces` + `enabledPlugins` 指向它，Claude Code 启动时自动安装 |

## 前置条件

- 一台常开的 macOS，Orca 在运行（`orca status`），目标仓库已在 Orca 注册（`orca repo list`），Linear 已连接（`orca linear team list`）。
- `gh`、`jq`、`codex`、`claude` 已安装并登录。
- Orca Settings → Agents 里 claude 的默认参数带模型（定时任务以 claude 做 coordinator，这是唯一在 Orca 里设的模型）；Orca 仓库设置里的 setup 脚本能在新 worktree 装依赖。
- 角色模型在 `agent-graph.env` 里管：worker 分三档 `AGENT_TIER_FAST/DEFAULT/STRONG`（`agent:model:effort`），由票标签 `tier:*` > estimate 映射 `AGENT_TIER_BY_ESTIMATE` > default 选档，`AGENT_TIER_ESCALATE=1` 时被打回后自动升一档；reviewer 在 `AGENT_ROLE_REVIEWER_CMD`。都随仓库进 git。只有 coordinator 例外：定时任务只能指定 agent id，模型取 Orca Settings → Agents 里 claude 的默认参数——**这是全局设置，对本机所有仓库、所有 claude 工人生效**，不要把某个角色的模型写在这里。
- Linear 里有与仓库同名的 project，以及 `ready-for-agent` / `auto-impl` / `auto-failed` 三个标签（接入时 agent 会带你建）。

## 安全须知

定时任务无人值守地运行 coordinator 和 Codex 工人，它们继承你本机的全部凭证（gh、wrangler、kubectl、云账号……）。lane 3（密钥文件、部署脚本、执行迁移等）靠 glob 和流程禁止，但没有系统级隔离：工人仍然**能**碰到生产凭证。接入后先只放小票（几十行、可写断言、lane 1），观察几轮再扩大范围；敏感仓库考虑用独立账号或只在隔离机器上跑。

## 核心机制

1. 定时任务每 5 分钟跑 `linear-precheck.sh`：project 内带 `ready-for-agent` 且未被阻塞的 Todo 票，且在途数 < 上限，才启动 coordinator。
2. coordinator（claude）领票、按 blast radius 拆分、写 gate spec 与 task spec，派 Codex 工人在独立 worktree 实现。
3. 工人改到绿后自跑 `gate.sh`；coordinator 不信自报，从 origin/main 取 `gate.project.sh` 独立复跑：范围、lane、抑制标记、新测试在 base 上必红、项目检查（类型/lint/守卫/定向测试）、CHECK。
4. 红了打回工人修，最多 3 次；3 次不过由 resolver 接管排障与修复。
5. lane 1 绿 → 开 PR、等 CI、squash 自合、等部署、冒烟、票置 Done；lane 2 → 检查通过后合并，需额外审批的交给 resolver 复核；lane 3 → gate 判红，由 resolver 查明范围与权限，仍不得绕过限制。
6. 每轮记账到 `.omc/agent-graph/runs.jsonl`（含 lane 与 tier），首跑通过率低就先改拆分与 spec，再谈加守卫；某档通过率低就调映射或升档。
