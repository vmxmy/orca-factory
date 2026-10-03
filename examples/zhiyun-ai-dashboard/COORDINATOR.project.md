# zhiyun-ai-dashboard 的项目规则

通用流程见 `COORDINATOR.md`；本文件只写本仓库特有的判据与规矩，冲突时以本文件为准。参数在 `agent-graph.env`，重型检查在 `gate.project.sh`。

## Lane 判据

判定在 `agent-graph.env`：`AGENT_LANE2_GLOBS` 命中 → lane 2，`AGENT_LANE3_GLOBS` 命中 → lane 3（gate 直接判红），`scripts/agent-gates/*` 在 `gate.sh` 里硬编码为 lane 2；其余 lane 1。以 env 里的 glob 为准，gate 输出的 `LANE` 行就是结论。下表仅为展示用说明，改判据改 env，不改表。

| Lane | 本仓库的路径 / 操作 |
| --- | --- |
| 1 可逆·局部 | `web/src` 下的页面、组件、viewmodel、lib；单个 `functions/api/*.js` 内部修复加测试；zh-CN 文案；文档。几十行、有可写的 vitest 断言 |
| 2 可逆·宽（`AGENT_LANE2_GLOBS`） | 路由契约（`spec/`、`functions/api/v1/_*-routes.js`）、`functions/api/_session.js`、`worker.js`、`wrangler.toml`、`web/src/lib/types.ts`、`registry/`、`functions/_greptime-read.js`、`package.json`、`web/package.json`、`.github/`、`.claude/`、`.codex/`、`AGENTS.md`、`CLAUDE.md`、`scripts/agent-gates/`、`web/src/components/ui/`（Kit 原语）、`postgres-migrations/` |
| 3 不可逆（`AGENT_LANE3_GLOBS` + 操作） | 文件：`.dev.vars*`、`wrangler.toml.secrets*`、`.github/workflows/deploy*`。操作（不经文件，靠流程禁止）：对 CNPG / Greptime **执行**迁移或 DDL、`wrangler deploy` / `secret`、LiteLLM 写操作、充值、R2 / D1 远程删除、`sops -d`、`kubectl delete`、push main / force push |

## 拆分注意

- source 文件上限 30 个导出 / 800 行、test 文件 1500 行（`functions/test/dependency-direction.js` 的 `SIZE_THRESHOLDS`），新导出先查落点的导出数；冻结清单只能缩不能加，超限只能拆文件。
- 生成物：`spec/`（`scripts/generate-api-specs.mjs`）、`docs/generated/`（各自生成器）。冲突或改动时重生成，不手改。
- 改 `functions/` 下模块先读 `functions/AGENTS.md`；改存储、迁移、flag、路由按根 `AGENTS.md` 的「做 X 之前读 Y、改完跑 Z」表执行。

## 工人约束（拼进每份 task spec）

- 类型检查只用 `cd web && npx tsc -b`，绝不用 `--noEmit`。
- 测试用 `npx vitest`，不用 `pnpm vitest`；只跑定向测试。
- i18n 只改 `web/src/i18n/resources/zh-CN.ts`；en-US / zh-HK 由 coordinator 按 i18n-translate 技能交 Haiku 生成，不用 OpenCC。
- 新增 `@ts-ignore` / `@ts-nocheck` / `as any` 一律不许（检查门会拦）。

## 合并与上线

- 合并即部署（CI push main 触发）。合并前 `behind_by > 0` 必须 rebase 并重跑 CI。
- 部署工作流名与冒烟命令见 `agent-graph.env`（`/login` 200、`/api/v1/me` 401）。
- UI 改动可额外 grep 线上 `assets/index-*.js` 确认新 i18n key 已上线。
