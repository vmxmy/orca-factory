# orca-factory driver

这是用于取代 LLM coordinator 的 Go 驱动器，目前提供子命令注册表和 `version` 命令；整体演进计划见地图 [ZII-826](https://linear.app/ziikoo/issue/ZII-826)。

调度器现在推进无部署、无需合并结果 gate 的已验证队首 PR，并在 GitHub 回读确认 squash 后写回 Done。交接与合并 intent 存在 Linear 评论，重启按冻结入队身份恢复；缺验证证据、CI 未明确成功、manual 路径、重叠或部署要求都会保留归属并报告等待。契约及 #142/#143 后续接点见 `CONTRACTS.md`。

`driver serve --dry-run` 不合并、不写回、不 fetch；scope 使用已读取的声明。旧票检查若需要 fetch 会打印省略原因并报告等待，不据此推断可以释放或重派。
