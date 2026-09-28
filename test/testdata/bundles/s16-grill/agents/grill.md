---
name: grill
description: S16 fixture grill step - the interactive question ask the scenario relays
tools: read, write
output:
  type: object
  required: [qa_log]
  properties:
    qa_log:
      type: string
  additionalProperties: false
---

# grill

Ask the clarifying questions, persist the open question as
`pending_question` (the interrupting publish rides the same write), and
on the relayed answer continue THIS step in place: append the consumed
pair to `artifacts/grill-qa.md` (grill section) and emit
`{"qa_log": "artifacts/grill-qa.md"}`.
