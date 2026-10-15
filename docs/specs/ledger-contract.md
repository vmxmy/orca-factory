# Shell ledger 行为契约

## 0. 冻结范围

本规格冻结 `core/ledger.sh` 的现有可观察行为，供 Go 替换实现使用。它不是功能改进方案。不得把现有 Go 账本的轮转、批量提交、时间窗口、字段校验或锁语义当作 shell 的既有行为。

- 冻结日期：2026-10-08。
- 源码基线：`76a944fbdaaaacb768da3856f5beda59bc3dd810`。
- 权威实现：`core/ledger.sh:1-88`。
- 安装副本：`scripts/agent-gates/ledger.sh:1-88`。本次 `cmp` 确认两份内容相同。
- Linear 已退役（OF-28）。原第 1–7 节冻结的 Linear 评论账本行为（调用者清单、`[ledger] agent-graph` 票地址、环境变量、调用计数、数据格式、读写、退出码、重试与并发）随 shell 账本删除；全文见提交 `d79cd3a20609c1cc9a0e9572c8e48358cdff90f1` 中的本文件。原 shell 字节保存在 `driver/testdata/ledger-shell/original-ledger.sh`，只供 `bin/orca-factory init/update` 识别旧入口并升级。
- 现行 `core/ledger.sh` 只接受 `AGENT_ISSUE_BACKEND=github` 或 `feishu`，直接转给 `driver ledger`；空值或其他值退出 2。

## 8. S2 实施增量（2026-10-08）

- 新契约入口为 `driver ledger-shell append/stats`；无配置、无网络的 `ledger-shell --contract-version` 输出 `ledger-shell-v1`。原 `driver ledger` 不变。
- Go 接管命令分派、发现/创建/追加/查询、三次尝试、固定 sleep、尝试前 count 及 PATH 中 Orca 的进程执行。保持 Orca stderr 直传，不使用共享 runx 的固定路径、超时或诊断捕获。相对 PATH（包括 `.`）按既有 shell 契约查找可执行文件；只在此进程边界处理 Go `exec.ErrDot`，不修改全局 GODEBUG 或共享 runx。
- JSON/统计表达式仍交给 PATH 中现有 jq。复用 `ledger.Title`，不复用不等价的 Go stats helper。任意 JSON 文档、未知字段、整个评论解析、jq JSON 排序、jq 错误状态及聚合失败前 stdout 因此保持；没有实现通用 jq 解释器，也没有执行旧 shell。此交付是 shell 编排迁入 Go，不是纯 Go 或外部依赖清零。
- 当时 `core/ledger.sh` 仅保留 team/project 检查、driver 定位、契约探测及 exec（OF-28 后改为按后端直接转给 `driver ledger`）。`AGENT_DRIVER_BIN` 是可选可执行路径，默认 `orca-factory-driver`；原 `agent-graph.sh` 的环境覆盖和动态分派不变。缺失/不兼容 driver 退出 1，不回退旧实现。
- `bin/orca-factory init/update` 在任何 core 写入前检查 driver 及 ledger 入口。识别冻结原始 blob `b34287c3b5e5fbdd6cd03e5a26e2a8572a7fe1a0` 与当前源码；拒绝自定义/未知/符号链接。暂存到目标同目录，明确设置 `0755` 可读可执行权限及语法检查成功后 rename；失败保留原入口。原子性只覆盖 ledger，不涉及票据或运行状态。
- 新公开 CLI 行为断言覆盖多 JSON/非对象、未知字段、首票与去重、数组排序、警告及 count；关键失败断言覆盖坏字段类型的原始 jq 退出 5/部分 stdout、非法 JSON 的退出 2、Orca stderr 与 exit 7 重试、jq 边界失败。安装断言覆盖缺失/不兼容 driver、自定义/符号链接冲突、rename 失败及原版/当前版成功切换。均先红后绿；这些是合成隔离测试，不冒充录制样本。
- 审查修正：原始 shell 使用版本管理夹具 `driver/testdata/ledger-shell/original-ledger.sh`，来源 SHA、Git blob 和 SHA-256 见相邻 provenance JSON；这是历史行为测试夹具，不是新生产 fallback。安装测试不再读取旧 Git 对象，在无 `.git`/旧对象的源码副本先复现 exit 128、修正后聚焦 race 通过。安装权限、相对 PATH 和 sleep 上下文断言均先红后绿；本批聚焦 test/race 各 3 tests、退出 0，未重复完整 lint/selftest 或重型 gate。
- 完整 selftest 使用新构建的 driver 与原有 fake Orca/sleep，实际通过既有账本、自接入、gate、插件 validate/test、计数等回归；没有真实票据写入。
- 主工作区开发安装副本已通过显式隔离候选 driver 的 `bin/orca-factory update .` 同步，仅 ledger 内容改变（`changed=1 added=0 unchanged=18`）；`check` 与安装入口契约探测均退出 0，源码与副本一致。候选 driver 位于 `/tmp/claude-1000/-home-laoxu-github-orca-factory/79717a69-f186-41dc-9e91-0049c99bec51/scratchpad/s2-integration-driver`，来源源码与当前主工作区 driver 文件字节一致。
- 代码合并与生产/客户发布分开：开发分支使用显式候选 driver 验收，不要求先覆盖日常 PATH 的旧 driver；客户部署不是代码合并前置条件。正式 `/home/laoxu/.local/bin/orca-factory-driver` 保持原字节，尚不支持该契约，须在合并后配套更新。没有重启服务、扫描或修改其他客户仓库；客户目标清单、逐目标配套检查、状态保留及发布结果继续作为真实发布待办。
