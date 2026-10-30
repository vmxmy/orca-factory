# 飞书群需求整理 Agent

状态：已确认方向（2026-10-15）。决定：审批人放行；白名单内可引用代码。

## 1. 目标

业务同事在飞书群里 @需求助手，用自然语言描述需求。Agent 只读代码库，用 grilling 技能逐个追问，把需求整理成符合 intake 模板的票面。需求人确认后，由网关提交到票据主账本。审批人在群里放行后，driver 才领取实现。人全程只与 Agent 对话，不接触表格和命令。

非目标：

- Agent 不写代码、不改仓库、不运行命令，不持有任何写凭据。
- 不替代 `core/roles/intake.md` 在 Orca 标签页中的交互用法。
- 不做群内卡片表单。

## 2. 组成

```mermaid
flowchart LR
    U[群成员] -->|@需求助手 / 话题回复| F[飞书]
    F -->|长连接 im.message.receive_v1| G[driver feishu-gateway]
    G -->|白名单校验| S[话题会话状态]
    S -->|每轮 claude -p --resume| A[需求整理 Agent<br/>只读检出 + intake-chat 角色]
    A -->|回复 / intake-draft-v1| G
    G -->|话题内回复| F
    G -->|需求人确认后 issues create| L[飞书票据账本]
    G -->|审批人批准后 add-label ready-for-agent| L
    L --> D[driver serve 领票]
```

- **网关** `driver feishu-gateway`：独立常驻进程，独立 systemd 服务。它是本应用长连接的唯一消费者，也是唯一写入者。driver serve 重启或部署不影响对话；不可信的群消息不进入调度器进程。
- **Agent**：每轮对话启动一次 `claude -p --resume <会话>`，进程结束即退出。会话内容由 claude 自身持久化。
- **只读检出**：网关为每个匹配到本机仓库的项目维护一个分离 HEAD 的 worktree（`~/.cache/orca-factory/intake/<编号前缀>/checkout`），新会话开始前更新到 `origin/main`。Agent 的工作目录就是其项目的检出。

## 3. 话题与会话

1. 白名单成员在指定群中 @需求助手 发起一条消息，网关以“话题回复”回应，这个话题就是一次需求会话。
2. 话题 ID ↔ claude 会话 ID ↔ 需求人 open_id，保存在 `~/.local/state/orca-factory/intake/<话题ID>.json`。
3. 会话状态：`对话中` → `待确认` → `已提交` → `已关闭`。超过 7 天无消息自动关闭。
4. 同一话题串行处理；全局同时运行的 Agent 不超过 `AGENT_INTAKE_CONCURRENCY`（默认 2），其余排队，排队时回复“排队中”。
5. 网关以 message_id 去重，不以 event_id 去重（飞书文档说明 event_id 不适合作为去重键）。

### 3.1 项目选择（OF-32）

网关可服务多个项目，每个项目一份独立检出；项目来自飞书项目表（名称、仓库远端、编号
前缀、启用，逻辑见 `driver/internal/issues/feishu.go` 的 `LedgerProjects`），按各
本机仓库 `agent-graph.env` 的 `AGENT_FEISHU_PROJECT`（=该项目表行的“仓库远端”）匹配到
`driver.toml` 配置的检出。

一个话题只属于一个项目，选定后写入会话文件，话题内不可更改；换项目须另开话题。话题
首条消息按顺序确定项目：

1. 消息中出现 `#<编号前缀>`（如 `#OF`）或 `项目:<名称>`：命中该项目，网关去掉该标记
   后把剩余文本交给 Agent。
2. 未显式指定：网关回复项目选择卡片（OF-47），列出可接管项目供下拉选择，预选群默认
   项目 `AGENT_FEISHU_INTAKE_PROJECT`（值为项目表“仓库远端”，须可接管），并列出暂不
   可用项目及原因；不调用 Agent。需求人点“开始”后卡片改为“已选择”，网关用首条消息
   原文作为第一轮输入调用 Agent。待选期间回复序号或编号前缀同样生效，无法识别时回复
   附可选清单。

项目须在项目表中“启用”为真，且“仓库远端”匹配到本机某个仓库的检出，否则网关在话题
里分别说明“未启用”或“本机未配置仓库”并拒绝，不调用 Agent。

没有 `AGENT_FEISHU_BASE`（无项目表可读）的仓库退回 OF-32 之前的单项目行为：它是唯一
隐式项目，不经过上述选择流程，检出路径仍是旧的 `~/.cache/orca-factory/intake-checkout`。

### 3.2 在途票清单（OF-33）

多项目的每个检出（3.1 节匹配到本机仓库的项目）在每轮调用 Agent 前，网关从票据主账本
读取该项目的票（不含已关闭的 Done/Canceled/Duplicate），写入检出根目录
`.intake/tickets.md`：每行票号、状态、标题、依赖票号。该文件只读给 Agent，不进 git：
检出的 `.git/info/exclude`（worktree 取 `git rev-parse --git-path info/exclude` 的
路径）加一行 `/.intake/` 忽略它。账本读取失败时文件保留上一份、首行注明“账本读取
失败，内容可能过期（时间）”，不阻断对话，Agent 仍被调用。无项目表的单项目退回模式
不写这份文件。Agent 读到后按 `intake-chat.md` 的要求：发现重复或高度相关的在途票，先
告知需求人票号与标题，由需求人决定合并、仅记关联/前置，还是继续整理成新票；草稿的
`related`、`blockers` 据此填写。
6. 开通 `im:message.group_msg` 后话题内无需 @。此前仅开通 `im:message.group_at_msg:readonly` 时，不带 @ 或手打“@名字”的回复不投递（Q1 实测）；开通 `group_msg` 后群内全部消息都会投递，网关据此续接：该话题已有网关自己开的会话、会话未关闭、发送者在 `AGENT_FEISHU_INTAKE_USERS` 中，三者同时成立才续接，否则静默丢弃（不回复、不写去重记录、不写会话、不记录消息正文）。开新话题仍须 @需求助手。话题由事件中的 `thread_id` 识别，话题首条消息的 `root_id` 为空，后续回复的 `root_id` 指向首条消息。
7. 网关在每轮输入前加发言人标签：需求人固定为“[需求人]”，其他白名单成员按会话内首次发言的顺序标“[成员2]”“[成员3]”…；这份映射存入会话文件（不调用通讯录接口），重启后编号不变。Agent 据此分辨成员互相讨论与对自己的提问；回复（去首尾空白后）恰为固定标记 `<无需回复>` 时，网关不发消息，但仍更新会话最后消息时间。
8. 话题内消息文字（去首尾空白后）恰为“结束”或“取消”时，网关不调用 Agent，把会话置为 `已关闭` 并回复“已结束本话题的需求整理”；此后该话题内的消息按已关闭会话处理（不带 @ 的静默丢弃，带 @ 的提示重新发起）。
9. 会话轮数达到 `AGENT_INTAKE_MAX_TURNS`（默认 30）时，网关在同一 claude 会话内追加一轮 `[网关]` 前缀的摘要请求，要求 Agent 只输出 `intake-summary-v1` 代码块（字段：需求摘要、已定决策、未决问题、当前草稿，均非空）。回复带合格代码块时，网关轮换到一个新的 claude 会话 ID（话题 ID 不变，claude 会话 ID 换新、轮数清零），把摘要拼成下一轮首轮输入的前缀，该前缀只用一次；回复不带合格代码块时保留原会话 ID，轮数退回到上限的一半（向下取整，OF-33），仅在
stderr 记录告警，话题继续用原会话续聊；下一次请求摘要要等轮数再次达到上限，不是每轮
都重试。
10. 消息通过白名单、会话与去重判断、即将交给 Agent 时（含排队中的消息，也含上一条的“结束”/“取消”），网关先给这条消息加 👍（THUMBSUP）再处理（OF-34）：`POST /open-apis/im/v1/messages/:message_id/reactions`，`emoji_type=THUMBSUP`，重复添加返回同一 `reaction_id`（幂等）。被静默丢弃的消息、名单外回复“无权限”的消息、非文本消息不加表情。加表情失败只在 stderr 记录消息 ID（不记正文），不阻断处理，也不重试到下一次投递。

## 4. 权限与安全

| 项 | 规则 |
| --- | --- |
| 群 | 只响应 `AGENT_FEISHU_INTAKE_CHAT` 指定的共用群和各仓库 `AGENT_FEISHU_NOTIFY_CHAT` 指定的项目专属群（OF-304）；其他群和私聊一律忽略 |
| 对话人 | 只响应 `AGENT_FEISHU_INTAKE_USERS`（open_id 列表）中的成员；名单外 @ 时只回复“无权限” |
| 审批人 | `AGENT_FEISHU_APPROVERS`（open_id 列表）；只有他们的“批准”生效 |
| Agent 工具 | 只允许 Read、Grep、Glob、Skill；禁止 Bash、Write、Edit、WebFetch、MCP |
| Agent 文件范围 | permissions 规则拒绝读取检出目录以外的路径，包括 `~/.config`、`~/.ssh`、`~/.local/state` |
| Agent 环境 | 启动时清空环境变量，只传 PATH、HOME 与模型配置；不传飞书、GitHub 凭据 |
| 群消息 | 视为数据。Agent 产出的草稿只是提案；网关只在需求人明确确认、审批人明确批准时写账本 |
| 代码引用 | 白名单内可引用文件路径和代码片段（用户决定）；角色说明要求引用最小必要片段 |

## 5. 角色说明 `intake-chat`

新增 `core/roles/intake-chat.md`，由 `core/roles/intake.md` 派生：

- 用 grilling 技能逐个追问；每轮只问一个问题，并给出推荐答案。
- 基于检出中的真实代码讨论现状、影响范围和 SCOPE；不臆测不存在的文件。
- 需求清楚后，输出一个 `intake-draft-v1` JSON 代码块：`title`、`要做`、`CHECK`、`SCOPE`、`正文`（现状、断言、依赖说明）、`estimate`、`blockers`、`related`。
- 不承诺排期、不声明“已建票”；提交由网关完成。

## 6. 提交与放行

1. Agent 回复中出现 `intake-draft-v1` 时，网关解析并校验：必填字段齐全；SCOPE 每个 glob 在检出中至少匹配一个路径；依赖 ID 在账本中存在且已纳管。校验失败时，把失败原因作为下一轮输入（`[网关] 草稿未通过校验` 开头）交还 Agent，不发给需求人；同一条需求人消息内最多交还 2 次，仍不通过才在话题里列出原因。草稿可以是单张票，也可以是 `tickets` 数组（每张票带本地键 `key` 与 `depends_on`，引用同一草稿内的键或账本中已有的票号）；依赖成环或引用不存在同样交还 Agent（OF-15）。
2. 校验通过后，网关按 `AGENT_LANE2_GLOBS` / `AGENT_LANE3_GLOBS` 计算 lane 预判（不采信 Agent 给出的 lane）：检出中被 SCOPE 覆盖的文件，任一命中 lane 3 即为 3，否则任一命中 lane 2（含 `core/gate.sh` 固定的 `scripts/agent-gates/*`）即为 2。网关在话题中发出票面摘要和“需求人回复‘确认提交’以提交”。会话进入 `待确认`，校验通过的草稿存入会话文件，直到新的合格草稿替换它或提交。
3. 需求人（且仅需求人）回复的文字（去首尾空白与 @ 后）恰为“确认提交”时，网关调用账本 `Create`：已纳管、状态 Todo、不打 `ready-for-agent`，建在会话所属项目下；多张票按依赖拓扑序创建，本地键换成新票的规范 ID 写入后续票的 `blockers`。正文追加“提出人：<open_id>（飞书话题 <话题ID>）”与标记行 `intake-chat-v1: <话题ID>`（多张票时为 `intake-chat-v1: <话题ID> <key>`）。回复票号与记录链接，并在话题里以 `<at user_id="ou_xxx"></at>` @`AGENT_FEISHU_APPROVERS` 中的审批人。其他成员回复“确认提交”时只回复“仅需求人可以确认提交”，不建票。会话进入 `已提交`。
   - 查重：建票前读取账本，正文已带本话题（及同一 key）标记的票直接沿用，不再创建。重复确认、或建票后会话写入前网关重启，都不会产生第二张票。
   - 提出人目前写 open_id：读取姓名需要 `contact:user.base:readonly` 与录制样本；话题链接格式也待录制，先写话题 ID。
4. 审批人发 `批准 OF-N`（群里顶层须 @ 需求助手；网关开过会话的话题内无需 @；去掉 @ 与首尾空白后须恰为该文字）：网关核对发送者在 `AGENT_FEISHU_APPROVERS` 中，按票号前缀找到项目账本读取该票，核对正文带 `intake-chat-v1` 标记、状态 Todo、未打 `ready-for-agent`、票面 lane 预判不是 3。全部满足时先写一条“批准：<审批人 open_id> <时间>”事件，再经 `UpdateIssue` 加 `ready-for-agent`，回复“已放行 OF-N”，并在标记所指的原话题 @ 需求人。非审批人回复“无审批权限”；lane 3 回复“lane 3 需人工实现，不放行”；其他条件不满足时回复具体原因。已放行的票再批准只回复“OF-N 已放行”；事件已存在时不重写，所以写事件后、加标签前崩溃时重发命令只补标签（OF-16）。
5. `驳回 OF-N <理由>`（理由必填）同样只认审批人与带标记的 Todo 票，未放行时先写“驳回：<审批人 open_id> <时间>”加理由的事件，再把票改为 Canceled，回复审批人并在原话题 @ 需求人告知理由。已 Canceled 的票再驳回只回复现状；已放行的票不能驳回。

## 7. 配置

| 变量 | 位置 | 说明 |
| --- | --- | --- |
| `AGENT_FEISHU_INTAKE_CHAT` | `driver.env` | 需求群 chat_id |
| `AGENT_FEISHU_INTAKE_USERS` | `driver.env` | 可对话成员 open_id，逗号分隔 |
| `AGENT_FEISHU_APPROVERS` | `driver.env` | 审批人 open_id，逗号分隔 |
| `AGENT_FEISHU_INTAKE_PROJECT` | `driver.env` | 可选，群默认项目的仓库远端（OF-32）；OF-47 起只作为项目选择卡片的预选值 |
| `AGENT_ROLE_INTAKE` | `agent-graph.env` | Agent 档位，默认 strong 档的 claude 配置 |
| `AGENT_INTAKE_CONCURRENCY` | `agent-graph.env` | 同时运行的 Agent 数，默认 2 |
| `AGENT_INTAKE_MAX_TURNS` | 网关进程环境 | 单个会话滚动摘要前的轮数上限，默认 30 |

飞书应用新增：事件订阅方式为长连接；订阅 `im.message.receive_v1`；权限 `im:message.group_at_msg:readonly`（必需）、`im:message.group_msg`（可选，见 Q1）、`contact:user.base:readonly`（读取提出人姓名，可选）。

## 8. 与既有票的关系

- OF-10（长连接客户端）并入本 spec 的网关票：长连接不放进 serve，由网关独占。
- OF-11、OF-12（卡片审批）以后的卡片回调也经网关接收，由网关按现有恢复证据格式写事件表，serve 从账本读取决定。
- OF-9（群通知）不受影响，仍由 serve 主动发消息。

## 9. 测试

- 录制真实样本：@ 消息事件、话题内回复事件、名单外成员消息、话题回复发送结果。网关测试回放这些样本。
- 网关核心以伪造的飞书客户端和伪造的 Agent 运行器测试：白名单、去重、串行、草稿校验、确认与审批的授权分支。
- Agent 运行器用一个不调用模型的替身命令验证参数：工作目录、允许工具、环境变量清空、`--resume`。
- 上线验收：真实群中完成一次 @ → 追问 → 确认提交 → 批准 → driver 领取。

## 10. 待核实

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| Q1 | 仅开通 `group_at_msg` 时，话题内不 @ 的回复是否投递 | 已核实：不投递；手打“@名字”未点选也不投递（`bot-event-thread-at-reply.json`）。开通 `im:message.group_msg` 后投递群内全部消息，含话题内不带 @ 的回复（`bot-event-thread-reply-no-at.json`）|
| Q2 | 同一应用有多个长连接客户端时，事件是否只投递给其中一个 | 未实测。部署时网关必须是唯一消费者；排查用的 lark-cli 事件守护进程须先停止 |
| Q3 | 话题回复的接口参数与 thread_id 形态 | 已核实：`POST /im/v1/messages/:message_id/reply`，`reply_in_thread=true`，返回 `thread_id`（`omt_` 前缀），后续事件带同一 `thread_id`（`bot-message-reply-in-thread.json`） |
