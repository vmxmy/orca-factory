# 项目经验

本文件是 Claude Code 与 Codex 共享的项目经验唯一权威来源。入口是根 [AGENTS.md](../../AGENTS.md)，Claude Code 通过根 [CLAUDE.md](../../CLAUDE.md) 导入；本机自动记忆只保存索引。操作步骤见 [Driver 开发检查](../../driver/DEVELOPMENT.md)。

## Go 工具与测试（2026-10-06）

- 固定 toolchain 后，扫描器与开发工具的编译器也要匹配；只比工具版本会漏掉旧编译器构建的二进制。安装入口校验包路径、版本、compiler。工具版本查 [锁文件](../../scripts/dev-tools.lock)，toolchain 查 [go.mod](../../driver/go.mod)。
- gotestsum 失败保留原始退出码和 JSON/JUnit；自动重试会遮蔽不稳定行为。修复后显式重跑，并保留前次失败证据。
- 断言 `runx.Spec.Env` 时逐元素精确匹配，例如 `slices.Contains(spec.Env, "KEY="+want)`；拼接后子串匹配会误认前缀、后缀或另一个变量。
- goleak 在 executor、scheduler 的 TestMain 检查整包退出。修复取消、关闭和等待回收的生命周期；宽泛 ignore 会隐藏真实泄漏。用法见开发文档。
- synctest 通过公开 `infra.Retry`、未注入的默认 `Sleep` 检验退避与取消；在隔离副本用受控 mutation 验证断言能失败，避免共享 worktree 暂时改坏生产代码。恢复后运行原始测试，保留已有录制契约。
- `make check` 只覆盖 Go 质量检查；project gate 的 scope、固定基线、契约和部署判定仍须单独满足。

## 部署

部署保留配置、持久状态、正在运行的执行器与 quota 暂停/配额保护；健康检查遇到配额暂停，按既有恢复机制处理，不能清空状态或强行解除保护来制造成功。部署前保留可回滚旧程序。
