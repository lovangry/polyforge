---
name: s15-lifecycle
description: S15 usage fixture - graceful shutdown mid-step, then resume of the SAME step
steps:
  - { id: alpha, kind: code }
  - { id: beta, kind: code }
---

# s15-lifecycle — S15 shutdown/resume lifecycle fixture

The S15 usage scenario (test/usage_s15_shutdown_resume_test.go) completes
`alpha`, parks the WI Agent while `beta` is in flight, then issues the
graceful shutdown. The published HEAD must carry `alpha: done` +
`beta: in_progress`; the resumed generation re-derives from that state,
re-dispatches ONLY `beta`, and never re-runs the completed step.
