---
name: e2e-run
description: Re-run the authored e2e suite against the implementation code produced - the hard rerun gate after code; any red scenario fails the gate and rewinds to code.
tools: [read, bash]
output:
  type: object
  required: [results_path, scenarios, authored_titles, verdict]
  properties:
    results_path:
      type: string
      description: Capsule-relative path to the results document (artifacts/e2e-results.md).
    verdict:
      type: string
      enum: [pass, fail]
      description: pass means every authored scenario passed or was honestly skipped (skips never fail the gate - review judges their justification); fail means at least one scenario failed. A fail verdict is recorded iff at least one scenario's status is fail - the oneOf split below makes that mechanical.
    fail_reason:
      type: string
      description: Required when verdict is fail - a precise description of what failed, to be carried back to code as its pending_rewind_input. Name the failing scenario's title, the expected-vs-observed contrast, and the implicated plan unit where discernible; vague language like "tests failed" without specifics wastes the rewind. The oneOf split below makes the requirement mechanical - a fail verdict without a non-empty fail_reason does not validate.
    authored_titles:
      type: array
      minItems: 1
      uniqueItems: true
      items: { type: string }
      description: Verbatim echo of e2e-author's scenario_titles, read from execution.json's steps.e2e-author.output.scenario_titles. Must equal it element for element and in order - this is the mechanical cross-check that one result exists per authored scenario, no more and no fewer; a run whose recorded titles diverge from the authored list did not exercise the authored suite. uniqueItems mirrors the producer's own schema (R3-F3 - e2e-author's scenario_titles declares uniqueItems too), so a VERBATIM echo of a valid authored list can never violate it; without the producer-side constraint a duplicated authored list would leave this step with no valid response - echoing it violates uniqueItems here, deduping it violates the exact-echo instruction. The element-for-element equality itself is not expressible in draft-07; the WI Agent performs that comparison mechanically before marking done, via the echoed-field cross-check: when the output schema requires echoing a field of a referenced prior step's output, compare element for element before marking `done` - a mismatch means the step did not exercise what the upstream step produced, and is an infrastructure failure of this step, never a silent `done`.
    scenarios:
      type: array
      minItems: 1
      items:
        type: object
        required: [title, status]
        properties:
          title: { type: string }
          status: { type: string, enum: [pass, fail, skipped] }
          detail: { type: string }
        oneOf:
          - description: A pass or fail entry - detail optional (recommended for a fail, mandatory nowhere).
            properties:
              status: { type: string, enum: [pass, fail] }
          - description: A skipped entry - only legal with a non-empty detail recording why the scenario was skipped and what review should weigh.
            properties:
              status: { type: string, enum: [skipped] }
              detail: { type: string, minLength: 1 }
            required: [status, detail]
      description: Per-scenario outcome, one entry per authored title, in the same order as authored_titles. Every entry's title must appear in authored_titles. A fail verdict is recorded iff at least one entry's status is fail (mechanical via the top-level oneOf/contains below).
  oneOf:
    - description: A pass verdict - no scenario may be recorded as fail under it, so an all-pass array cannot ride a fail verdict and vice versa (mirrors verification's checks constraint; draft-07 mechanical only).
      properties:
        verdict: { type: string, enum: [pass] }
        scenarios:
          type: array
          items:
            type: object
            properties:
              status: { type: string, enum: [pass, skipped] }
      required: [verdict]
    - description: A fail verdict - at least one scenario failed; requires a non-empty fail_reason. The contains constraint mechanically requires at least one scenario with status fail, so a red scenario cannot hide under a pass verdict (draft-07 mechanical only).
      properties:
        verdict: { type: string, enum: [fail] }
        fail_reason: { type: string, minLength: 1 }
        scenarios:
          type: array
          minItems: 1
          contains:
            type: object
            required: [status]
            properties:
              status: { type: string, enum: [fail] }
      required: [verdict, fail_reason]
---

# e2e-run

Re-run every scenario `e2e-author` authored (`artifacts/e2e-scenarios.md` and/or the
generated test files it produced under `repos/<name>/**`) against the implementation
`code` produced. `e2e-baseline` laid the red bar against the pre-implementation tree;
this step re-runs the same authored suite after `code` and judges it — the rerun is a
hard gate: red rewinds to `code`.

## What to do

1. Read `artifacts/e2e-scenarios.md` (and any test files `e2e-author` recorded in
   `test_files`).
2. Copy `scenario_titles` verbatim from `execution.json`'s
   `steps.e2e-author.output.scenario_titles` into your `authored_titles` output field -
   element for element, same order, no additions and no omissions. The executor
   mechanically compares the two lists; they must be equal, and `scenarios` must carry
   exactly one entry per authored title.
3. Run each scenario as written — if a scenario has a runnable test file, execute it via
   `bash`; if it is only a structured GIVEN/WHEN/THEN document, exercise the described
   flow manually (through the project's actual interfaces: CLI, API calls, etc.) and
   judge the outcome against the THEN clause.
4. Do not rewrite, relax, or skip a scenario because the implementation does not satisfy
   it as written — record it as `fail` with a detailed `detail` and move on to the next
   scenario. Silently editing a failing scenario to make it pass defeats the entire
   purpose of authoring it independently in an earlier step.
5. Record every scenario's outcome in `artifacts/e2e-results.md`, in the same order
   `e2e-author` declared them, so `review` can line the two documents up directly.

## Gate semantics

`e2e-run` gates the flow on the rerun result: `verdict: fail` is recorded iff at least
one scenario's status is `fail` (the schema's oneOf/contains split makes that
mechanical). A red rerun means the implementation under-delivered the bar that was
laid and visible before `code` ran — an implementation failure, not review material.
On a red rerun, report `outcome: gate_failed` with `verdict: fail`; the driver rewinds
to `code` with this verdict attached as `pending_rewind_input` (the existing bounded
mechanism — the workflow's rewind limits still apply). Write `fail_reason` precisely:
the failing scenario's title, expected vs observed, and the implicated plan unit where
discernible.

Skips are legal ONLY with a non-empty `detail` recording why, and they do not fail the
gate — `review` (the next step) judges whether a skip's justification is acceptable.
A green rerun records `verdict: pass` and `outcome: completed`, and the flow advances
to `review`.

## Output

Emit the final JSON object per the schema above: the results document path, the verbatim
`authored_titles` echo (equal to `e2e-author`'s `scenario_titles`), the per-scenario
outcome list - one entry per authored title, same order - and the gate `verdict`.
