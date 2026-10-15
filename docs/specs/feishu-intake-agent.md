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
- **只读检出**：网关维护一个分离 HEAD 的 worktree，新会话开始前更新到 `origin/main`。Agent 的工作目录就是这个检出。

## 3. 话题与会话

1. 白名单成员在指定群中 @需求助手 发起一条消息，网关以“话题回复”回应，这个话题就是一次需求会话。
2. 话题 ID ↔ claude 会话 ID ↔ 需求人 open_id，保存在 `~/.local/state/orca-factory/intake/<话题ID>.json`。
3. 会话状态：`对话中` → `待确认` → `已提交` → `已关闭`。超过 7 天无消息自动关闭。
4. 同一话题串行处理；全局同时运行的 Agent 不超过 `AGENT_INTAKE_CONCURRENCY`（默认 2），其余排队，排队时回复“排队中”。
5. 网关以 message_id 去重，不以 event_id 去重（飞书文档说明 event_id 不适合作为去重键）。
6. 开通 `im:message.group_msg` 后话题内无需 @。此前仅开通 `im:message.group_at_msg:readonly` 时，不带 @ 或手打“@名字”的回复不投递（Q1 实测）；开通 `group_msg` 后群内全部消息都会投递，网关据此续接：该话题已有网关自己开的会话、会话未关闭、发送者在 `AGENT_FEISHU_INTAKE_USERS` 中，三者同时成立才续接，否则静默丢弃（不回复、不写去重记录、不写会话、不记录消息正文）。开新话题仍须 @需求助手。话题由事件中的 `thread_id` 识别，话题首条消息的 `root_id` 为空，后续回复的 `root_id` 指向首条消息。
7. 网关在每轮输入前加发言人标签：需求人固定为“[需求人]”，其他白名单成员按会话内首次发言的顺序标“[成员2]”“[成员3]”…；这份映射存入会话文件（不调用通讯录接口），重启后编号不变。Agent 据此分辨成员互相讨论与对自己的提问；回复（去首尾空白后）恰为固定标记 `<无需回复>` 时，网关不发消息，但仍更新会话最后消息时间。
8. 话题内消息文字（去首尾空白后）恰为“结束”或“取消”时，网关不调用 Agent，把会话置为 `已关闭` 并回复“已结束本话题的需求整理”；此后该话题内的消息按已关闭会话处理（不带 @ 的静默丢弃，带 @ 的提示重新发起）。
9. 会话轮数达到 `AGENT_INTAKE_MAX_TURNS`（默认 30）时，网关在同一 claude 会话内追加一轮 `[网关]` 前缀的摘要请求，要求 Agent 只输出 `intake-summary-v1` 代码块（字段：需求摘要、已定决策、未决问题、当前草稿，均非空）。回复带合格代码块时，网关轮换到一个新的 claude 会话 ID（话题 ID 不变，claude 会话 ID 换新、轮数清零），把摘要拼成下一轮首轮输入的前缀，该前缀只用一次；回复不带合格代码块时保留原会话 ID 和轮数，仅在 stderr 记录告警，话题继续用原会话续聊。

## 4. 权限与安全

| 项 | 规则 |
| --- | --- |
| 群 | 只响应 `AGENT_FEISHU_INTAKE_CHAT` 指定的群；其他群和私聊一律忽略 |
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

1. Agent 回复中出现 `intake-draft-v1` 时，网关解析并校验：必填字段齐全；SCOPE 每个 glob 在检出中至少匹配一个路径；依赖 ID 在账本中存在。校验失败时，把失败原因作为下一轮输入交还 Agent。
2. 校验通过后，网关按 `AGENT_LANE2_GLOBS` / `AGENT_LANE3_GLOBS` 计算 lane 预判（不采信 Agent 给出的 lane），在话题中发出票面摘要和“回复‘确认提交’以提交”。会话进入 `待确认`。
3. 需求人（且仅需求人）回复“确认提交”后，网关调用 `issues create`：已纳管、状态 Todo、不打 `ready-for-agent`；正文追加“提出人：<姓名>（飞书话题链接）”。回复票号与记录链接，并在群里 @审批人。
4. 审批人在群里发“@需求助手 批准 OF-N”：网关核对发送者在审批人名单中、该票由需求助手提交、未打 `ready-for-agent`、lane 预判不是 3，然后 `issues update --add-label ready-for-agent`，写一条“批准：<审批人> <时间>”事件，回复已放行。lane 3 回复“lane 3 需人工实现，不放行”。
5. “驳回 OF-N <理由>”把票改为 Canceled 并在原话题告知需求人。

## 7. 配置

| 变量 | 位置 | 说明 |
| --- | --- | --- |
| `AGENT_FEISHU_INTAKE_CHAT` | `driver.env` | 需求群 chat_id |
| `AGENT_FEISHU_INTAKE_USERS` | `driver.env` | 可对话成员 open_id，逗号分隔 |
| `AGENT_FEISHU_APPROVERS` | `driver.env` | 审批人 open_id，逗号分隔 |
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
