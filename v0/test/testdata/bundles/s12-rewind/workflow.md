---
name: s12-rewind
description: S12 usage fixture - business gate FAIL drives the bounded on_fail rewind (01 §6.2)
steps:
  - { id: build, kind: code }
  - { id: polish, kind: code }
  - { id: check, kind: gate, gate: true, on_fail: { action: rewind, target: build } }
rewind: { max_total: 1, max_per_step: 2 }
---

# s12-rewind — S12 gate FAIL rewind fixture

The S12 usage scenario (test/usage_s12_gate_rewind_test.go) drives this
bundle with the fakepi drive executor. `build` and `polish` are plain code
steps; `check` is the business gate whose FAIL verdict rewinds the whole
`[build, polish]` interval back to `build` per `01-workflow-skills.md`
§6.2. `rewind.max_total: 1` is deliberately tiny so the rewind-exhausted
terminal (`failed_rewind_exhausted`, WF-101) fires on the SECOND gate FAIL
of the same run — the bounded shape the scenario pins.

Artifacts: `build` writes `artifacts/build.md`, `polish` writes
`artifacts/polish.md`, `check` writes `artifacts/check-verdict.md`.
