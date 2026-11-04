依赖图：1 层，本批 2 张，互不阻塞，宽度 2。

## 现状
driver/internal/scheduler/merge_gate.go:18-76 的 mergeAdapter.Run 在合并结果 worktree 上直接跑冻结的 gate.sh，不执行 AGENT_INSTALL_CMD。缺工具链时 gate 输出 red 且退出 1，被映射为 merge.ErrGateRed（:72-73），计一次 correction。与 OF-61 同一缺陷，触发于"PR 落后 main 且有文件重叠"的合并验证。

## 验收
断言: 新测试 driver/internal/scheduler/merge_gate_test.go 用 runtime.Run 注入假执行器，两个用例都 PASS：(a) install 在 wt 目录执行且先于 gate.sh；(b) install 失败时 errors.As 得到 *infra.Ticket，errors.Is(err, merge.ErrGateRed) 为 false，gate.sh 未被调用。
CHECK_IS_NEW_TEST: yes
CHECK_TEST_FILES: driver/internal/scheduler/merge_gate_test.go

## LANE 预判
2（依据：命中 driver/*）

## 依赖
无阻塞；与 OF-61 同类修复，互不依赖。

## intake 自检
- CHECK 可执行且会红：是（main b4b5228 实测退出 1）
- SCOPE 路径存在：是（新测试建在已有目录 driver/internal/scheduler/）
- lane 预判与 glob 一致：是
- project / Todo / ready-for-agent：是
- 依赖：无阻塞，related OF-61（canonical ID 已回读）
- 拆票形状：接口票 无；依赖深度 1；最宽一层 2 张
- estimate=3 → 预计 tier=default

## 要做
1. Run() 在执行 gate.sh 前，于合并 worktree（wt）执行 `bash -c "$AGENT_INSTALL_CMD"`；环境沿用 :38-47 已拼好的 env，值取自 runtime.Repositories[...].Env，未设置视为 true。
2. 安装失败时返回 &infra.Ticket{}（经 safeMergeGateError 脱敏），不执行 gate.sh，不返回 merge.ErrGateRed。

## 验收
CHECK: cd driver && go test -count=1 -v -run '^TestMergeGate(RunsInstallCommand|InstallFailureIsTicketInfra)$' ./internal/scheduler/ 2>&1 | grep -c -- '^--- PASS: TestMergeGate' | grep -qx 2

## SCOPE
driver/internal/scheduler/merge_gate.go driver/internal/scheduler/merge_gate_test.go

## 依赖
DEPENDENCIES: {"blockers":[],"related":["6a90fddd-7d67-415d-ac00-37ae8d61c83f"]}
