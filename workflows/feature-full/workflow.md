---
steps:
  - { id: grill, kind: qa, interactive: true }
  - { id: spec, kind: author }
  - { id: grill-review, kind: gate, interactive: true, on_fail: { action: rewind, target: spec } }
  - { id: plan, kind: author }
  - { id: code, kind: code }
  - { id: e2e-author, kind: author }
  - { id: verification, kind: gate, on_fail: { action: rewind, target: code } }
  - { id: e2e-run, kind: code }
  - { id: review, kind: review, on_fail: { action: rewind, target: code } }
  - { id: release, kind: code, on_fail: { action: rewind, target: review } }
budget: { requests: 200, tokens: 500000, wall_minutes: 240 }
---

# Feature-full 流程说明

> 骨架占位：frontmatter 是唯一结构 schema（`docs/design/.../01-workflow-skills.md` §4），
> 本正文是 WI Agent 第 0 步加载的流程级说明。正式内容按项目方法论展开。
