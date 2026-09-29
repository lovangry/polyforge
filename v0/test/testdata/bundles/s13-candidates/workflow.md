---
name: s13-candidates
description: S13 usage fixture - model candidate advancement on infra failure (01 §6.3)
steps:
  - { id: probe, kind: code, model_candidates: [fake-alpha, fake-beta] }
---

# s13-candidates — S13 candidate advancement fixture

The S13 usage scenario (test/usage_s13_candidates_test.go) injects
provider errors at the fake-harness level on the first dispatches of
`probe` and pins the `01 §6.3` semantics: the step resumes IN PLACE (never
reset to `pending`), the effective candidate ordinal advances 1 -> 2 per
the recomputation from `attempts[]`, and the ordinal is capped by the
list length (two entries here) — a third infra failure re-dispatches with
`fake-beta` again, never past the end of the list.
