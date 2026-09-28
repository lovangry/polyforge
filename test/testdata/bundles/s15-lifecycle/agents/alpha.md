---
name: alpha
description: S15 fixture alpha step - completes before the shutdown lands
tools: read, write
output:
  type: object
  required: [summary]
  properties:
    summary:
      type: string
  additionalProperties: false
---

# alpha

The step that reaches `done` and publishes before the shutdown; the
resumed generation must never dispatch it again.
