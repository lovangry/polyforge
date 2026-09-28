---
name: probe
description: S13 fixture probe step - the step whose dispatches walk the candidate list
tools: read, write, bash
output:
  type: object
  required: [summary, candidate]
  properties:
    summary:
      type: string
    candidate:
      type: string
  additionalProperties: false
---

# probe

Run the probe work with the effective model candidate and emit
`{"summary": ..., "candidate": "<the candidate used>"}`. The scenario's
injected provider errors never produce a business judgment here — they are
infrastructure failures handled per `01 §6.3`.
