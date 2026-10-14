# 飞书多维表格录制样本

2026-10-15 在测试 Base 上用 `lark-cli 1.0.97` 的 `api` 命令录制，用户身份，接口为 `base/v3` 与 `drive/v1`。用途与结论见 [飞书多维表格票据账本](../../../../docs/specs/feishu-bitable-ledger.md) 第 2.1 节。

每个文件是 `{request, response}`：`request` 是方法、路径、查询参数和请求体；`response` 是 lark-cli 的输出，`data` 为接口的 `data` 对象，外层 `code`/`msg` 由 CLI 去掉，出错时保留在 `error` 中。

已脱敏：base token 替换为 `BASE_TOKEN`，用户 open_id 为 `ou_REDACTED`，应用 ID 为 `cli_REDACTED`，用户名为“测试用户”，租户域名为 `TENANT`。记录 ID、字段 ID 保持原样。
