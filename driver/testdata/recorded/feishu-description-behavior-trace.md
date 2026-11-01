## 现状
审批摘要在 `functions/mcp/ziikoo-mcp.js:229-235` 经过 `sanitizeMcpContent` 和 `redactVendorFields`，`keyHash` 变成 `[REDACTED]`，`provider` 被删除，`model` 被改写；审批卡片 `web/src/pages/McpApprovals.tsx:38-50` 只显示 operationId 和摘要。执行路径在 `functions/mcp/ziikoo-mcp.js:247-253` 先 `consumeApproval` 再调用 v1，执行失败也会消耗批准；非 UUID 的 approvalId 会触发数据库 uuid 错误。

## 验收
断言: 不同 keyHash 的审批摘要可区分，执行失败不消耗 approvalId，非法 approvalId 返回 404/400。

## LANE 预判
2（处置 Agent 补齐：要做第 4 条必须改 `functions/api/v1/_mcp-account-routes.js`，命中 `$AGENT_LANE2_GLOBS` 的 `functions/api/v1/_*-routes.js`；原预判 1 与原 SCOPE 漏列该文件及 `functions/mcp/oauth-mcp-integration.test.js`）

## 依赖
无阻塞。关联 ZY-315、ZY-317、ZY-319；canonical ID 已回读。

## intake 自检
- CHECK 可执行且会红：是，2026-11-01 在 main（de0df26d）从仓库根运行，rc=1。
- CHECK 红因：断言失败，输出 `ASSERT: approval summary or lifecycle still collapses targets or burns approvals`。
- SCOPE 路径存在：是，全部为已存在文件；新迁移写到 `postgres-migrations/` 下。
- 外部契约样本：不涉及，未新增外部字段解析。
- 行为追踪：摘要 → `functions/mcp/ziikoo-mcp.js:229-235`；审批卡片 → `web/src/pages/McpApprovals.tsx:38-50`；消费时序 → `functions/mcp/ziikoo-mcp.js:247-253`；审批存储 → `functions/mcp/_approvals.js:219-238`；确认接口 → `functions/api/v1/_mcp-account-routes.js:46-74`。
- lane 预判与 glob 一致：是。
- project / Todo / ready-for-agent：是（建票时设置）。
- 依赖：无阻塞；related canonical ID 已回读且 complete=true。
- 拆票形状：接口票 无，原因：6 张票是相互独立的缺陷修复，没有共享类型或接口；依赖深度 1；最宽一层 6 张。

## 要做
1. 新增审批摘要构造函数，保留目标 ID（如 keyHash、teamId、deptId）和 method/path，只对真正的秘密值做稳定指纹或短掩码；摘要构造不删除、不改写供应商字段；非运营者的响应仍受 `functions/api/v1/index.js` 的租户边界脱敏（处置 Agent 补齐，放宽该边界须另开票由负责人决定）。审批卡片显示 method/path 和可辨识目标。
2. 在 v1 参数校验与目标鉴权成功后再消费审批；执行前失败允许用同一 approvalId 重试，执行成功后仍只消费一次。
3. 对 approvalId 做 UUID 格式校验，非法值返回 404/400 而不是 500。
4. 给 approve/reject 路由加 Origin/Sec-Fetch-Site 同源检查（保留 SameSite=Lax 作为纵深）。
5. 增加摘要可区分、失败不消费、非法 ID、跨源拒绝的测试。

## 验收
CHECK: grep -qs 'keeps distinct approval targets visible to the approver' functions/mcp/ziikoo-mcp.test.js && grep -qs 'does not consume approval before a successful execution' functions/mcp/_approvals.pg.test.js || { echo 'ASSERT: approval summary or lifecycle still collapses targets or burns approvals'; exit 1; }; cd web && npx vitest run --project unit ../functions/mcp/ziikoo-mcp.test.js && npx vitest run --project pg ../functions/mcp/_approvals.pg.test.js

## SCOPE
functions/mcp/ziikoo-mcp.js functions/mcp/ziikoo-mcp.test.js functions/mcp/_approvals.js functions/mcp/_approvals.pg.test.js web/src/pages/McpApprovals.tsx functions/api/v1/_mcp-account-routes.js functions/mcp/oauth-mcp-integration.test.js functions/api/v1/_engine.js functions/api/v1/_authz.js functions/mcp/rest-adapter.js

## 依赖
DEPENDENCIES: {"blockers":[],"related":["bb7f56f8-6108-4918-b1f9-3048fffa6601","76380212-3a81-425a-91a5-f911d29d7525","3f4316c7-441b-4860-9a5e-3ca3cebb1134"]}
