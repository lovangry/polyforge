---
name: build
description: S12 fixture build step - produce the implementation the gate judges
tools: read, write, bash
output:
  type: object
  required: [summary]
  properties:
    summary:
      type: string
  additionalProperties: false
---

# build

Produce `artifacts/build.md`. On a rewind re-entry, consume the gate's
FAIL verdict from `pending_rewind_input` and address it.
