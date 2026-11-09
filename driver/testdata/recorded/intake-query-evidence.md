# intake 查询证据样本（OF-427）

采集日期：2026-11-09，本机 driver `1fe4001`。

| 文件 | 命令 | 投影与脱敏 |
| --- | --- | --- |
| `driver-status-json-stale.json` | `orca-factory-driver status --json` | 删除每行 `url`；其余字段原样。采集时 serve 处于 draining，快照本身为 `stale`。 |
| `driver-maintenance-status.json` | `orca-factory-driver maintenance status` | 原样。 |
| `driver-logs-executor-auth-failure.jsonl` | `~/.local/state/orca-factory/orca-factory/OF-427/executor.log`（`orca-factory-driver logs OF-427 --json` 读取的同一文件） | 取 2026-11-07 第一次运行的 7 行，删除含完整 shell 脚本参数的一行。 |
| `driver-logs-scheduler-repository.jsonl` | `~/.local/state/orca-factory/scheduler.log` | 取一条同时带 `ticket` 与 `repository` 的记录，原样。 |
| `driver-journal-scheduler-decision.jsonl` | `journalctl --user -u orca-factory-driver -o cat`（2026-11-09 09:45） | 取 OF-425 的两条 `scheduler decision` 记录，原样；`repo` 为检出路径，不带 `repository`。 |
| `issues-read-OF-427.json` | `scripts/agent-gates/agent-graph.sh issues read --id OF-427 --json`（2026-11-09） | 描述只保留第一段；删除含 `resolver-state` 或 `merge-` 的评论，保留 18 条；其余字段原样。 |
| `claude-print-json-intake-query.json` | 在 `20c3a58` 检出中运行 `claude -p --output-format json --model claude-opus-5-5 --effort high --append-system-prompt-file scripts/agent-gates/roles/intake-chat.md --tools Read,Grep,Glob --permission-mode dontAsk --strict-mcp-config`，提问 OF-427 为何未被领取 | 删除 `credential_expires_at`；其余字段原样。`result` 请求 4 次查询。 |

测试中另行构造的跨项目同票号、超量记录、凭据形态文本、空日志与读取失败样本均为合成数据，在测试代码中标明，不是现网录制。
