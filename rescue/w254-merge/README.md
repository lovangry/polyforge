# 抢救自 polyforge-removed-20261116T200143Z

2026-11-16 删除旧工作树前，从它带出的两处未收尾的 merge 冲突现场
（`docs/upgrade.md`、`test/w254_docs_set_acceptance_test.go`）。
`.base` = merge base（stage1），`.ours` = 本地侧（stage2），`.theirs` = 上游侧（stage3）。
同一次抢救里，暂存区的五个文件改动已作为提交落在本分支上；
13 个未推送提交在分支 `rescue/polyforge19-13w`。
