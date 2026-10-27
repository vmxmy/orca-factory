# ziikoo-driver

Claude Code mod。斜杠命令 `/ziikoo-driver` 打开「Driver」面板，五页分两组。会话里第一次打开时在流水线页，之后再打开回到上次所在的页。

运行组看 driver 现在在做什么：

- 流水线：按项目分组显示在途票。每张票一行：票号、阶段轨道（领、施工、门禁、审查、合并、部署）、阶段名、在当前阶段停留的时长；下一行灰显标题，卡住时再多一行原因。没配部署的仓库不画部署点。颜色只表示状态：完成绿、当前蓝、需要注意黄（correction、处置 Agent、快照过期）、失败红。项目标题行带检出与 core 同步徽标。顶部是在途、部署屏障、待领计数。窄于 80 列时不画轨道。
- 健康：构建短 sha、服务状态、维护模式、状态快照；`doctor` 的每项检查一行（中文名、通过 / 注意 / 失败、原因）；最后是 problems。

配置组看本机 driver 的生效配置：

- 模型：按仓库切换，列出各角色的 Agent、模型、强度和来源键，底部是打回升档与小票审查阈值摘要。driver 施工首轮总用 default 档，strong 只在打回后升档时使用。
- 调度：`driver.toml` 的 8 个调度键，分为并发与限速、GitHub、Orca 与执行器、账本四组。每行依次是中文名、带单位的值、来源和灰色的原键名；来源是 driver.toml 时高亮，是默认值时灰显。
- 环境：最上面是工厂目录；`driver.env` 的键只显示「已设置 / 未设置」，不显示值；服务环境、代理和工具路径。

页签栏每组前面是组名，当前组的组名加粗。右上角在运行组显示快照新鲜度，在配置组显示 config 的取数时间。

## 打开

- 本仓库：交互会话自动加载 `.claude/skills/ziikoo-driver/`，直接输入 `/ziikoo-driver`。
- 其他仓库：`claude --plugin-dir <本目录的绝对路径>`，再输入 `/ziikoo-driver`。

面板打开后持有键盘：`1`–`5` 切页（运行组 1 流水线、2 健康；配置组 3 模型、4 调度、5 环境），`r` 刷新，Tab 在按钮和仓库下拉之间移动，Esc 回到输入框。

## 数据来源

面板只运行 `orca-factory-driver` 的三个只读命令，不读任何配置文件：`config --json`（每页都用，打开面板和按 `r` 时读取；配置组的页只读它）、`status --json`（流水线页的在途票和右上角的快照新鲜度；打开面板时停在运行组、切到运行组的页、在运行组按 `r` 时读取，停在运行组时每 10 秒重读一次，关掉面板后停止）、`doctor --json`（流水线页的项目徽标和健康页的检查列表；在读取 status 的同样时机运行，退出码 1 也照常解析）。输出必须符合 `driver-config-v1` 契约（见 `driver/CONTRACTS.md`）。命令失败时面板显示退出码和 stderr 前几行；schema 或字段形状不对，或者出现面板不认识的调度键时，显示「输出无法解析」和原因。右上「↻ 刷新」重新取数。

## 检查

```sh
claude plugin validate .claude/skills/ziikoo-driver && claude plugin test .claude/skills/ziikoo-driver
```

测试在引擎底层替身 `process.run`，用 `test/sample-config.ts` 的样本走真实取数路径。
