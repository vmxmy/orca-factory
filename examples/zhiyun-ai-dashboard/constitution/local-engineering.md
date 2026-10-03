# 项目工程条款：zhiyun-ai-dashboard

与 `constitution/engineering.md` 并列读取，只追加。

## 模块大小上限

- source 文件上限 30 个导出 / 800 行，test 文件 1500 行；检查器 `functions/test/dependency-direction.js` 的 `SIZE_THRESHOLDS`。新导出先查落点导出数；超限只能拆文件。依据：该测试文件。
- 冻结清单只能缩不能加。依据：同上。

## 生成物

- `spec/` 由 `scripts/generate-api-specs.mjs` 生成；冲突或改动时重生成，不手改。依据：`scripts/`。
- `docs/generated/` 由各自生成器生成；同上。

## 必读

- 改 `functions/` 下模块先读 `functions/AGENTS.md`；改存储、迁移、flag、路由按根 `AGENTS.md` 的「做 X 之前读 Y、改完跑 Z」表执行。
