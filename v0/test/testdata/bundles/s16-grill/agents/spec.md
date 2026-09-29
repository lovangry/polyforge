---
name: spec
description: S16 fixture spec step - consumes the grill question/answer log
tools: read, write
output:
  type: object
  required: [summary]
  properties:
    summary:
      type: string
  additionalProperties: false
---

# spec

Author the specification from the gathered requirements, reading the
grill section of `artifacts/grill-qa.md`.
