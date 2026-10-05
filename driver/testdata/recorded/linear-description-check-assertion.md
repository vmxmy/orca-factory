## 现状

驱动器知道每一次状态转移（领票、派工、gate、审查、开 PR、合并、暂停），但看板只能定时拉取。看板插件宿主的 `$.process.run` 要等命令结束才返回，难以持有 socket 流，所以推送改用「驱动器原子重写本地状态文件、看板监听文件」的方式，复用看板快照监听的同一机制。

## 要做

1. 驱动器在每次状态转移与每轮巡检结束时，把与 `driver status --json`（ZII-867）相同的内容原子写入 `${XDG_RUNTIME_DIR:-/tmp}/orca-factory/status.json`（先写临时文件再 rename），并带 `generatedAt`。这是可重建的实时缓存，不是事实来源。
2. 写入失败只记日志，不影响状态机推进。
3. 看板用快照监听机制监听这个文件：文件存在且 `generatedAt` 在 2 分钟内时优先用它显示在途票、归属、全局暂停与 Linear 消耗；否则退回 precheck 快照。
4. 不改插件版本号。
5. 测试：Go 侧 `TestStatusFilePush` 覆盖状态转移后文件内容更新、原子写（读者不会读到半个文件）、写失败不影响推进；看板侧导出纯函数 `pickStatusSource(status, snapshot, now)`，CHECK 用 bun 断言新鲜与过期两种选择。

## 验收

CHECK: (cd driver && go vet ./... && out=$(go test -count=1 -v -run TestStatusFilePush ./...) && grep -q -- '--- PASS: TestStatusFilePush' <<<"$out") && bun build mods/swarm-board/hooks/driver-status.ts --target=node >/dev/null && bun -e "const m=await import('./mods/swarm-board/hooks/driver-status.ts');const now=Date.parse('2026-10-05T12:00:00Z');if(m.pickStatusSource({generatedAt:(now-60000)/1000},{},now)!=='driver')process.exit(1);if(m.pickStatusSource({generatedAt:(now-300000)/1000},{},now)!=='snapshot')process.exit(1)" && test -z "$(git diff origin/main -- mods/swarm-board/.claude-plugin/plugin.json)"
断言: 驱动器在状态转移时原子更新状态文件；看板在文件新鲜时用驱动器数据，过期时退回快照；版本号未动。

## LANE 预判

2（依据：命中 `driver/*`、`mods/*`）。合并：审查员 green 后自动合并。

## SCOPE

driver/internal/status/\* driver/internal/scheduler/\* driver/internal/executor/\* mods/swarm-board/hooks/driver-status.ts mods/swarm-board/hooks/register.tsx

## 依赖

blocked-by ZII-867（status JSON）、ZII-890（监听机制）、ZII-882（executor 包当前队尾，串行避免冲突）。

## intake 自检

* CHECK 可执行且会红：是。main 上不存在 TestStatusFilePush 与 driver-status.ts。
* SCOPE 路径存在：是（包与目录已存在，driver-status.ts 新建在已存在的 hooks/ 下）。
* lane 预判与 glob 一致：是。
* project / Todo / ready-for-agent：是。
* 依赖：三条 blocked-by，已建关系。
* 拆票形状：单票，无接口票。
* estimate=3 → 预计 tier=default
