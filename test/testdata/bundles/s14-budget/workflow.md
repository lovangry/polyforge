---
name: s14-budget
description: S14 usage fixture - step budget exhaustion lands the OVER_BUDGET interrupt (01 §6.6)
budget: { requests: 100 }
steps:
  - { id: warm, kind: code }
  - { id: burn, kind: code, budget: { requests: 2 } }
---

# s14-budget — S14 budget exhaustion fixture

The S14 usage scenario (test/usage_s14_budget_test.go) drives `warm`
normally, then has the fake agent over-consume on `burn`: the step-level
`budget: {requests: 2}` is hit on the FIRST dispatch of `burn` while the
workflow-level baseline (`requests: 100`) stays far away — pinning that
the step layer reports its own `WF-103 OVER_BUDGET` and that over-budget
is a typed interrupt (Core `blocked`, `waiting_on=owner`), never a FAIL
verdict and never a rewind.
