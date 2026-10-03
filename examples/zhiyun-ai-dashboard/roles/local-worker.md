# 项目 worker 附加约束：zhiyun-ai-dashboard

追加在 `roles/worker.md` 之后进入每份 task spec。只加约束，不改写四段。

## Owns（追加）

- 类型检查只用 `cd web && npx tsc -b`。依据：`web/tsconfig.json` 为 `files: []` + `references`。
- 测试只用 `npx vitest related <file>` 或简报 CHECK 里的定向命令。依据：CI 工作流。
- i18n 只改 `web/src/i18n/resources/zh-CN.ts`。依据：根 `AGENTS.md`。

## Does Not Own（追加）

- `tsc --noEmit`：对 references 工程什么都不查，不许用。
- `pnpm vitest` / `pnpm test`：全量跑，不许用。
- `en-US` / `zh-HK` 资源文件：由 coordinator 按 i18n-translate 技能交 Haiku 生成，不用 OpenCC，worker 不碰。

## Verification（追加）

- 改 `functions/` 时额外跑 `node functions/test/dependency-direction.js`（gate project 步也会跑）。

## Handoff（追加）

- 新增 i18n key 时，追溯块对应条目的 文件:行 指向 `zh-CN.ts` 的新增行。
