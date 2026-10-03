# 共享条款：工作流

由 orca-factory 分发，项目不得覆盖；项目特化只能写 `local-workflow.md` 追加。

## 1. worktree 纪律

- 一个单元一个 worktree：`worker-start --worktree new-top-level --repo path:<主检出> --base-branch origin/main --name <unit>`。correction 复用原 worktree（`--worktree path:<wt>`，此时不带 `--name/--repo/--base-branch`）。
- 不在主检出切分支、不在主检出改文件。coordinator 的运行目录是 Orca 为本次运行建的 worktree（`$AGENT_RUN_WORKTREE_PREFIX*`）。
- 每次运行开头 `scripts/agent-gates/agent-graph.sh prune-run-worktrees`。
- 单元 worktree 只在 PR 回读 `merged == true` 后删（`orca worktree rm --worktree path:<wt>`）；转人时保留 worktree、分支与 PR。
- 依赖边：下游等上游 PR 合入 main 后再 `worker-start`，从新的 origin/main 起 worktree。Orca 只看上游 task 是否 completed，不看产物是否进了 main。

## 2. 提交规范

- commit：`fix(<scope>): <摘要> (<票号>)`；`feat` / `chore` / `test` 同理。只 `git add` SCOPE 内文件。
- 分支：`git push -u origin <branch>`。永不 push main，永不 force push。
- PR 用 REST 开（GraphQL 常 EOF）：
  `gh api -X POST repos/$AGENT_GH_REPO/pulls -f title=… -f head=… -f base=main -F body=@<wt>/.omc/tmp/<ID>-pr.md -q .number`
- PR body 首行 `Fixes <票号>`。
- 合并：`mergeable_state == clean` 后 `gh api -X PUT repos/…/pulls/<n>/merge -f merge_method=squash`，回读 `.merged == true`。`behind_by > 0` 先 rebase 重跑 CI。

## 3. 暂存文件

- 一切暂存（gate spec、简报、task spec、VERDICT、PR body）放当前 worktree 内 `.omc/tmp/`（`.omc/` 已 gitignore），不放共享 `/tmp`。
- coordinator 的 spec 放运行 worktree 的 `.omc/tmp/<ID>/`，传给 worker 与 gate 时用绝对路径。
- zsh 里别用 `PIPESTATUS`：把输出写到 `.omc/tmp/` 再看退出码。

## 4. 结束标记与 ledger

- 每次运行结束（收尾、转人、无票）固定两步，顺序不变：
  1. `scripts/agent-gates/agent-graph.sh ledger append '<json>'`
  2. `mkdir -p .omc && touch .omc/agent-run-finished`（在运行 worktree）
- ledger 行字段：`ticket, lane, outcome(merged|escalated|no_ticket), first_gate(green|red), corrections, ci_reds, pr, minutes, started_at, finished_at`。`first_gate` 是 coordinator 第一次独立复跑的结果；`corrections` 含 CI 红与追溯块缺失；无票时 `outcome=no_ticket`，票字段省略。
- 不写结束标记 → 下次运行按 24 小时崩溃规则清理。

## 5. runtime_unavailable

- `orca` 返回 `runtime_unavailable`（CLI 连不上本机 Orca，并发时偶发）：隔几秒重试，最多 3 次。
- 不把它当业务结果：它不是"没有阻塞关系"、不是"票不存在"、不是"worker 没完成"。
- 3 次仍失败 → 当次运行转人（见 handoffs.md §4），不要带着不确定状态继续。
