## 现状
工坊沙盘（`/ziikoo-driver` 生产页面）在 driver 正常空闲时长期显示「快照过期」并起雾。原因：`.claude/skills/ziikoo-driver/scene/page/diorama/model.mjs` 的 `staleOf` 用固定阈值 `STALE_MS = 40_000` 比较快照年龄，而 driver 空闲时只在巡检时刷新快照，巡检间隔为 2–3 分钟。实测：状态条同时显示「快照 1 分钟前」与「下次巡检约 1 分钟后」，driver ok，却被判为过期。`Scene.nextScanAt`（毫秒时间戳，可缺）已由 OF-442 透传到页面。

## 验收
断言: driver 状态为 ok 且带 `nextScanAt` 时，当前时刻（`scene.generatedAt + ageMs`）未超过 `nextScanAt + 40 秒` 就不算过期，超过才算；没有 `nextScanAt` 时保持现有规则（年龄超过 40 秒算过期）；`state` 不是 ok 或为 unknown 的现有规则不变。

## LANE 预判
2（依据：命中 `$AGENT_LANE2_GLOBS` 的 `.claude/*`）

## 依赖
无阻塞。

## intake 自检
- CHECK 可执行且会红：是，在 main 上实际运行
- CHECK 红因：断言失败，`staleOf` 对年龄 90 秒、下次巡检在 120 秒处的快照返回 true，node 退出码 1
- SCOPE 路径存在：是
- 外部契约样本：不解析新的外部输出
- 行为追踪：过期判定 → .claude/skills/ziikoo-driver/scene/page/diorama/model.mjs（staleOf、skyOf）；调用方 → .claude/skills/ziikoo-driver/scene/page/diorama/index.mjs
- lane 预判与 glob 一致：是
- project / Todo / ready-for-agent：是
- 依赖：无
- 拆票形状：接口票 无；依赖深度 2；最宽一层 1 张（本票为第 1 层，「沙盘地面感」票为第 2 层）

## 要做
1. 在 `.claude/skills/ziikoo-driver/test/diorama.browser.cjs` 的模型断言里加一条：带 `nextScanAt` 的 ok 快照在宽限内不过期、超过宽限过期、没有 `nextScanAt` 时沿用 40 秒规则。先写断言看它失败。
2. 修改 `staleOf(scene, ageMs)`，签名不变：`scene.nextScanAt` 存在时按 `scene.generatedAt + ageMs > scene.nextScanAt + STALE_MS` 判定；不存在时沿用 `ageMs > STALE_MS`。`skyOf` 不需要改规则。
3. `.claude/skills/ziikoo-driver/README.md` 中关于过期/起雾的说明同步为新规则。

不做：不改 driver、不改 `hooks/`、不改天空与海面颜色、不改 HUD 文案。

## 验收
CHECK: node -e "import('./.claude/skills/ziikoo-driver/scene/page/diorama/model.mjs').then(m=>{const g=1e12;const ok=!m.staleOf({state:'ok',generatedAt:g,nextScanAt:g+120000},90000)&&m.staleOf({state:'ok',generatedAt:g,nextScanAt:g+120000},170000)&&m.staleOf({state:'ok',generatedAt:g},41000);process.exit(ok?0:1)})" && claude plugin test .claude/skills/ziikoo-driver && node .claude/skills/ziikoo-driver/test/diorama.browser.cjs

## SCOPE
.claude/skills/ziikoo-driver/* driver/testdata/recorded/driver-status-json-next-scan.json

## 依赖
DEPENDENCIES: {"blockers":[],"related":[]}
