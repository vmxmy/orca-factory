# Linear 读取策略核查

核查日期：2026-10-06。结论：保留 Linear 也能通过批量、分流增量显著减少请求；迁移 GitHub 是已批准的独立选择，不能自动消除 N+1。本次仅研究，不调用 Linear API、不读取凭据、不启用同步或 webhook。任务提供的既有导出为 **123 issues、1266 comments、21 requests**，不是本次重测结果。

## 已核实：现有读取与官方约束

本地 `HEAD = origin/main = 8db2d1d`：[Runtime 第127行](https://github.com/vmxmy/orca-factory/blob/8db2d1dec0e26af1745a4437435476386f7ecdcb/driver/internal/scheduler/runtime.go#L127) 每轮传 `time.Time{}`；第153行逐票读评论，第165行才过滤 Done/Canceled，目的是保留 merge 归属和部署屏障。[客户端](https://github.com/vmxmy/orca-factory/blob/8db2d1dec0e26af1745a4437435476386f7ecdcb/driver/internal/linear/linear.go#L233) 已合并项目查询，但 issue 每页20、评论每票每页100，非零时间过滤还是严格 `gt`。全仓搜索表明 [`FullReconcile`](https://github.com/vmxmy/orca-factory/blob/8db2d1dec0e26af1745a4437435476386f7ecdcb/driver/internal/scheduler/adaptive.go#L57) 仅计算、赋值和测试，没有读取策略消费者。

官方使用漏桶持续补充额度；单查询复杂度上限 **10,000**，嵌套 connection 按分页大小乘算，因此合并请求不等于降低复杂度。应同时观察 `X-Complexity`、`X-RateLimit-Requests-*`、`X-RateLimit-Complexity-*` 和 `X-RateLimit-Endpoint-Requests-*`；Reset 为 epoch 毫秒。限流可能是 HTTP400、`errors[].extensions.code=RATELIMITED`，不能只处理429；建议按补充速率平滑发请求，遇限流结合响应头退避并加抖动。[限流文档](https://linear.app/developers/rate-limiting)

HTTP200 仍可能含部分失败；归档默认隐藏，导出/修复需明确 `includeArchived:true`。[GraphQL](https://linear.app/developers/graphql) 每条 connection 均须循 `hasNextPage/endCursor` 完整翻页，可按 `updatedAt` 排序；过滤支持 `gte` 和关系条件。[分页](https://linear.app/developers/pagination)、[过滤](https://linear.app/developers/filtering)

## 已核实：社区实际实现

| 一手来源 | 可借鉴实现与边界 |
| --- | --- |
| [Airbyte manifest](https://github.com/airbytehq/airbyte/blob/e39455951d08a0b9776389bee2052a54cb024c68/airbyte-integrations/connectors/source-linear/manifest.yaml) | `incremental_sync_updated_at` 注入 `filter.updatedAt.gte`；issues 每页25，顶层 comments 每页50、独立增量，均取归档。`issue_relations` 查询无 filter/增量配置；`issue_history` 用父 issue 分区逐票读取。 |
| [Airbyte 配套说明](https://github.com/airbytehq/airbyte/blob/master/docs/integrations/sources/linear.md#supported-sync-modes) | 每流独立游标，流成功完成才推进；失败重读，包含边界，目标端按主键去重。关系流缺可用增量过滤；history 的逐票成本不能当通用同步捷径。 |
| [Nango issues.ts](https://github.com/NangoHQ/integration-templates/blob/02b840bafa26f2af89953c21c376eb6c5ed460ea/integrations/linear/syncs/issues.ts) | 所读版本 `3.0.2`：`gte`、每页50、按更新时间；先 `batchSave` 再存分页 checkpoint，翻页完成后推进首屏高水位。该文件未展示删除处理，不能据此声称完整镜像。 |

以上只借鉴算法，不建议安装框架。社区源码链接使用协调者核实的最近文件提交；内容核查来自当日分支 raw。Airbyte 说明仍是分支链接，可能漂移；本仓证据已固定提交。

## 建议：无 webhook 的批量增量

以下是设计提案，**不是本次实现**；当前 GitHub-only 迁移复用既有导出，不新增 Linear 后台任务。

1. 若未来仍需读取 Linear：薄 issue 流与顶层 comment 流分开，服务端限定范围，按 issueId 本地归并；必要时用有界 alias 批次补明细，分别维护子分页，禁用巨型嵌套查询。
2. 每流持久化 `updatedAt >= watermark-overlap`，按实体 ID 去重并保留较新版本；整流成功落盘后才推进高水位。页 checkpoint 与已保存数据一起提交；部分 GraphQL 错误不算成功。重叠窗口可先设2分钟再测量，并非官方保证，仍须周期修复；不假定评论变化必然更新父 issue。
3. 永久删除无法靠更新时间补回（[Airbyte 删除限制](https://github.com/airbytehq/airbyte/blob/master/docs/integrations/sources/linear.md#deleted-records-arent-removed-from-your-destination)）；评论与关系采用周期完整集合比对。仅在范围、权限与分页均完整的成功快照后确认缺失，读取失败不能当删除；归档与删除分开。claim、取消、merge 前重新确认状态、归属评论及阻塞关系；未知写入结果先读回操作标识/回执，不能盲重试。缓存用于发现候选，不能替代这些正确性屏障。

## 将来可选：webhook + 周期修复

官方要求公网 HTTPS、非 localhost，5秒内 HTTP200；失败或任何非200 最多重试3次，间隔1分钟、1小时、6小时。原始 body 验 HMAC-SHA256，校验 `webhookTimestamp` 新鲜度（官方建议1分钟）；`Linear-Delivery` 是唯一 payload ID，`webhookId` 是配置 ID。事件含 create/update/remove；支持清单未列独立 IssueRelation，文档未承诺事件顺序。[官方 webhook](https://linear.app/developers/webhooks)

建议验证后持久化入队再应答，用 Delivery 去重；按实体版本拒绝旧更新，保留删除墓碑，冲突时重读。Issue/Comment 事件驱动缓存失效，关系及漏投靠周期修复；关键动作仍走上面的实时屏障。当前不部署接收端。

## 建议验收预算

| 场景 | 请求预算与正确性断言（未来实现时测试） |
| --- | --- |
| 123票冷读 | 原路径至少 `ceil(123/20)+123=130` 次，另加子分页；只算快照和评论，不含启动元数据。 |
| 同一导出夹具批读 | 固定夹具及字段下以既有21次为回归上限；不能把它当任意数据量保证。 |
| 无变化热读 | 两个独立增量流、重叠结果各不足一页、无修复/关键动作时 ≤2次；不能逐票扫评论。 |
| 有变化 | 预算 `max(1,ceil(ΔI/页长))+max(1,ceil(ΔC/页长))+补读+修复`，Δ包含重叠记录；额外记录复杂度。 |
| 正确性 | 同时间戳、多页并发修改、失败重跑、删除/归档、Done仍有merge归属、取消保留部署屏障、未知写入读回均不得丢失；关键动作请求单独计数，不能为达预算省略。 |

本次为文档变更；核对一手来源与本地代码，运行 `git diff --check`，不新增代码测试，无删除路径。
