---
name: check
description: S12 fixture gate step - the business verdict that drives the rewind
tools: read
output:
  type: object
  required: [summary, verdict]
  properties:
    summary:
      type: string
    verdict:
      type: string
      enum: [PASS, FAIL]
    reasons:
      type: array
      items: { type: string }
---

# check

Judge the polished build (`artifacts/polish.md`) and emit a business
verdict: `{"summary": ..., "verdict": "PASS"|"FAIL", "reasons": [...]}`.
FAIL rewinds to `build` per the frontmatter `on_fail`.
