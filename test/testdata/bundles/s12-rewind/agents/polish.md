---
name: polish
description: S12 fixture polish step - refine the build output before the gate
tools: read, write
output:
  type: object
  required: [summary]
  properties:
    summary:
      type: string
  additionalProperties: false
---

# polish

Refine `artifacts/build.md` and write `artifacts/polish.md`.
