---
name: polyforge
description: Polyforge 路由 skill —— 用户 harness 唯一安装面。把自然语言意图映射为 12 个 verbs；机制权威在 Daemon/Core 文档，本 skill 只管怎么用。
license: proprietary
compatibility:
  harnesses: [pi, claude]
---

# polyforge 路由 skill

意图映射表（NL 模式 → verb fragment）：

| 意图 | fragment |
|---|---|
| 建票 | `verbs/file.md` |
| 执行某 WI | `verbs/submit.md` |
| 看进度 | `verbs/observe.md` |
| 回答 needs_input | `verbs/answer.md` |
| 跑一批 | `verbs/batch-confirm.md` |
| 今天有什么活 | `verbs/queue.md` |
| 暂停 / 继续 | `verbs/pause.md` / `verbs/resume.md` |
| 不要了 | `verbs/cancel.md` |
| 收尾 | `verbs/wrap.md` |
| 之前怎么处理过 X | `verbs/recall.md` |
| 升级 / 启停 / 自检 | `verbs/ops.md` |

> 骨架占位。正式实现按 `docs/design/polyforge-vnext/01-workflow-skills.md` 与
> `00-architecture.md` §5 的 verbs 分流表展开；verbs 只调 API 不含机制。
