## 现状

lane 2 审查命令是 `AGENT_ROLE_REVIEWER_CMD='codex exec --json'`（scripts/agent-gates/agent-graph.env:24），与 worker 同属 codex，异构性弱。core/COORDINATOR.md:74 在命令末尾硬拼 ` -`，claude -p 不支持这种写法（会把 `-` 当提示词正文）。

## 要做

1. core/COORDINATOR.md:74 去掉末尾的 ` -` 与"末尾 `-` 让 codex 从 stdin 读审查指令"的说明，改为 `bash -c "$AGENT_ROLE_REVIEWER_CMD"`，注明命令自己从 stdin 读指令。（codex exec 在 stdin 有管道且无位置参数时同样读 stdin，其他接入仓库的 `codex exec --json` 不受影响。）
2. 执行 `bin/orca-factory update .`，让 scripts/agent-gates/COORDINATOR.md 与 core 一致（不手改分发文件）。
3. scripts/agent-gates/agent-graph.env 的 AGENT_ROLE_REVIEWER_CMD 改为：
   claude -p --model claude-opus-5-5 --permission-mode dontAsk --allowedTools "Read Grep Glob Bash(git diff:*) Bash(git status:*) Bash(git log:\*)"
4. 不改 templates/、examples/、ONBOARD.md、其他接入仓库的默认值（codex 默认保持不变）。

## 验收

CHECK: set -a; . scripts/agent-gates/agent-graph.env; set +a; \[\[ $AGENT_ROLE_REVIEWER_CMD == claude\\ -p\* \]\] && ! grep -q 'AGENT_ROLE_REVIEWER_CMD -"' core/COORDINATOR.md scripts/agent-gates/COORDINATOR.md && bin/orca-factory check . >/dev/null
断言: 审查命令是 claude -p 形式；两份 COORDINATOR.md 不再硬拼 ` -`；core 与安装副本无漂移。

## LANE 预判

2（依据：命中 `$AGENT_LANE2_GLOBS` 的 `core/*`、`scripts/agent-gates/*`）

## SCOPE

core/COORDINATOR.md scripts/agent-gates/COORDINATOR.md scripts/agent-gates/agent-graph.env

## 依赖

无

## intake 自检

* CHECK 可执行且会红：是。main 上实跑退出码 1（命令仍是 codex，且 COORDINATOR.md 仍含 ` -"`）。
* SCOPE 路径存在：是（三个文件均已存在）。
* lane 预判与 glob 一致：是，命中 `core/*` 与 `scripts/agent-gates/*`，未命中 lane 3。
* project / Todo / ready-for-agent：是。
* 依赖：无。
* estimate=2 → 预计 tier=fast
