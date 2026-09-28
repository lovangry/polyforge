---
name: beta
description: S15 fixture beta step - in flight when the shutdown lands
tools: read, write
output:
  type: object
  required: [summary]
  properties:
    summary:
      type: string
  additionalProperties: false
---

# beta

The step that is `in_progress` at the shutdown boundary; the fresh
generation after `pf resume` re-derives and continues exactly this step.
