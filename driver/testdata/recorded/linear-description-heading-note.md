## 现状

看板与驱动器都没有「每票用量」的统一数据结构；ledger 行只有 minutes、stage_secs 等字段，没有 token 与成本。看板「总览」的 token 读取路径写死为 macOS 路径，在 Linux 上为空。

## 要做（接口票：只定义结构与兼容读取，不做采集）

1. 新建 `driver/internal/usage`：类型 `TicketUsage`（票号；按角色 worker、reviewer、coordinator、session 与按模型的分项；input、output、cache_read、cache_write、reasoning token；按 API 价折算的 `cost_usd`；`price_version`；`apportioned` 标记表示含分摊的 coordinator 用量），以及函数签名 `Collect(ctx, ticket, worktrees) (TicketUsage, error)` 的桩实现（返回零值）。
2. `driver/internal/ledger`：行解析接受可选的 `usage` 对象（上述字段）；旧行没有该字段时正常读取；字段类型错误时报 `invalid usage field`。stats 输出格式不变。
3. `mods/swarm-board/types/index.d.ts`：增加与 Go 一致的 `TicketUsage` 类型。
4. 不改插件版本号。
5. 测试 `TestUsageFields`：带 usage 的行能解析、旧行兼容、类型错误报错。

## 验收

CHECK: cd driver && go vet ./... && out=$(go test -count=1 -v -run TestUsageFields ./internal/ledger/... ./internal/usage/...) && grep -q -- '--- PASS: TestUsageFields' <<<"$out" && grep -q 'TicketUsage' ../mods/swarm-board/types/index.d.ts && test -z "$(git -C .. diff origin/main -- mods/swarm-board/.claude-plugin/plugin.json)"
断言: 每票用量结构在 Go 与看板两侧定义一致；ledger 兼容新旧行；版本号未动。

## LANE 预判

2（依据：命中 `driver/*`、`mods/*`）。合并：审查员 green 后自动合并。

## SCOPE

driver/internal/usage/\* driver/internal/ledger/\* mods/swarm-board/types/index.d.ts

## 依赖

无（ZII-884、ZII-885 已合并）。

## intake 自检

* CHECK 可执行且会红：是。main 上不存在 usage 包与 TestUsageFields。
* SCOPE 路径存在：是（usage 新建于已存在的 driver/internal/ 下）。
* lane 预判与 glob 一致：是。
* project / Todo / ready-for-agent：是。
* 拆票形状：本票为接口票；下游 3 张，依赖深度 3，最宽一层 2 张。
* estimate=2 → 预计 tier=fast
