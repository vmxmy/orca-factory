# 飞书多维表格录制样本

2026-10-15 在测试 Base 上用 `lark-cli 1.0.97` 的 `api` 命令录制，用户身份，接口为 `base/v3` 与 `drive/v1`。`bot-*` 为应用身份录制，其余为用户身份。用途与结论见 [飞书多维表格票据账本](../../../../docs/specs/feishu-bitable-ledger.md) 第 2.1 节。

每个文件是 `{request, response}`：`request` 是方法、路径、查询参数和请求体；`response` 是 lark-cli 的输出，`data` 为接口的 `data` 对象，外层 `code`/`msg` 由 CLI 去掉，出错时保留在 `error` 中。

已脱敏：base token 替换为 `BASE_TOKEN`，用户 open_id 为 `ou_REDACTED`，应用 ID 为 `cli_REDACTED`，用户名为“测试用户”，租户域名为 `TENANT`。记录 ID、字段 ID 保持原样。

`bot-message-*` 于 2026-10-15 以应用身份录制，接口为 `im/v1/messages`（群通知，OF-9）：发送成功、群 ID 无效（`230001`）、机器人不在群内（`230002`）。缺少 `im:message:send_as_bot` 权限时返回与 `bot-scope-denied.json` 相同形态的 `99991672`。群 ID 脱敏为 `oc_REDACTED`，`tenant_key` 为 `TENANT_KEY`。

`bot-event-*` 于 2026-10-15 用官方 Go SDK（`oapi-sdk-go/v3` v3.12.1）长连接录制，是 `im.message.receive_v1` 的原始事件体（schema 2.0），保存在 `event` 键下，未经 lark-cli 加工：群内新消息 @机器人（`bot-event-message-at.json`）、话题内 @机器人的回复（`bot-event-thread-at-reply.json`，带 `root_id`、`parent_id`、`thread_id`）。仅开通 `im:message.group_at_msg:readonly` 时，话题内不带 @ 的回复不投递；手打 “@名字” 而未点选成员不算提及，同样不投递。`bot-message-reply-in-thread.json` 为 `reply_in_thread=true` 的话题回复，返回 `thread_id`。发送者 open_id 脱敏为 `ou_REDACTED`，机器人 open_id 为 `ou_BOT_REDACTED`，union_id 为 `on_REDACTED` / `on_BOT_REDACTED`。
