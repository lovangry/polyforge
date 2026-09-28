---
name: e2e-run
description: Execute the e2e-author scenarios against the verified implementation and record raw pass/fail results per scenario.
tools: [read, bash]
output:
  type: object
  required: [results_path, scenarios, authored_titles]
  properties:
    results_path:
      type: string
      description: Capsule-relative path to the raw results document (artifacts/e2e-results.md).
    authored_titles:
      type: array
      minItems: 1
      uniqueItems: true
      items: { type: string }
      description: Verbatim echo of e2e-author's scenario_titles, read from execution.json's steps.e2e-author.output.scenario_titles. Must equal it element for element and in order - this is the mechanical cross-check that one result exists per authored scenario, no more and no fewer; a run whose recorded titles diverge from the authored list did not exercise the authored suite. uniqueItems mirrors the producer's own schema (R3-F3 — e2e-author's scenario_titles declares uniqueItems too), so a VERBATIM echo of a valid authored list can never violate it; without the producer-side constraint a duplicated authored list would leave this step with no valid response - echoing it violates uniqueItems here, deduping it violates the exact-echo instruction. The element-for-element equality itself is not expressible in draft-07; the WI Agent performs that comparison mechanically before marking done (pf-execute §4 echoed-field cross-check).
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
      description: Per-scenario outcome, one entry per authored title, in the same order as authored_titles. Every entry's title must appear in authored_titles.
---

# e2e-run

Execute every scenario `e2e-author` authored (`artifacts/e2e-scenarios.md` and/or the
generated test files it produced under `repos/<name>/**`) against the implementation
`code` produced and `verification` already confirmed builds and passes unit-level checks.

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

## This step does not gate

`e2e-run` is `kind: code`, not `kind: gate`, in this reference flow — it produces raw
results, not a pass/fail verdict for the workflow engine to act on. `review` (the next
step) is the one that judges whether the overall change is acceptable, using these raw
results as one of its inputs alongside the code diff itself. Do not editorialize results
into an overall verdict here; report each scenario's outcome plainly.

## Output

Emit the final JSON object per the schema above: the results document path, the verbatim
`authored_titles` echo (equal to `e2e-author`'s `scenario_titles`), and the per-scenario
outcome list - one entry per authored title, same order.
