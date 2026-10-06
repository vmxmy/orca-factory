# orca-factory 流水线

把 Linear 票自动变成合并到 main 的代码：intake 建票，coordinator 领票并编排 worker 与 reviewer，gate 判定结果。

## 角色

**coordinator**:
流水线中负责一张票从领取到合并或异常处置全过程的角色。它是角色名，不指具体进程。
_Avoid_: 编排器、调度器

**驱动器（driver）**:
承担 coordinator 角色、按状态机推进流程的 Go 程序。它以调度器和执行器两种模式运行。
_Avoid_: coordinator 脚本、runner

**调度器**:
驱动器的常驻单实例模式，是唯一的领票者和合并者。
_Avoid_: 主驱动器、scheduler 进程

**执行器**:
驱动器在每张票各自的 Orca 终端中运行的模式，只推进这一张票。
_Avoid_: 子驱动器、ticket runner

**coordinator 会话**:
驱动器在需要判断或写作的环节临时拉起的 LLM 会话，例如写简报、写 correction、解决冲突。
_Avoid_: coordinator（指 LLM 会话时）

**处置 Agent（resolver）**:
负责异常排查、修复与审批复核的角色，把原先需要人工处理的票推进到可继续合并或恢复部署。
_Avoid_: 人工兜底、重试 worker

**归属**:
一张在途票此刻由谁负责推进：执行器、合并通道或处置 Agent，任何时刻只有一个。
_Avoid_: 认领人、owner、锁

**合并通道**:
调度器内按顺序逐个合并 PR 的环节。PR 交给它之后，票的归属从执行器转给合并通道。
_Avoid_: 合并队列、merge lane

**在途**:
归属为执行器、合并通道或处置 Agent 的票，仍占用流水线的并行容量。
_Avoid_: 进行中、running

## 发布

**发布 PR**:
唯一允许修改 `mods/` 下插件版本号的 PR，只改该插件的 `plugin.json` 与 `CHANGELOG.md`，由驱动器自动发起。
_Avoid_: 版本 PR、release 提交

**功能 PR**:
实现一张票的 PR，不得修改插件版本号。
_Avoid_: 普通 PR、开发 PR

## 失败

**correction**:
因 gate、CI、审查员或追溯块不合格而把单元打回 worker 重做的一次返工。
_Avoid_: 重试、返修

**基础设施错误**:
与代码质量无关的失败，例如审查员空输出、模型不可用、限流、登录界面、网络中断。它不计入 correction，分为瞬时、单票、全局三类。
_Avoid_: 环境问题、偶发失败

**单票错误**:
只影响一张票某一步的基础设施错误，计入这张票的基础设施错误预算。
_Avoid_: 票级故障

**全局暂停**:
某个外部依赖整体不可用时，驱动器停止所有依赖它的步骤的状态。暂停不算任何一张票失败。
_Avoid_: 停机、熔断（指流水线时）
