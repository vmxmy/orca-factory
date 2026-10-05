# Change shape

- When the project has no external callers and no shipped data, replace the old path in the same change, update every caller, and leave no compatibility shim or data migration. When callers or shipped data exist, keep the contract they already use.
- Delete code, files, and docs that this change made unused. If a deletion is uncertain, ask. A database delete or a data reset waits for an explicit ask.
- When the change needs a behavior test, write the assertion, run it, see it fail, then write only the code that makes it pass. A new test locks a behavior this change can break. Style and copy changes get no new test.
- Run the check that exercises the change and read its output before saying done. Name every path deleted. A check that did not run means the change is not done: name the check and the reason.
- Record a correction in `~/.agents/LESSONS.md` as one line at the top: `When X, do Y`. The same mistake a second time rewrites that line. At 20 lines, merge duplicates, delete stale lines, and show the result.

# Go 开发入口

- 修改 `driver/`、CI、测试或开发工具前，读取 [共享项目记忆](docs/agents/MEMORY.md) 和 [Driver 开发检查](driver/DEVELOPMENT.md)，按变更选择并实际执行其中的 Make 入口。部署前也读取共享记忆中的部署约束。
- 工具版本只以 [scripts/dev-tools.lock](scripts/dev-tools.lock) 为准；Go toolchain 与测试依赖以 [driver/go.mod](driver/go.mod) 为准。通过根 Makefile 安装、运行工具，保留失败退出和报告证据。
- 并发退出检查保留 goleak 整包门禁；重试计时与取消覆盖公开 Retry 的默认 Sleep 路径。具体用法与验证边界见开发文档。

共享记忆由上述指令触发读取；文件名 `MEMORY.md` 本身不构成 Codex 自动加载入口。
