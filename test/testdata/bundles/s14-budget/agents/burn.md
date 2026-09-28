---
name: burn
description: S14 fixture burn step - the tiny step budget this scenario exceeds
tools: read, write, bash
output:
  type: object
  required: [summary]
  properties:
    summary:
      type: string
  additionalProperties: false
---

# burn

The over-consuming step: its own `budget: {requests: 2}` ceiling is hit on
the first dispatch. Hitting it is a resource interrupt (`WF-103
OVER_BUDGET`), never a quality signal (01 §6.6).
