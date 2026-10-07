# Driver 开发检查

在仓库根目录运行以下命令，人和 Agent 使用同一入口。需要 [driver/go.mod](go.mod) 指定的 Go toolchain、GNU Make、Bash、Git；race 还需要 CGO 支持和 C 编译器。语言最低版本以同一文件的 `go` 指令为准。

| 入口 | 行为 |
| --- | --- |
| `make build` | 构建所有 driver 包，不安装或重启服务 |
| `make test` | gotestsum 运行一次未缓存的完整测试，输出摘要、JSON、JUnit |
| `make vet` | `go vet ./...` |
| `make lint` | golangci-lint，超时 5 分钟 |
| `make race` | gotestsum 运行完整 race 检查，输出独立报告 |
| `make vuln` | govulncheck；漏洞或扫描失败均返回非零 |
| `make check` | 依次执行 build、vet、lint、test、race、vuln，遇失败停止 |
| `make tools` | 安装固定版本的 gotestsum、golangci-lint、govulncheck |
| `make dev-tools` | 安装版本锁文件中的全部工具 |

测试相关入口默认不重跑失败测试，`-count=1` 禁用测试结果缓存。聚焦单个包或测试时使用：

```sh
make test PACKAGES=./internal/infra TEST_ARGS='-run TestRetry'
make race PACKAGES=./internal/executor
```

工具按 [scripts/dev-tools.lock](../scripts/dev-tools.lock) 的固定版本安装到 `.omc/tools/bin`，使用 module 中指定的 Go toolchain 构建。每次使用时验证可执行文件的包路径、版本和编译器；版本不匹配则重新安装。安装不改变 `driver/go.mod`、`go.sum`、用户 PATH 或编辑器设置。首次安装需要访问 Go 模块代理；漏洞扫描还需要访问官方漏洞库。失败不会转成成功，也不自动降级到旧工具。`.omc/` 已在现有 gitignore 中排除。

报告路径：

- `make test`：`.omc/reports/test/tests.json`、`.omc/reports/test/junit.xml`
- `make race`：`.omc/reports/race/tests.json`、`.omc/reports/race/junit.xml`

同一入口下次执行会覆盖报告。JSON 是 Go test2json 事件流，一行一个 JSON 对象；JUnit 包含测试结果及失败。命令保持真实失败退出，不能仅以报告文件存在判断通过。不同入口的报告互不覆盖；避免在同一工作树并发运行两个 `make test` 或两个 `make race`。

## Goroutine 泄漏：goleak

[executor/main_test.go](internal/executor/main_test.go) 和 [scheduler/main_test.go](internal/scheduler/main_test.go) 在 `TestMain` 中调用 `goleak.VerifyTestMain(m)`，检查整包测试结束后的残留 goroutine。依赖由 go.mod 管理，仅用于测试。

```sh
make test PACKAGES='./internal/executor ./internal/scheduler'
make race PACKAGES='./internal/executor ./internal/scheduler'
```

定位失败可以用 `TEST_ARGS='-run TestName'` 聚焦已有测试；验收泄漏修复仍要运行整包，让生命周期相互作用得到检查。先确保 context 取消、资源关闭和 goroutine 退出已被等待，再判断泄漏。保持无宽泛忽略；确有第三方常驻 goroutine 时，先证明所有权与生命周期，再评审最窄例外及原因。

## 虚拟时间：synctest

[infra/synctest_test.go](internal/infra/synctest_test.go) 从外部测试包调用公开 `infra.Retry`。在 `synctest.Test` 中保持 `Options.Sleep` 未设置，固定 `Random` 返回 `0.5` 消除 jitter，覆盖真实默认计时器路径。调用时点应为 0、2、6、14 秒，分别验证 2/4/8 秒退避。

取消测试在第一次、第二次、第三次等待期间取消 context，断言返回 `context.Canceled`；再跨过原有重试截止时间，断言操作没有再次调用。bubble 中的 `time.Sleep` 推进虚拟时间；`synctest.Wait` 等待其他 goroutine 稳定阻塞并同步观察，不用真实等待或轮询来同步共享变量。

```sh
make test PACKAGES=./internal/infra TEST_ARGS='-run TestRetryDefaultSleep'
make race PACKAGES=./internal/infra TEST_ARGS='-run TestRetryDefaultSleep'
```

验证新断言敏感性时，在隔离临时副本中改变退避时长、屏蔽默认 Sleep 的取消或允许取消后再调用；逐项确认测试变红，恢复后变绿。不要把 mutation 留入提交或在共享 worktree 中修改他人正在验证的代码。保留现有录制契约测试。

## 本地语言服务与调试

`make dev-tools` 还安装锁文件指定的 gopls 和 Delve。使用工作树工具时可以直接调用：

```sh
.omc/tools/bin/gopls version
.omc/tools/bin/dlv version
```

也可在自己的 shell 中临时 `export PATH="$PWD/.omc/tools/bin:$PATH"`。此入口不修改编辑器设置或覆盖用户已有工具。工具升级时修改锁文件，再检查真实诊断/调试行为；不要用 `@latest`。

## GitHub Actions

[Go checks](../.github/workflows/go.yml) 对每个 PR 触发（无 path/branch filter），也支持 main push 和手动 `workflow_dispatch`。五个独立 job 直接运行相同的 Make 入口，没有相互依赖：

| 稳定检查名称 | 命令 |
| --- | --- |
| `Driver build` | `make build` |
| `Driver vet` | `make vet` |
| `Driver lint` | `make lint` |
| `Driver race` | `make race` |
| `Driver vuln` | `make vuln` |

工作流仅声明 `contents: read`，checkout 不持久化凭证，Actions 引用固定完整 commit SHA。setup-go 从 `driver/go.mod` 优先读取 toolchain。race 成功或失败后都尝试上传两份报告，artifact 名 `race-reports`，保留 14 天；若工具安装阶段失败，没有报告时不掩盖原失败。

合并到 main 要求上述五项检查通过。远端分支保护使用上表名称并绑定 GitHub Actions app，要求分支更新至最新主线，管理员也受约束；通过 GitHub API 回读确认配置，检查启动本身不能证明保护已生效。本地 Make 不修改远端保护。现有 `scripts/agent-gates/gate.sh` 和 project gate 继续保留；`make check` 不替代 scope、固定基线、契约及部署门禁。

### Go 模块下载代理

Go checks 通过仓库变量 `GO_MODULE_PROXY` 设置所有 job 的 `GOPROXY`，未设置时仍使用 `https://proxy.golang.org,direct`。遇到本地 runner 无法访问默认代理时，维护者可设置为 `https://goproxy.cn,https://proxy.golang.org,direct`；逗号分隔的后备仅在代理返回 404/410 时启用，超时不会自动切换。该设置不禁用 Go 校验和数据库、不跳过任何检查，也不改变 runner 标签或锁定的工具版本；代理可达性和实际下载以最终 CI 为准。

CI 还可通过仓库变量 `CI_NETWORK_PROXY` 设置 job 的 `http_proxy` 和 `https_proxy`，用于 checkout、工具链和依赖下载。未设置时为空，保持直接连接。代理地址必须从 runner 容器内可达；宿主的 `127.0.0.1` 不是容器中的宿主地址。本机通过仅监听 Docker bridge 地址 `172.17.0.1:17890` 的 systemd socket 将连接转发到现有 `127.0.0.1:7890` 代理，未改用 host 网络或挂载宿主文件。维护者需保留该 socket 服务；切换托管 runner 前清空这个本机代理变量。不要把含密码的代理 URL 放入仓库变量。

### Runner 选择与本地隔离

五个 job 使用仓库变量 `GO_RUNNER_LABELS_JSON` 选择 runner。变量未设置或为空时，默认标签为 `["ubuntu-24.04"]`，使用 GitHub 托管云端 runner。本地 runner 就绪后，维护者可将变量设置为 JSON 字符串数组，例如：

```json
["self-hosted", "linux", "x64", "orca-go-isolated"]
```

示例标签需要与已注册 runner 的标签完全对应，所有标签必须同时匹配。变量是调度选择，**不是自动 failover**：选择本地标签后，runner 离线或忙碌会使 job 等待；不会因为不可用自动转到云端。需要云端时清空/删除变量或显式设置 `["ubuntu-24.04"]`，再发起新运行。值必须是非空字符串数组；无效 JSON 会使表达式报错，不会回退。

本地方案使用每个 job 独立的一次性（ephemeral）容器，任务完成后退出并销毁容器及其工作目录。主机侧启动器负责创建容器、注册 runner、补充下一实例和清理；工作流本身不会创建或部署 runner。 安装与维护步骤见[本地 runner 启动器说明](../scripts/local-runner/README.md)。

- 容器启动时明确限制 CPU、内存和进程数，并限制并发实例总量，为主机上的生产服务保留资源。
- 禁止挂载宿主 home、Docker socket 或生产凭据；使用独立临时工作目录，不让 job 接触生产配置和持久状态。
- 长期 GitHub 授权仅保留在宿主。JIT 短期配置经 stdin 进入 runner 进程，用于注册自身，不会继承给 job 环境；PR job 保持现有最小权限。不要用常驻生产执行器充当 CI runner。

设置变量、部署启动器和调整实际资源上限由维护者执行；只有真实 job 完成才能证明本地 runner 已接通。job 名称和所调用的 Make 入口保持不变。

[Dependabot](../.github/dependabot.yml) 每周检查 `/driver` 的 Go module 与根目录的 GitHub Actions，依赖变更仍经 PR 门禁。

## 官方接口依据

- [gotestsum](https://github.com/gotestyourself/gotestsum)：`--jsonfile`、`--junitfile` 和退出状态。
- [setup-go v6.0.0](https://github.com/actions/setup-go/tree/v6.0.0)：`go-version-file` 优先读取 toolchain，`cache-dependency-path` 指向 driver/go.sum。
- [upload-artifact v4.6.2](https://github.com/actions/upload-artifact/tree/v4.6.2)：`.omc` 下报告需要 `include-hidden-files: true`，只上传明确列出的报告文件。
- [GitHub workflow 语法](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax)：路径/分支过滤可使被跳过的必需检查保持 pending。

### Resolver role

Exceptions and approval waits now transfer to `处置Agent` / `Agent处置`.
Historical `人` / `转人` / `等人批` receipts remain readable and are admitted
through the same resolver path. `driver resolve <ticket>` supervises an Orca
worker using `scripts/agent-gates/roles/resolver.md`; `AGENT_ROLE_RESOLVER`
overrides `AGENT_TIER_STRONG` (`agent:model:effort`, default `codex`).

The ticket ledger stores `resolver-state` receipts before dispatch and release.
A retry in the same terminal reuses its frozen payload and request identity.
A replacement terminal reconciles the original task and dispatch from complete
Orca reads; missing evidence never authorizes another worker start. A failed task retains
its evidence and retries after five minutes. Resolver work consumes ordinary
admission capacity. Read failures never authorize a second worker.

Successful worker reports contain a JSON merge or deployment proposal. Only
the scheduler applies it: gate/reviewer evidence, exact PR head, live CI,
repository policy and deployment identity checks still run. Resolver approval
permits lane 2 review waits; it does not permit lane 3. Deployment barriers keep
their FIFO position while a resolver investigates. New validated work receives
a new queue identity; old executor checkpoints and budgets are not reset.
