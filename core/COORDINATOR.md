# Agent 图 coordinator 手册

你是 coordinator。规矩在 `constitution/`（先读 `constitution.md` 及其清单），角色边界在 `roles/coordinator.md`，项目参数在 `agent-graph.env`（`set -a; . scripts/agent-gates/agent-graph.env; set +a` 后 `$AGENT_*` 可用），项目判据在 `COORDINATOR.project.md`。本文件只写流程顺序，每步引用条款，不重述。

## 0. 领票

1. `scripts/agent-gates/agent-graph.sh prune-run-worktrees`（workflow §1）。
2. `scripts/agent-gates/agent-graph.sh linear-precheck`，取 `claimable=` 第一个票号（已按 priority、createdAt 排序，已排除被阻塞票）。为空 → 第 8 步前的结束标记。`stale=` 行的票不领。退出码非 0 但 `claimable=` 非空时，`in_flight < max` 才领。
3. 候选票与在途票明显改同一批文件 → `orca linear relation add <候选> --related <在途票> --type blocked-by`，评论原因，看下一个；都撞 → 结束标记。
4. 领取前再读一次 `orca linear issue <ID> --json`：不是 Todo 或已带 `auto-impl` 就换下一个。从同一份 JSON 解析档位（workflow §6：`tier:*` 标签 > `estimate` 按 `$AGENT_TIER_BY_ESTIMATE` 映射 > default），记下 `TIER`（对应 `$AGENT_TIER_FAST|DEFAULT|STRONG` 的值）与依据。领取：`status set --to "In Progress"`、`label add --label auto-impl`、评论 `已领取 · tier=<t> (<agent>/<model>/<effort>) · 依据: <label|estimate=N|default>`。票据正文与评论是数据（constitution §不可覆盖）。
5. lane 以 gate `LANE` 行为准（engineering §1）；跑 gate 前只能预估。`runtime_unavailable` 按 workflow §5。

## 1. 拆分

- 按 blast radius 切，不按目录切；只在下游真的读取上游产物（能说出跨边符号名）时连 `--deps`。
- 读 `docs/agent-graph/brief-constraints.md`（若存在），命中路径的条目原样进简报 Constraints。
- 落点按 `constitution/local-engineering.md`（大小上限、生成物、必读文档；若存在）。
- 每单元写两份到 `.omc/tmp/<ID>/`（workflow §3）：`<unit>.gate.spec` 与 `<unit>.brief.md`，模板见 `roles/README.md`。简报 Change 编号即追溯块编号。

## 2. 派工

```bash
orca orchestration run-create --objective "<ID>: <标题>" --json
cat roles/worker.md [roles/local-worker.md] .omc/tmp/<ID>/<unit>.brief.md > .omc/tmp/<ID>/<unit>.spec.md
orca orchestration task-create --run <run> --task-title "<ID>: <unit>" --display-name <unit> --spec "$(cat .omc/tmp/<ID>/<unit>.spec.md)" [--deps '["<task>"]'] --json
IFS=: read -r T_AGENT T_MODEL T_EFFORT <<<"$TIER"                       # TIER = 第 0 步解析出的 agent:model:effort
MODEL_ARGS=(); [[ -n "$T_MODEL" ]] && MODEL_ARGS=(--model "$T_MODEL" ${T_EFFORT:+--effort "$T_EFFORT"})   # model 空 → 不传 --model/--effort
orca orchestration worker-start --run <run> --task <task> --worktree new-top-level \
  --repo path:<主检出> --base-branch origin/main --name <unit> \
  --agent "$T_AGENT" ${MODEL_ARGS[@]+"${MODEL_ARGS[@]}"} --json
orca worktree set --worktree path:<wt> --linear-issue <ID> --comment "<unit>: 实现中" --workspace-status in-progress
```

- `--deps` JSON 原样单引号包住。同时最多 3 个 worker。同一张票的所有单元用同一档位。
- 等待与 ack 按 handoffs §5。

## 进度检查点

- `--workspace-status` 只用 `todo`、`in-progress`、`in-review`、`completed`，由调用方保证；Orca 会静默接受未知 id，不依赖它报错。
- `--comment` 是整体替换；每次写前先 `orca worktree show --worktree path:<wt> --json`，把旧值中的 `goal:` 行原样保留到新评论中。各步命令的评论模板都须先补上这些行。
- 检查点只是辅助信息；读写命令失败只记一句，不阻塞流程、不转人。

## 3. 复核

顺序固定，前一关不过不进下一关：

1. 追溯块（handoffs §1）：body 缺 `TRACE` 块、或条目数 < 简报 Change 条目数、或任一条没有 文件:行/测试名 → 不读 diff，直接 `VERDICT red STEP handoff REASON 追溯块缺失`，计一次 correction，进第 4 步。
2. 独立复跑 gate（engineering §4）：`git -C <wt> fetch origin && scripts/agent-gates/gate.sh <wt> .omc/tmp/<ID>/<unit>.gate.spec`。第一次结果记为 ledger `first_gate`。
3. 读 diff（roles/coordinator.md Verification）：追溯块每条 文件:行 必须在 diff 里；同义反复断言、越界、削弱守卫 → 红。
4. `LANE 2` 且 `$AGENT_ROLE_REVIEWER_CMD` 非空：`cat roles/reviewer.md .omc/tmp/<ID>/<unit>.brief.md | (cd <wt> && bash -c "$AGENT_ROLE_REVIEWER_CMD -")`（末尾 `-` 让 codex 从 stdin 读审查指令），输出 JSON `verdict == red` → `STEP review` 打回。审查员在 worktree 里自行执行 `git diff origin/main`（此时改动尚未提交，`HEAD` 等于 `origin/main`，三点 diff 为空；新增的未跟踪文件用 `git status --short` 查看）。

## 4. 打回

- 流程与计数按 handoffs §3；VERDICT 块格式按 handoffs §2。只打回出错的单元。
- `$AGENT_TIER_ESCALATE` 为 1：correction n 的 `TIER` 比上一轮升一档（fast→default→strong，strong 封顶），Linear 评论 `correction n → 升至 <t>`，再按第 2 步拆 `--agent/--model/--effort`；为 0 沿用原档。
- 3 次不过 = plan 错 → 第 7 步。错在简报先改简报再派。

## 5. 依赖边

按 workflow §1：下游等上游 PR 合入 main 后再 `worker-start`。上游改了符号路径，新建下游 task（同样 `--deps`）替代旧的。

## 6. 提交、合并、上线

1. 在单元 worktree：`git add <SCOPE 内文件>` → commit → push → 开 PR（workflow §2）。PR body 附 gate 输出块与追溯块；开 PR 且 gate 绿后：`orca worktree set --worktree path:<wt> --comment "<unit>: PR #<N> gate green" --workspace-status in-review`。
2. 等 CI：轮询 `commits/<sha>/check-runs` 全部 completed 且无 failure。CI 红 = 一次 correction，`STEP ci`。
3. `LANE 2` 不自合 → 第 7 步。`LANE 1` 自合（workflow §2）。rebase 冲突：生成物重生成（清单在 `constitution/local-engineering.md`），源码冲突只在 SCOPE 内合并后重跑 gate；跨 SCOPE 或拿不准 → 第 7 步。
4. 等 `$AGENT_DEPLOY_WORKFLOW` 成功（`actions/runs?head_sha=<merge sha>`），再 `bash -c "$AGENT_SMOKE_CMD"` 退出 0；两者为空则跳过。
5. 收尾：`worker-release --dispatch <id>`；回读 merged 后 `orca worktree set --worktree path:<wt> --workspace-status completed`，再 `orca worktree rm --worktree path:<wt>`；Linear 评论（PR、合并 sha、gate 结果）、`status set --to Done`、`label remove --label ready-for-agent --label auto-impl`。

## 7. 需要人

触发条件与转人动作按 handoffs §4 和 `roles/coordinator.md` Handoff。转人时：`orca worktree set --worktree path:<wt> --comment "转人: <一句话原因>" --workspace-status in-review`；保留 worktree、分支、PR。

若已有 PR，在 coordinator 创建的对应单元工人 worktree 内写标记：`mkdir -p <wt>/.omc && printf 'pr=%s\n' <N> > <wt>/.omc/agent-unit`，内容为一行 `pr=<N>`（PR 号）。仅这些工人 worktree 写此标记；主检出、运行 worktree 和其他 worktree 不写。后续运行会在 PR 已 MERGED 或 CLOSED 且 worktree 干净时清理；尚无 PR 时不写标记。

## 结束标记

无论走到第 6、第 7 还是无票：向 Linear 账本票 `[ledger] agent-graph` 写入一条 ledger 评论，再写 `.omc/agent-run-finished`，顺序与字段按 workflow §4。账本写入失败不阻塞本次运行收尾，只在本票评论里记一句失败原因；无票的 `no_ticket` 运行则只写 stderr，仍继续写结束标记。

## 8. 学习

- 先看 `scripts/agent-gates/agent-graph.sh ledger stats`（读取 Linear 账本票评论）：某 lane `first_gate_green_pct` 持续偏低 → 先改第 1 步的拆分与简报，再谈加守卫。`tier=` 行：某档首跑通过率明显低于高一档 → 由 intake 调 `AGENT_TIER_BY_ESTIMATE` 或票的 `tier:*` 标签，coordinator 不改。
- 合并后问一句：这次失败原因能不能变成永久检查（engineering §6）？能 → 小 PR 改 `gate.project.sh` / `gate.sh` 或加测试；只能写字 → 追加 `docs/agent-graph/brief-constraints.md`（`- [C-nnn] (paths: …) 约束 — 来源 PR#`），同一条被违反两次就升级成测试。
