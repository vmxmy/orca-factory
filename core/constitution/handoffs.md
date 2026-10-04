# 共享条款：交接

由 orca-factory 分发，项目不得覆盖；项目特化只能写 `local-handoffs.md` 追加。

## 1. worker_done

- 完成只有一种信号：从派工终端发一次 `worker_done`，命令按注入前言，形如
  `orca orchestration send --from <handle> --dispatch-capability <cap> --type worker_done --subject "<状态>" --body "<body>" --task-id <task> --dispatch-id <dispatch> --outcome succeeded|failed --files-modified <逗号分隔>`。
- heartbeat、status、终端里的"done"都不是完成。
- `--outcome` 必写；失败不得只写在正文里。
- `--outcome succeeded` 的 body 必含两块，顺序固定：
  1. 追溯块，首行 `TRACE`，之后每条一行：
     ```
     TRACE
     REQ 1 → web/src/lib/foo.ts:42 → CHECK
     REQ 2 → web/src/lib/foo.test.ts:"rejects empty id" → check-red-on-base
     ```
     `REQ n` 对应简报 `Change` 第 n 条；条目数 ≥ Change 条目数；每条都有 文件:行 或测试名，末段是 gate 步名或 `CHECK`。
  2. gate 输出块：`GATE_PROJECT_SRC=worktree gate.sh` 的七行原样粘贴。
- `--files-modified` 的值来自 `git diff --name-only HEAD` 加 `git ls-files --others --exclude-standard`。
- `--outcome failed` 的 body：卡在哪一步、最后一次 gate 输出块、需要 coordinator 做什么。

## 2. VERDICT 块

gate 输出、coordinator 打回、reviewer 结论都用同一格式：

```
UNIT      <unit>
VERDICT   red|green
STEP      <scope|suppression|check-red-on-base|<project 步名>|check|lane|handoff|review|ci>
REASON    <一句话>
EVIDENCE  <文件:行 + 输出摘录，≤20 行>
SCOPE     <gate spec 的 SCOPE>
LANE      <1|2|3>
```

- `STEP handoff`：追溯块缺失或不完整（coordinator 不读 diff 直接打回）。
- `STEP review`：异构审查打回。
- `STEP ci`：PR CI 红，EVIDENCE 取 `gh run view <id> --log-failed`。
- reviewer 的 VERDICT 以 JSON 输出：`{"unit","verdict","step":"review","reason","evidence":[{"file","line","note"}],"scope","lane"}`。

## 3. correction

- `--retry-of` 只接受失败的 dispatch。worker 自报 succeeded 被驳回 → 走 `--parent` 子任务：
  1. `orca orchestration task-update --id <task> --status failed --result '{"verdict":"<VERDICT 块>"}'`
  2. `orca orchestration task-create --run <run> --parent <task> --task-title "correction n/3: <unit>" --spec "<correction n/3 + VERDICT 块 + 修复步骤 + 原简报>"`
  3. `orca orchestration worker-start --run <run> --task <child> --worktree path:<原 wt> --agent <agent> [--model <model> --effort <effort>]`，agent/model/effort 来自本轮档位（`workflow.md §6`：`AGENT_TIER_ESCALATE=1` 则比上一轮升一档，并评论 `correction n → 升至 <t>`）
  4. 通过后 `task-update --id <task> --status completed`
- 只打回出错的单元，不打回整批。
- 每单元 3 次封顶。追溯块缺失计一次，CI 红计一次，review 打回计一次。第 4 次 = plan 错，停，转人。
- 错在 coordinator 写的简报（如把导出放进已满的文件）→ 先改简报再派 correction，这一次也计数。

## 4. escalation / ask

- worker 遇到以下任一情况发 `escalation`，不硬做：需越 SCOPE、简报自相矛盾、CHECK 写不出会红的断言、依赖装不上、gate 因 lane 判红。
  `orca orchestration send --from <handle> --dispatch-capability <cap> --type escalation --subject "Blocked: <原因>" --body "<证据>" --task-id <task> --dispatch-id <dispatch>`
- 单一事实问题（符号名、落点、二选一）用 `ask`，阻塞等 `reply`；超时按简报默认值做并在 worker_done body 里注明。
- coordinator 收到 escalation：能改 plan 就改 plan 新建 task 重派；否则转人。
- 转人（coordinator 对人的 handoff）：Linear `label remove --label ready-for-agent`、`label add --label auto-failed`，评论固定三行，顺序不变；保持 `auto-impl` + In Progress；保留 worktree、分支、PR。
  ```text
  转人: <一句话卡点>
  证据: <文件:行 / PR / worktree 路径 / 命令输出摘录>
  需要你: <一个具体动作>
  ```

## 5. 等待

- `orca orchestration check --run <run> --types worker_done,escalation,question --wait --timeout-ms 1800000 --json`；处理完 `--ack <deliveryId>`，否则下次还返回同一批。
- heartbeat 不是完成。连续 3 次空等 → `worker-list --run <run> --json` 看每行 `projection.nextAction`，按 Orca 编排协议处理；没有 worker_done 就不能 release、不能重派。
