---
name: warm
description: S14 fixture warm step - completes inside every budget layer
tools: read, write
output:
  type: object
  required: [summary]
  properties:
    summary:
      type: string
  additionalProperties: false
---

# warm

A cheap step that completes within both budget layers; it stays `done`
when `burn` is later stopped by `WF-103 OVER_BUDGET`.
