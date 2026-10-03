# orca-factory

把 Orca 自驱动开发流水线装进任意仓库：Linear 票自动被实现、检查、合并、上线，人只在 lane 2/3 和失败时介入。

## 怎么用

在 Orca 里开一个 agent（Claude 或 Codex），对它说一句话：

```
读 https://github.com/vmxmy/orca-factory 的 ONBOARD.md，把 ~/github/<repo> 接入流水线
```

agent 会调研目标仓库、向你确认几个无法推导的决定、生成三份项目配置装进 `<repo>/scripts/agent-gates/`、创建一个**禁用**的 Orca 定时任务，并跑一次验收后开 PR。合并后你用一张小票试跑，满意再启用。

手动安装也可以：`bin/orca-factory init <repo-path>`，然后按 [ONBOARD.md](ONBOARD.md) 填三份项目文件。

## 目录

| 路径 | 内容 |
| --- | --- |
| `ONBOARD.md` | 给 agent 读的接入手册（核心交付） |
| `core/` | 通用脚本与手册，原样复制到目标仓库：`gate.sh`（确定性检查门）、`gate.test.sh`、`linear-precheck.sh`（定时任务前置扫描）、`prune-run-worktrees.sh`、`ledger.sh`、`agent-graph.sh`（入口）、`install.sh`（创建定时任务）、`COORDINATOR.md`（coordinator 通用流程） |
| `templates/` | 三份项目文件的模板（`agent-graph.env`、`gate.project.sh`、`COORDINATOR.project.md`），带 `{{占位}}` |
| `examples/zhiyun-ai-dashboard/` | 一个真实项目的三份配置，照着填 |
| `bin/orca-factory` | `init` / `update`（只刷新 core）/ `check`（core 是否漂移）/ `selftest` |

## 前置条件

- 一台常开的 macOS，Orca 在运行（`orca status`），目标仓库已在 Orca 注册（`orca repo list`），Linear 已连接（`orca linear team list`）。
- `gh`、`jq`、`codex`、`claude` 已安装并登录。
- Orca Settings → Agents 里 claude 的默认参数带模型（定时任务以 claude 做 coordinator，不带模型会用默认模型）；Orca 仓库设置里的 setup 脚本能在新 worktree 里装好依赖。
- Linear 里有与仓库同名的 project，以及 `ready-for-agent` / `auto-impl` / `auto-failed` 三个标签（接入时 agent 会带你建）。

## 安全须知

定时任务无人值守地运行 coordinator 和 Codex 工人，它们继承你本机的全部凭证（gh、wrangler、kubectl、云账号……）。lane 3（密钥文件、部署脚本、执行迁移等）靠 glob 和流程禁止，但没有系统级隔离：工人仍然**能**碰到生产凭证。接入后先只放小票（几十行、可写断言、lane 1），观察几轮再扩大范围；敏感仓库考虑用独立账号或只在隔离机器上跑。

## 核心机制

1. 定时任务每 5 分钟跑 `linear-precheck.sh`：project 内带 `ready-for-agent` 且未被阻塞的 Todo 票，且在途数 < 上限，才启动 coordinator。
2. coordinator（claude）领票、按 blast radius 拆分、写 gate spec 与 task spec，派 Codex 工人在独立 worktree 实现。
3. 工人改到绿后自跑 `gate.sh`；coordinator 不信自报，从 origin/main 取 `gate.project.sh` 独立复跑：范围、lane、抑制标记、新测试在 base 上必红、项目检查（类型/lint/守卫/定向测试）、CHECK。
4. 红了打回工人修，最多 3 次；3 次不过视为 plan 错，转人。
5. lane 1 绿 → 开 PR、等 CI、squash 自合、等部署、冒烟、票置 Done；lane 2 → 开 PR 等人批；lane 3 → gate 直接判红，转人。
6. 每轮记账到 `.omc/agent-graph/runs.jsonl`，首跑通过率低就先改拆分与 spec，再谈加守卫。
