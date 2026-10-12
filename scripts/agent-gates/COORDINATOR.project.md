# orca-factory 的项目规则

通用流程见 `COORDINATOR.md`；本文件只写本仓库特有的 lane 判据与合并上线规矩。参数在 `agent-graph.env`，重型检查在 `gate.project.sh`，工人约束在 `roles/local-worker.md`。

本仓库是流水线的分发源：`core/` 是源，`scripts/agent-gates/` 是它在本仓库自己身上的安装副本（自接入）。两者必须一致，`gate.project.sh` 的 `drift` 步用 `bin/orca-factory check .` 守这一点。

## Lane 判据

判定在 `agent-graph.env`：`AGENT_LANE2_GLOBS` 命中 → lane 2，`AGENT_LANE3_GLOBS` 命中 → lane 3（gate 直接判红），`scripts/agent-gates/*` 在 `gate.sh` 里硬编码为 lane 2；其余 lane 1。以 env 里的 glob 为准，gate 输出的 `LANE` 行就是结论。下表仅为展示用说明，改判据改 env，不改表。

| Lane | 本仓库的路径 / 操作 |
| --- | --- |
| 1 可逆·局部 | `examples/*`、`README.md`、`LICENSE` 等其余路径；几十行、`bin/orca-factory selftest` 能覆盖。依据：用户答复 |
| 2 可逆·宽（`AGENT_LANE2_GLOBS`） | 流水线本体 `core/`、`bin/`、`templates/`、`ONBOARD.md`（改坏会波及所有接入仓库）；`.github/`、`.claude/`、`.codex/`、`AGENTS.md`、`CLAUDE.md`、`scripts/agent-gates/`。依据：用户答复、`bin/orca-factory` 的分发逻辑 |
| 3 不可逆（`AGENT_LANE3_GLOBS` + 操作） | 文件：`.env*`、`*secrets*`、`.github/workflows/deploy*`（当前都不存在，预留）。操作（不经文件，靠流程禁止）：push main / force push、改写已发布的 marketplace 版本、删除 Linear project / 标签、启用或删除 Orca 定时任务 |

## 合并与上线

- lane 1 合并策略：coordinator 自合（squash）。依据：用户答复。
- lane 2 合并策略：按通用流程第 6 步及当前 `agent-graph.env` 推进；审批等待、路径复核及其他未满足的前提交给第 7 步 resolver 处置。resolver 提案仍须绑定当前 head，并通过 gate、独立审查和 CI；lane 3 保持授权边界。
- 无自动部署：`AGENT_DEPLOY_WORKFLOW`/`AGENT_SMOKE_CMD` 留空，跳过上线步。合并前同步最新 main 并重跑相关检查；必须等待远端五项 Driver CI 检查通过，不能用本地 gate 替代。
- 本仓库的验证命令是 `bin/orca-factory selftest`（约 7 秒）。写 spec 时把它放进 `CHECK`，不要放进 `gate.project.sh`（嵌套抢重型锁，见该文件头注释）。
