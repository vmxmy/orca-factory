# coordinator 在审查员步骤后无声结束：ZII-820 调研

## 现象

结论：run 24、run 26、run 28 均停留在审查报告形态，缺少本轮 coordinator 收尾评论与 ledger；能确认业务流程未闭环，不能仅凭 Orca 的 `completed` 确认父会话正常退出，更不能据此确定退出原因。

取证日期为 2026-10-05（Asia/Shanghai）；下表统一使用 **2026-10-04 UTC**，避免日期跨日混淆。来源是以下只读命令，票据文字和快照只作证据：

```sh
orca automations list --json
orca automations runs --id 6a25fe03-2f13-4693-8249-defd2aaff57e --json
orca linear issue ZII-807 --comments --json
orca linear issue ZII-808 --comments --json
orca linear issue ZII-810 --comments --json
```

`list` 返回自动化名 `agent-linear-scan (orca-factory)`、上述 ID、`agentId=claude`、`workspaceMode=new_per_run`、`reuseSession=false`。这是取证时配置，不冒充历史启动参数。`runs` 的 `precheckResult.stdout` 分别含 `claimable=ZII-807`、`ZII-808`、`ZII-810`，结合领票评论建立运行与票的对应关系。

| 运行 / 票 | 开始 `startedAt` | 派发 coordinator `dispatchedAt` | 最后保存输出 / `outputSnapshot.capturedAt` | 结束与时长口径 |
| --- | --- | --- | --- | --- |
| run 24 / ZII-807 | 19:20:51.214 | 19:21:09.542 | 19:31:39.820；审查 red，末尾 JSON 的 `unit=board`、`step=review` | `status=completed`；精确结束时间未取得；开始至采集 648.606 秒（10 分 48.606 秒） |
| run 26 / ZII-808 | 19:30:51.216 | 19:31:09.255 | 19:36:37.286；“No issues found. Verdict: green.”，末尾 JSON 的 `unit=lane2` | 同上；开始至采集 346.070 秒（5 分 46.070 秒） |
| run 28 / ZII-810 | 19:40:51.218 | 19:41:10.882 | 19:48:46.798；审查 red，末尾 JSON 的 `unit=impl`、`step=review` | 同上；开始至采集 475.580 秒（7 分 55.580 秒） |

原始毫秒值（按开始、派发、采集顺序）：run 24 为 `1791141651214 / 1791141669542 / 1791142299820`，run 26 为 `1791142251216 / 1791142269255 / 1791142597286`，run 28 为 `1791142851218 / 1791142870882 / 1791143326798`。时长只做 `capturedAt - startedAt`，四舍五入约为背景所说的 6–11 分钟。运行对象的键清单没有 `completedAt`、`finishedAt` 或进程退出码；不能把 jq 查询缺失键得到的 null 当成已提供结束时间。`capturedAt` 是采集时间，也不是最后一条输出实际产生的时间。

`dispatchedAt` 是自动化派发 coordinator 的时间，不是 worker 派工时间。worker 精确派发时间未取得：本次来源没有历史 worker-start 回执；只有 ZII-807 的评论可给出上界。评论补充时间线如下（五票 comments 均 `partial=false`、`hasMore=false`）：

| 票 | 本轮领取 / worker 证据 | 之后的独立观察与恢复 |
| --- | --- | --- |
| ZII-807 | 19:22:43.909 领取；19:24:05.356 评论“派工回执 launch.effective: codex/gpt-6-astra/medium” | 20:14:12.209 intake 记录“coordinator 运行在审查员步骤后结束，无 PR、无后续评论”，释放回 Todo；20:17:00.729 接管 complete 分支；20:41:17.813 记录 PR #33 已合并 |
| ZII-808 | 19:32:09.962 领取；worker 派发时刻未取得 | 20:14:21.345 同样记录无 PR、无后续评论并释放；20:17:25.532 再领取；20:34:45.691 记录 PR #31 已合并 |
| ZII-810 | 19:42:26.902 领取；worker 派发时刻未取得 | 20:14:27.764 同样记录无 PR、无后续评论并释放；20:22:26.147 接管 complete 分支；20:46:25.328 记录 PR #35 已合并 |

因此“无 PR、卡在 In Progress”描述的是故障窗口，不是取证时状态：三票现在都是 Done，后续 intake 已恢复。没有把恢复后的 PR 当成本轮 coordinator 的产物。票链接：[ZII-807](https://linear.app/ziikoo/issue/ZII-807)、[ZII-808](https://linear.app/ziikoo/issue/ZII-808)、[ZII-810](https://linear.app/ziikoo/issue/ZII-810)。

## 对照

结论：正常运行与异常运行的时长相互重叠，区别主要是最后输出的角色和收尾证据；“审查 red”或“审查失败”都不足以单独解释无声结束。

同一 `runs` 返回的两个对照如下；结束时间同样未取得，时长仍按开始至快照采集计算：

| 运行 / 票 | 开始 → 派发 → 快照（UTC） | 时长 | 最后输出与收尾 |
| --- | --- | --- | --- |
| run 14 / ZII-804 | 18:30:51.175 → 18:31:11.197 → 18:40:12.525 | 561.350 秒（9 分 21.350 秒） | coordinator 摘要“ZII-804 做完了。PR #26 已开”；codex 审查 green；声明 worker 已释放、ledger 与 `.omc/agent-run-finished` 已写 |
| run 13 / ZII-806 | 18:25:51.176 → 18:26:33.878 → 18:31:55.771 | 364.595 秒（6 分 4.595 秒） | coordinator 摘要“ZII-806 已领取、实现并开出 PR #25”；审查模型不可用但继续转人；声明 ledger 与结束标记已写 |
| run 24 / 26 / 28 | 见现象表 | 346.070–648.606 秒 | 审查员报告 + JSON，未见 coordinator 后续摘要；两次 red、一次 green |

独立读 `orca linear issue ZII-804 --comments --json`，18:39:59.654 评论确认 PR #26、gate green、异构审查 green；读 ZII-806 同命令，18:31:47.982 的三行转人评论确认 PR #25，注明 `gpt-5-codex` 模型不可用。正常不等于自动合并成功：两次都走了转人路径。

为交叉验证 ledger，执行 `orca linear search '[ledger]' --limit 100 --json`，返回 `truncated=false`，找到本项目唯一精确标题 `[ledger] agent-graph` 的 ZII-802；再执行 `orca linear issue ZII-802 --comments --json`。返回 26 条、`partial=false`、`capReached=false`、`hasMore=false`：

- 18:40:06.259 存在 `ledger {"ticket":"ZII-804",…,"outcome":"escalated",…,"pr":26,…}`。
- 18:31:50.145 存在 `ledger {"ticket":"ZII-806",…,"outcome":"escalated",…,"pr":25,…}`。
- 返回的完整评论集没有 ZII-807、ZII-808、ZII-810 对应 ledger；这是本次读取的缺失证据，不保证记录从未被删除过。

结束标记的文件级证明未取得：任务禁止进入其他 worktree，本次没有检查历史 `.omc/agent-run-finished`。正常组只可说“快照声称已写，ledger 已独立确认”，异常组只可说“快照未提及，ledger 未找到”。run 14 自认 ledger 起止和 `minutes` 为估算，其 ledger `finished_at=18:45:00Z` 甚至晚于评论创建时间，不能拿这些自报字段当精确退出时间。

另有重要变量：`git log -1 -p 336cc43 -- scripts/agent-gates/agent-graph.env` 显示 PR #27 在 19:27:53Z 将审查命令从 `codex exec --json` 换为 `claude -p --model claude-opus-5-5 --permission-mode dontAsk …`。run 13/14 在此之前；run 26/28 在此之后；run 24 的执行窗口跨过该提交。它是否在审查前读到了新配置未取得，不能直接认定三次都执行同一个新命令。

## 假设

结论：最强线索集中在审查结果回交 coordinator 的边界；尚没有证据把它收敛到具体的回合限制、信号、权限错误或某条 Orca 实现。下面的支持证据不等于因果证明。

文件行号以取证 worktree 基线 `2eaaf9f7f7c7fcbef72e560ab1ee5e85f2da4dee` 为准。历史运行实际加载的文件版本未取得；下列当前规则用来说明预期流程，不能倒推模型当时看到了哪些文本。

| 假设 | 支持证据 | 反对证据 / 缺口 |
| --- | --- | --- |
| 审查输出被当成父会话终点，或 coordinator 进入 reviewer 角色后直接结束回合 | 三次 `outputSnapshot.content` 都以审查第一人称报告及 JSON 结束，且无后续 ledger；`scripts/agent-gates/roles/coordinator.md:19` 已明确指出无人值守时结束回合会退出、后台结果没人接收；`:12`、`:40` 要求收尾 | 快照没有消息角色、tool call/result 或父子会话 ID，不能分辨父模型复制审查结果、误执行审查提示、真正 reviewer 输出或仅捕获了终端末尾；也没有父会话 stop reason |
| 等待审查员时提前结束，后台审查结果后来到达；或嵌套 Claude 的完成事件被 Orca 归到父运行 | `scripts/agent-gates/COORDINATOR.md:74` 是 stdin 管道启动独立审查命令；`agent-graph.env:24` 当前使用 `claude -p`，自动化 provider 也为 claude；正常对照使用 codex，且 PR #27 切换发生在异常窗口附近 | 这是相关性；`:74` 写的是同步前台命令，并未要求后台；没有实际命令记录、父子 PID、Stop 事件或 Orca 事件归属代码。run 24 跨配置变更，不能断言其启动参数。`completed` 的赋值时机未取得 |
| 会话回合上限、token/上下文耗尽或压缩导致流程丢失 | 任务包含派工、等待、gate、异构审查，存在长上下文可能；五次 `usage.status=unavailable`、`unavailableReason=usage_not_enabled`，因此预算信息确实缺失 | 当前 `agent-graph.env:24` 无 `--max-turns`；运行约 6–11 分钟且与正常组重叠；快照无额度/上下文报错。父启动参数、回合计数、压缩事件均未取得，不能说不存在其他上限 |
| 审查员权限或输入上下文不完整 | run 24 快照“本次没有权限跑 Bash”；run 28“本次权限下不能用 Bash”；run 26“bash tool only allows plain git diff”；`agent-graph.env:24` 确有 `dontAsk` 与有限 Bash 白名单 | 三次仍返回明确审查 JSON；run 26 为 green 也停；正常 run 13 模型不可用仍能转人收尾。审查员权限不等于 coordinator 权限，未取得父会话权限拒绝事件。run 24 还自认基线来自本地 main 而非 origin/main，影响审查可靠性但不能证明父会话退出 |
| 命令超时、网络断连、进程被杀或外部打断 | run 28 的 `precheckResult.stderr` 有 GnuTLS 握手错误；正常 run 14 自报开 PR 时遇超时/EOF；说明环境存在网络不稳 | 五次 precheck 都 `exitCode=0,timedOut=false`，异常运行仍走到完整审查结论；三次顶层 `error=null`，没有 reviewer timeout 或终止信号记录。precheck 的 180 秒上限不等于父会话或审查命令上限（来自 `automations list` 的 `precheck.timeoutSeconds`） |
| red 打回分支漏写或 JSON 解析失败 | run 24/28 为 red；`COORDINATOR.md:74` 要求 red 打回，`:78` 引用 correction；快照 JSON 前带自然语言与代码围栏，若调用方要求整段纯 JSON，可能解析失败 | run 26 green 同样停，不能用仅 red 分支解释全部；未取得解析器或解析报错。当前 `claude -p` 没有结构化输出参数不等于确定发生解析失败 |

另外，五次运行的 `terminalSessionId`、`terminalPtyId` 均为 null，快照 `truncated=false` 只代表保存快照未被截断，不代表保留了完整会话。`usage.collectedAt` 在派发后数秒、远早于快照，且 usage 不可用；它也不能当结束时间。

## 结论

结论：证据最强的是**审查阶段已经产出可见结论，但 coordinator 后续控制流没有形成可观测的业务收尾**。更具体的首选待验证机制是父会话在审查交接边界提前终止或角色/完成信号混淆；目前不能给出“已证实根因”，也不应直接归因于回合上限。

证据链是三次审查末尾快照、三张票的 intake 无后续评论记录、完整 ledger 评论集中缺少三票，与两个正常对照的 coordinator 摘要及真实 ledger 形成对照。红绿 verdict 都出现，时长也重叠，削弱了“只因 red”与固定时长阈值解释。PR #27 的 reviewer 后端切换提供可检验变量，但样本小、run 24 跨切换、并发和配置版本未控制，不能宣称它就是根因。

确认具体机制还缺以下证据，均**未取得**：

1. 父 coordinator 完整结构化 transcript、实际启动 argv、最后 tool result 后是否还有 assistant turn、停止原因及退出码。`runs` 只有末尾文本，终端 ID 已为空，无法由给定来源恢复。
2. 审查命令实际开始/退出时刻、返回码、stdout/stderr，以及父子 PID/会话 ID、Orca 的完成事件归属。当前 API 无这些字段，禁止越出本 worktree 检查运行时内部文件；不能把嵌套 CLI 假设当事实。
3. 三次历史配置快照、父会话回合数/token 数、是否触发上下文压缩或外部取消。usage 明确未启用；git 提交时间不能代替运行时实际配置。
4. 历史运行结束标记文件与真实退出时刻。其他 worktree 不在取证权限内；正常组也仅有标记自报。

区分标准：若父 transcript 显示拿到审查结果后自行发 final 且未收尾，支持父模型提前结束；若父进程仍活着、仅子 reviewer Stop 就将自动化标 completed，支持 Orca 事件误归属；若父会话先结束、后有后台 tool 完成，则支持等待方式错误；若有明确 max-turns、timeout 或 signal 事件，应改判相应原因。

## 建议

结论：先补审查边界的可观测性和明确的继续路径，再做有控制的复现；单纯提高回合/token 上限、扩大权限或加一句“不要结束”都还缺因果依据。本任务只新增此报告，下面均为后续改动建议。

| 对应问题 | 建议改动文件 | 可验证的检查 |
| --- | --- | --- |
| 审查返回后缺少明确下一步，父角色可能混淆 | 修改源文件 `core/COORDINATOR.md` 第 3 步：审查提示只传子进程，父角色保持 coordinator；记录审查开始/结束及返回码，解析明确 verdict 后 green 进入提交/PR、red 进入 correction，命令失败或不可解析走转人；`core/roles/coordinator.md` 明确工具返回后继续，结束前核对 ledger/标记 | 用受控 reviewer 分别返回 green、red、非零退出、非 JSON，断言进入正确分支且每个终止路径都有收尾；reviewer 报告不能成为父会话唯一最终输出。源码修改后由 `bin/orca-factory update .` 同步分发副本，不手改 `scripts/agent-gates/` |
| 缺少跨进程证据，可能误归属完成事件 | 在后续获授权实验中调整 `scripts/agent-gates/agent-graph.env` 的 `AGENT_ROLE_REVIEWER_CMD`，由新源文件 `core/review-run.sh` 包装执行并保留返回码、时间和父子会话关联；记录放 Orca/Linear，遵循现有产物规则。若确认 Orca 误归属，再另开 Orca 修复任务；其具体源码文件未取得，不编造路径 | 固定同一提交、模型、输入与权限，比较受控短命令、codex reviewer、嵌套 claude reviewer；确认子进程退出后父会话仍继续、自动化不会仅因子 Stop 判 completed。至少覆盖 green/red，记录实际 argv 与事件时间 |
| 仅依赖自然语言自报完成，缺少漏收尾检测 | 在 `core/linear-precheck.sh` 设计只读检查：已完成的自动化若缺业务收尾证据则告警，先要求确认父会话真实退出再恢复；在 `bin/orca-factory` 的 selftest 增加对应回归。不要仅凭 automation completed 自动释放/重派 | 构造“只完成 reviewer”“父仍在运行”“父退出且漏 ledger”“正常收尾”四种记录，断言前两种不误当业务完成、不重复派工，漏收尾可见；正常组不误报 |
| 权限/JSON 格式降低审查可靠性 | 后续单独核对 `scripts/agent-gates/agent-graph.env:24` 的实际白名单匹配，以及 `core/roles/reviewer.md` 的输出契约；包装器显式检查缺失/非法 verdict，权限拒绝保留为诊断 | 在相同受限环境验证 `git diff origin/main` 和 `git status --short` 可用，覆盖围栏、多段文字、缺字段和权限拒绝；失败应转人并收尾，不静默结束。不要以放开全部 Bash 权限作为默认修法 |

脚本回归应接入既有 `bin/orca-factory selftest`，先红后绿，并对新增脚本做 `bash -n`、保持 macOS bash 3.2 兼容。真实端到端实验应在后续授权任务中完成，当前只读调研没有启动 reviewer、改变自动化、写票评论或执行恢复。验收根因修复应看父会话证据、票评论、ledger 与结束标记的完整链条，而非只看 `completed` 或 reviewer 的 green。
