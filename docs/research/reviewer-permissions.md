# lane 2 审查员 Bash 权限调研（ZII-821）

## 复现

结论：2026-10-05 在本 worktree 实测，原配置拒绝了 `git -C <当前绝对路径> diff origin/main`，普通 `git diff`、三点比较、`--stat`、本次管道及复合命令均通过。判断依据是工具调用与结果的 ID 配对，不是审查员最后的文字总结。

环境：Claude Code `2.1.289`，模型 `claude-opus-5-5`，cwd 为 `/home/laoxu/orca/workspaces/orca-factory/research-2`。配置来源为 `scripts/agent-gates/agent-graph.env:24`，启动方式来源为 `scripts/agent-gates/COORDINATOR.md:74`。未改配置。临时简报、stdout 和工具日志副本均在当前 worktree 的 `.omc/tmp/reviewer-permissions/`；关键证据在本报告内，不依赖临时文件长期保留。

原值如下（外层单引号是 env 的赋值语法，内部双引号不是字面的反斜杠转义）：

```sh
AGENT_ROLE_REVIEWER_CMD='claude -p --model claude-opus-5-5 --permission-mode dontAsk --allowedTools "Read Grep Glob Bash(git diff:*) Bash(git status:*) Bash(git log:*)"'
```

先运行简报 CHECK，文件不存在，退出码 **1**，无 stdout。然后仅在 SCOPE 文件写入三行探针，并 `git add docs/research/reviewer-permissions.md`，保证未提交新增内容也可被 `git diff origin/main` 读到：

```text
# 审查员权限调研

复现标记：REVIEW_DIFF_PROBE_821
```

第一次 stdin 为原始 `scripts/agent-gates/roles/reviewer.md` 加以下简报，角色原文未删改：

```text
# 复现简报 ZII-821 / research
Change:
1. 新增 docs/research/reviewer-permissions.md，内容为标题与复现标记 REVIEW_DIFF_PROBE_821。
SCOPE: docs/research/reviewer-permissions.md
只读审查当前 worktree 相对 origin/main 的完整 diff，报告你实际读到的新增内容。
```

实际启动命令（bash 内执行）：

```sh
source scripts/agent-gates/agent-graph.env
cat scripts/agent-gates/roles/reviewer.md .omc/tmp/reviewer-permissions/brief.md > .omc/tmp/reviewer-permissions/prompt.md
bash -c "$AGENT_ROLE_REVIEWER_CMD" < .omc/tmp/reviewer-permissions/prompt.md > .omc/tmp/reviewer-permissions/original.log 2>&1
```

第一次审查退出 **0**，session `c32950e8-1572-4e52-a7c2-1dc1fdcaf538`。以下是它尝试的全部三条 Bash 调用；全被许可执行，第二条内部含 Git 参数错误：

```sh
git status --short && echo --- && git diff origin/main --stat && echo --- && git diff origin/main && echo --- && ls scripts/agent-gates/constitution/
git log --oneline -3 origin/main && echo --- && git merge-base HEAD origin/main && git rev-parse HEAD && echo --- && git diff --cached --stat origin/main...HEAD; git diff --cached --stat HEAD && echo --- && git log --oneline HEAD..origin/main && echo --- && sed -n '1,80p' scripts/agent-gates/constitution/handoffs.md
cat scripts/agent-gates/constitution/constitution.md | head -60
```

输出摘录：`A  docs/research/reviewer-permissions.md`；完整补丁含 `+复现标记：REVIEW_DIFF_PROBE_821`；第二条出现 `usage: git diff [<options>] [<commit>] [--] [<path>...]`。后者来自 `--cached` 与三点形式混用，不是权限拒绝。首轮 HEAD 为 `2eaaf9f`，而共享 `origin/main` 已前进至 `398dae6`，因此双端比较还显示 `docs/research/worktree-cleanup.md` 的基线差异；没有删除这个路径。

将探针作里程碑提交、rebase 到 `origin/main` 后，HEAD 为 `2d668df05c9cbeef46ad7bf783d552abf3e3c667`，相对 `398dae6` 只新增三行探针。第二轮仍使用原命令原值，在同一简报末尾追加下表六条只读观测命令，要求逐条独立执行、不合并、不替换，再审完整 diff；重新拼接角色与简报为 `prompt-probes.md`，用相同 `bash -c "$AGENT_ROLE_REVIEWER_CMD"` 启动，stdout 存 `probes.log`。

第二轮退出 **0**，session `a9b99b96-d6ce-4d18-ae53-f84c8c8e14e6`。以下为全部七条 Bash 调用（首条为审查员自行读取宪章，其余是六条探针）：

| 命令原文 | 工具结果 |
| --- | --- |
| `ls scripts/agent-gates/constitution/ && cat scripts/agent-gates/constitution/constitution.md` | 通过，列出四份宪章文件并输出入口正文 |
| `git diff origin/main...HEAD` | 通过，完整三行新增补丁 |
| `git diff origin/main --stat` | 通过，`1 file changed, 3 insertions(+)` |
| `git -C /home/laoxu/orca/workspaces/orca-factory/research-2 diff origin/main` | **拒绝**，`is_error: true` |
| `git diff origin/main \| head -80` | 通过，完整三行新增补丁 |
| `git status --short && git diff origin/main` | 通过，status 为空，随后完整补丁 |
| `git diff --no-ext-diff --no-textconv origin/main` | 通过，完整三行新增补丁 |

表格中 `\|` 仅为 Markdown 转义；实际 Bash 字符串含普通 `|`。拒绝证据摘录：

```text
tool_use.id: toolu_01H6hzusFgcJrEipNeqj9Zx5
input.command: git -C /home/laoxu/orca/workspaces/orca-factory/research-2 diff origin/main
tool_result.tool_use_id: toolu_01H6hzusFgcJrEipNeqj9Zx5
is_error: true
Permission to use Bash has been denied because Claude Code is running in don't ask mode.
```

管道调用 ID 为 `toolu_01PZ9rS89WoEoqK7zFdceo7p`，对应 `is_error: false`，输出含完整探针。审查员最终文字把第 4 条管道写成“被拒”、把第 3 条 `git -C` 写成成功，与工具日志相反，因此不能采用它的权限汇总。权限拒绝也未使整个审查进程非零退出，退出 0 不代表零拒绝。

## 原因

`Bash(git diff:*)` 覆盖以 `git diff` 开头的写法；拒绝调用的前缀却是 `git -C /home/laoxu/orca/workspaces/orca-factory/research-2 diff`，既不匹配该前缀，也不匹配 `Bash(git status:*)` 或 `Bash(git log:*)`。Read、Grep、Glob 授权的是独立工具，不能替 Bash 调用授权。上述拒绝结果与第三轮固定路径精确规则成功，共同支持这是本次重现的缺口。

实测不支持“三点范围或 `--stat` 本身导致拒绝”：两者都通过。也不支持“所有管道和复合命令都会拒绝”：两轮中的 `head` 管道与 `status && diff` 都通过。不能把这些有限样本推广到任意命令组合。[Claude Code 官方权限文档](https://code.claude.com/docs/en/permissions#compound-commands)说明复合命令按子命令分别判断；其[只读命令说明](https://code.claude.com/docs/en/permissions#read-only-commands)解释为何某些未列入白名单的读取也可通过。`dontAsk` 对仍需批准的调用直接拒绝。[来源](https://code.claude.com/docs/en/permissions#permission-modes)

首轮没有自然产生 `git -C`，所以未复现；第二轮明确观测该写法后才得到拒绝。未取得最初故障会话的原始日志，不能断言历史事件一定具有同一原因。本机用户设置含 Orca `PreToolUse`、`PermissionRequest` hooks，两轮均保留；未更改或绕过 hooks，未穷尽 managed settings。结论限定于此版本、此目录及当前有效设置。

## 方案

建议固定 cwd 并优先使用单条 `git diff --no-ext-diff --no-textconv origin/main`；确实需要 `git -C` 时，仅批准当前 worktree 的完整命令。以下是本次第三轮实际验证的 **完整 `--allowedTools` 值**，用精确命令替代任意参数前缀，未改仓库 env：

```text
Read Grep Glob Bash(git diff origin/main) Bash(git diff origin/main...HEAD) Bash(git diff origin/main --stat) Bash(git -C /home/laoxu/orca/workspaces/orca-factory/research-2 diff origin/main) Bash(git diff --no-ext-diff --no-textconv origin/main) Bash(git status --short) Bash(git log --oneline -3 origin/main)
```

| 条目 | 只读用途与边界 |
| --- | --- |
| `Read`、`Grep`、`Glob` | 读取文件、搜索正文和列出匹配路径，不授予 Write、Edit 或网络工具 |
| `Bash(git diff origin/main)` | 读取当前 worktree（包括已暂存）相对基线的补丁；精确参数，不预批准追加输出文件选项 |
| `Bash(git diff origin/main...HEAD)` | 读取已提交分支相对共同祖先的补丁；不能代替未提交内容检查 |
| `Bash(git diff origin/main --stat)` | 只显示变更统计；不能充当完整 diff 的证据 |
| `Bash(git -C /home/laoxu/orca/workspaces/orca-factory/research-2 diff origin/main)` | 补上实际被拒写法，仅指定当前目录、diff 和固定基线，不批准任意目录或任意 git 子命令 |
| `Bash(git diff --no-ext-diff --no-textconv origin/main)` | 读取完整 diff，并显式禁用外部 diff 和 textconv，适合作为规范读取命令 |
| `Bash(git status --short)` | 读取短状态，定位未跟踪文件；未跟踪正文仍需 Read |
| `Bash(git log --oneline -3 origin/main)` | 读取固定基线最近三条提交摘要，不修改引用或工作树 |

没有 `Bash` 全工具授权、`Bash(git:*)`、写文件工具、git 写操作或 curl；也不新增整个 `head:*`、`sed:*` 或 shell 解释器授权。这里的“只读”指新增授权的业务操作，不是操作系统级沙箱保证；原有 hooks、设置及 Git 仓库配置仍须可信。特别是泛化 `git diff:*` 会涵盖可写输出文件等额外选项，所以不推荐仅为了兼容写法扩大到任意参数。[Git 官方 diff 文档](https://git-scm.com/docs/git-diff)列出 `--output`、`--no-ext-diff`、`--no-textconv` 的行为。白名单是额外预批准，不会撤销运行环境已有权限。

上面的路径是本次实际值；移植到其他 worktree 时必须生成该目录的精确条目，不能原样复制旧路径或替换成 `git -C *`。若统一为固定 cwd，便不需要该条兼容写法。此报告仅提出方案，不落地修改配置。

## 验证

保持第二轮相同 HEAD、同一 `prompt-probes.md` 和同一模型、`dontAsk` 模式，第三轮实际运行：

```sh
claude -p --model claude-opus-5-5 --permission-mode dontAsk \
  --allowedTools 'Read Grep Glob Bash(git diff origin/main) Bash(git diff origin/main...HEAD) Bash(git diff origin/main --stat) Bash(git -C /home/laoxu/orca/workspaces/orca-factory/research-2 diff origin/main) Bash(git diff --no-ext-diff --no-textconv origin/main) Bash(git status --short) Bash(git log --oneline -3 origin/main)' \
  --output-format stream-json --verbose \
  < .omc/tmp/reviewer-permissions/prompt-probes.md \
  > .omc/tmp/reviewer-permissions/adjusted.jsonl \
  2> .omc/tmp/reviewer-permissions/adjusted.stderr
```

`--output-format stream-json --verbose` 只用于保存调用与结果证据。session 为 `40969b13-c89c-419b-a37c-22e9dfcd673a`，进程退出 **0**；恰好六条 Bash 调用，与第二轮六条探针逐条相同，均 `is_error: false`。结果事件为 `subtype: success`、`terminal_reason: completed`、`permission_denials: []`。

修复目标调用的实际结果：

```text
tool_use.id: toolu_01SzXQ4FLVafCqJ2rgw6jYTY
input.command: git -C /home/laoxu/orca/workspaces/orca-factory/research-2 diff origin/main
tool_result.tool_use_id: toolu_01SzXQ4FLVafCqJ2rgw6jYTY
is_error: false
diff --git a/docs/research/reviewer-permissions.md b/docs/research/reviewer-permissions.md
new file mode 100644
index 0000000..2ed2ad4
--- /dev/null
+++ b/docs/research/reviewer-permissions.md
@@ -0,0 +1,3 @@
+# 审查员权限调研
+
+复现标记：REVIEW_DIFF_PROBE_821
```

规范读取命令 `git diff --no-ext-diff --no-textconv origin/main` 的调用 ID 为 `toolu_013agcgzayndg5Y7mT6pA6sB`，也返回上面完整补丁，没有依赖 `head` 截断；统计为 `1 file changed, 3 insertions(+)`，三个新增行均在工具输出中。审查员复述了标题和完整标记，最后 VERDICT 为 green。它自行填的 lane 不是项目 gate 的结论，不能据此判定本报告 lane。

验证对象是受控三行探针，而不是写成后的这篇报告；这足以证明调整后的权限读到了该次全部 diff，不代表审核了任意大小补丁或完成安全穷举。没有运行写操作或网络命令来试探权限，也没有用外部工具替审查员绕过被拒调用。首轮自然审查、第二轮原配置拒绝、第三轮调整后成功三者必须一起解释，不能只引用最终 green。

最终产物仅此报告，探针内容已被报告取代，没有删除路径。报告 CHECK、项目 selftest 和 lane 1 gate 的结果以本次交接附带的实际输出为准。
