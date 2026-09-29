---
name: s16-grill
description: S16 usage fixture - the needs_input relay round trip of an interactive grill step
steps:
  - { id: grill, kind: qa, interactive: true }
  - { id: spec, kind: spec }
---

# s16-grill — S16 needs_input relay fixture

The S16 usage scenario (test/usage_s16_grill_relay_test.go) drives the
interactive `grill` step: the agent persists a `pending_question`, the
interrupting publish makes it visible on the observe faces, the owner
answers through the real CLI answer verb, the five-field relay reaches
the waiting agent, the SAME step continues in place (never a new step),
and the question/answer pair lands in the step's `answers[]` archive and
in the shared `artifacts/grill-qa.md` log the `spec` step consumes.
