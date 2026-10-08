# 共享条款：工作流

由 orca-factory 分发，项目不得覆盖；项目特化只能写 `local-workflow.md` 追加。

## 1. worktree 纪律

- 一个单元一个 worktree：`worker-start --worktree new-top-level --repo path:<主检出> --base-branch origin/main --name <unit>`。correction 复用原 worktree（`--worktree path:<wt>`，此时不带 `--name/--repo/--base-branch`）。
- 不在主检出切分支、不在主检出改文件。coordinator 的运行目录是 Orca 为本次运行建的 worktree（`$AGENT_RUN_WORKTREE_PREFIX*`）。
- 每次运行开头 `scripts/agent-gates/agent-graph.sh prune-run-worktrees`。
- 单元 worktree 只在 PR 回读 `merged == true` 后删（`orca worktree rm --worktree path:<wt>`）；交给 resolver时保留 worktree、分支与 PR。
- 依赖边：下游等上游 PR 合入 main 后再 `worker-start`，从新的 origin/main 起 worktree。Orca 只看上游 task 是否 completed，不看产物是否进了 main。

## 2. 提交规范

- commit：`fix(<scope>): <摘要> (<票号>)`；`feat` / `chore` / `test` 同理。只 `git add` SCOPE 内文件。
- 分支：`git push -u origin <branch>`。永不 push main，永不 force push。
- PR 用 REST 开（GraphQL 常 EOF）：
  `gh api -X POST repos/$AGENT_GH_REPO/pulls -f title=… -f head=… -f base=main -F body=@<wt>/.omc/tmp/<ID>-pr.md -q .number`
- GitHub 任务 PR body 首行 `关联票: <Issue URL>`，附 canonical identifier / ID。不写会在 squash 时自动关闭业务票的 `Closes` / `Fixes`；部署与 smoke 通过后才 Done。旧 Linear 配置保留 `Fixes <票号>`。
- 合并通道：每个仓库一条串行 FIFO 通道，发布 PR 与功能 PR 按入队顺序共用；队首等待时后续 PR 不超车。使用 squash 合并并回读 `.merged == true`，固定已验证的 head SHA。
- 落后 main（`behind_by > 0`）不是阻塞：取 PR 与 main 相对共同祖先的完整改动路径（含重命名前后路径）求交集。交集为空、`mergeable_state == clean` 且 CI 在当前 head 全绿 → 直接 squash 合并。
- 有文件重叠：用固定的 PR head 与当前 main SHA 执行 `git merge-tree --write-tree`。文本无冲突时，以合并结果建立临时 worktree，在其中运行该单元 gate；gate 绿、当前 head CI 全绿且 mergeability 为 clean 才合并。合并前回读 head 与 main，任一变化则重新验证；连续 2 轮追不上 main → 交给 resolver。验证结束清理临时 worktree，不改写 PR 分支，不 rebase、不 force push。
- 文本冲突：交回调度器启动新执行器解决，计一次 correction；携带已用追赶轮数，连续 2 轮仍追不上 main 则交给 resolver。合并结果 gate 本身判红同样计一次 correction 并交回调度器，不放宽 gate。验证 commit、目录或 worktree 建立失败时保留队首，报告错误或等待，不计 correction；gate 已绿后的清理失败附带报告，继续合并，不计 correction。
- 有部署的仓库：合并后用该 squash commit 等待 `AGENT_DEPLOY_WORKFLOW` 成功，再运行 `AGENT_SMOKE_CMD`；两者通过前保持该仓库通道等待，下一 PR 不合并。部署或冒烟失败 → 交给 resolver并暂停该仓库通道，其他仓库继续；恢复由 resolver 提交绑定队列与 SHA 的证据，由调度器校验执行。

## 3. 暂存文件

- 一切暂存（gate spec、简报、task spec、VERDICT、PR body）放当前 worktree 内 `.omc/tmp/`（`.omc/` 已 gitignore），不放共享 `/tmp`。
- coordinator 的 spec 放运行 worktree 的 `.omc/tmp/<ID>/`，传给 worker 与 gate 时用绝对路径。
- zsh 里别用 `PIPESTATUS`：把输出写到 `.omc/tmp/` 再看退出码。

## 4. 结束标记与 ledger

- 每次运行结束（收尾、交给 resolver、无票）固定两步，顺序不变：
  1. `scripts/agent-gates/agent-graph.sh ledger append '<json>'`，写入当前主账本中标题精确为 `[ledger] agent-graph` 的账本票评论（GitHub 围栏为 `AGENT_GH_REPO`，旧 Linear 围栏为 `AGENT_LINEAR_PROJECT`）（`ledger <JSON>`）。
  2. `mkdir -p .omc && touch .omc/agent-run-finished`（在运行 worktree）
- 账本写入失败不阻塞本次运行收尾：只在本票评论里记一句失败原因（无票的 `no_ticket` 运行则只写 stderr），然后继续写结束标记。
- `ledger stats` 从 当前主账本票评论读取并按 `(ticket, started_at)` 去重；无账本票时输出 `runs=0`，不创建票。
- ledger 行字段：`ticket, lane, tier(fast|default|strong), outcome(merged|escalated|no_ticket), first_gate(green|red), corrections, ci_reds, pr, minutes, started_at, finished_at`。`first_gate` 是 coordinator 第一次独立复跑的结果；`tier` 是领票时解析出的档位（首跑所用，升档不改写）；`corrections` 含 CI 红与追溯块缺失；无票时 `outcome=no_ticket`，票字段省略。
- 不写结束标记 → 下次运行按 24 小时崩溃规则清理。

## 5. runtime_unavailable

- `orca` 返回 `runtime_unavailable`（CLI 连不上本机 Orca，并发时偶发）：隔几秒重试，最多 3 次。
- 不把它当业务结果：它不是"没有阻塞关系"、不是"票不存在"、不是"worker 没完成"。
- 3 次仍失败 → 当次运行交给 resolver（见 handoffs.md §4），不要带着不确定状态继续。

## 6. 档位解析

- 三档在 `agent-graph.env`：`AGENT_TIER_FAST` / `AGENT_TIER_DEFAULT` / `AGENT_TIER_STRONG`，值为 `agent:model:effort`；model 可空（如 `codex::low`）= 该 agent 的默认模型，此时派工不传 `--model` 也不传 `--effort`（`--effort` 依赖 `--model`）。档位顺序 fast < default < strong。
- 领票时按优先级解析，取第一个命中的：
  1. 票标签 `tier:fast` / `tier:default` / `tier:strong`（多个取最高档）；依据写 `label`。
  2. 票 `estimate` 落进 `AGENT_TIER_BY_ESTIMATE` 的某个区间：格式 `lo-hi:tier` 空格分隔，`hi` 空表示无上限（`8-:strong`）；依据写 `estimate=N`。
  3. 其余（无标签、无 estimate、estimate 不落任何区间）→ default；依据写 `default`。
- 领票评论固定格式：`已领取 · tier=<t> (<agent>/<model>/<effort>) · 依据: <label|estimate=N|default>`；model 为空时写 `-`。
- correction：`AGENT_TIER_ESCALATE=1` 时每次 correction 比上一轮升一档，strong 封顶；`=0` 不升。升档时票据评论 `correction n → 升至 <t>`，并用新档的 agent/model/effort 起 worker。ledger `tier` 仍记领票时的档位。
- 解析只读 env 与票面；coordinator 不改 env、不改票标签。
- 实际生效的 agent/model/effort 以 `worker-start` **回执**里的 `launch.effective` 为准，派工时即记录（评论与 ledger 用它，不用解析值）；`worker-show` 不带该字段。回执 model 为空（agent 用自身默认模型）时，用 `ps -axo command | grep -E '^(codex --no-daemon|claude) '` 看真实进程参数核实。
