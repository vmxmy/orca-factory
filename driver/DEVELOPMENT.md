# Driver 开发检查

在仓库根目录运行以下命令，人和 Agent 使用同一入口。需要 Go 1.27.1（`driver/go.mod` 的 toolchain）、GNU Make、Bash、Git；race 还需要 CGO 支持和 C 编译器。语言最低版本仍为 `go 1.25`。

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

## 本地语言服务与调试

`make dev-tools` 还安装 gopls v0.23.0 和 Delve v1.27.2，它们已由本地工具验证使用 Go 1.27.1 编译、执行静态诊断及断点调试。使用工作树工具时可以直接调用：

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

required checks 由维护者在 GitHub 上配置，使用上表名称并绑定 GitHub Actions app。本地 Make 不修改远端保护。现有 `scripts/agent-gates/gate.sh` 和 project gate 继续保留；`make check` 不替代 scope、固定基线、契约及部署门禁。

[Dependabot](../.github/dependabot.yml) 每周检查 `/driver` 的 Go module 与根目录的 GitHub Actions，依赖变更仍经 PR 门禁。

## 官方接口依据

- [gotestsum v1.13.0](https://github.com/gotestyourself/gotestsum/tree/v1.13.0)：`--jsonfile`、`--junitfile` 和退出状态。
- [setup-go v6.0.0](https://github.com/actions/setup-go/tree/v6.0.0)：`go-version-file` 优先读取 toolchain，`cache-dependency-path` 指向 driver/go.sum。
- [upload-artifact v4.6.2](https://github.com/actions/upload-artifact/tree/v4.6.2)：`.omc` 下报告需要 `include-hidden-files: true`，只上传明确列出的报告文件。
- [GitHub workflow 语法](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax)：路径/分支过滤可使被跳过的必需检查保持 pending。
