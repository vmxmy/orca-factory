# 项目 worker 附加约束：orca-factory

## Owns（追加）

- 验证以票面 `CHECK` 为准。改 `core/` 或 `bin/` 时跑 `bin/orca-factory selftest`（含安装、gate 回归、ledger 与 CLI 检查）。依据：`bin/orca-factory` 的 `cmd_selftest`
- 改 `driver/` 时按 `driver/DEVELOPMENT.md` 选 Make 入口（`make test`、`make race` 等）。依据：`AGENTS.md` 的 Go 开发入口
- 改了 `core/` 下任何文件，必须再跑 `bin/orca-factory update .`，让 `scripts/agent-gates/` 副本跟上，否则 gate 的 `drift` 步判红。依据：`gate.project.sh`

## Does Not Own（追加）

- 不手改 `scripts/agent-gates/` 下由 core 分发的文件，只改 `core/` 再 `update`。依据：`orca-factory check` 的 `DIFFERS` / `OVERRIDDEN`
- 不改 `scripts/agent-gates/constitution/{engineering,workflow,handoffs}.md` 的副本；项目规则只写 `local-*.md`。依据：`ONBOARD.md` 第 4 步
- 不要求 shellcheck / shfmt，本机没有。依据：`command -v shellcheck shfmt` 无输出

## Verification（追加）

- shell 脚本必须兼容 macOS bash 3.2（`set -u` 下空数组展开会崩）。提交前至少 `bash -n <脚本>`。依据：commit 7ce0282
