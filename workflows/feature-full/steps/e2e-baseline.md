---
name: e2e-baseline
description: Execute every authored e2e scenario against the pre-implementation tree and lay the acceptance bar - a per-scenario semantic red with evidence is the legal completed output.
tools: [read, bash]
output:
  type: object
  required: [results_path, scenarios, authored_titles]
  properties:
    results_path:
      type: string
      description: Capsule-relative path to the baseline results document (artifacts/e2e-baseline.md).
    authored_titles:
      type: array
      minItems: 1
      uniqueItems: true
      items: { type: string }
      description: "Verbatim echo of e2e-author's scenario_titles, read from execution.json's steps.e2e-author.output.scenario_titles. Must equal it element for element and in order - this is the mechanical cross-check that one result exists per authored scenario, no more and no fewer; a baseline whose recorded titles diverge from the authored list did not exercise the authored suite. uniqueItems mirrors the producer's own schema (R3-F3 - e2e-author's scenario_titles declares uniqueItems too), so a VERBATIM echo of a valid authored list can never violate it; without the producer-side constraint a duplicated authored list would leave this step with no valid response - echoing it violates uniqueItems here, deduping it violates the exact-echo instruction. The element-for-element equality itself is not expressible in draft-07; the step performs that comparison mechanically before returning its completed envelope, via the echoed-field cross-check: when the output schema requires echoing a field of a referenced prior step's output, compare element for element before returning `completed` - a mismatch means the step did not exercise what the upstream step produced, and is an infrastructure failure of this step, never a silent `completed`."
    scenarios:
      type: array
      minItems: 1
      items:
        type: object
        required: [title, status, evidence]
        properties:
          title: { type: string }
          status: { type: string, enum: [fail] }
          evidence: { type: string, minLength: 1 }
      description: One entry per authored title, in the same order as authored_titles. Every entry records a semantic failure - the assertion-level red that is this step's legal completed output. The status enum admits only fail (a vacuous pass cannot ride a completed result - a scenario that passes makes the outcome incomplete, whose output is not held to this schema); evidence carries the assertion-level failure proof - a runner output excerpt or an observed-vs-expected contrast, long enough to be checked by a reader (minLength 1).
---

# e2e-baseline

Lay the acceptance bar: execute every scenario `e2e-author` authored
(`artifacts/e2e-scenarios.md` and/or the generated test files it produced under
`repos/<name>/**`) against the tree as it stands BEFORE `code` implements anything.
This is the e2e-first contract's first execution: a per-scenario semantic red here is
the legal completed output — the demonstrably-failing bar that `code` implements
against and that `e2e-run` re-runs as a hard gate after `code`.

## What to do

1. Read `artifacts/e2e-scenarios.md` (and any test files `e2e-author` recorded in
   `test_files`). The authored suite is read from the committed tree: `e2e-author`
   committed those files (the driver refuses its completion otherwise).
2. Copy `scenario_titles` verbatim from `execution.json`'s
   `steps.e2e-author.output.scenario_titles` into your `authored_titles` output field -
   element for element, same order, no additions and no omissions. The executor
   mechanically compares the two lists; they must be equal, and `scenarios` must carry
   exactly one entry per authored title.
3. Run each scenario against the pre-implementation tree — if a scenario has a runnable
   test file, execute it via `bash`; if it is only a structured GIVEN/WHEN/THEN
   document, drive the described flow through the project's actual interfaces (CLI,
   API calls, config) and judge the outcome against the THEN clause.
4. Do not fix, work around, or implement anything to make a scenario pass — this step
   observes and executes; it never edits the tree (`tools` are read and bash only).
   Every scenario that fails at the assertion level is recorded as a `fail` entry with
   `evidence`.
5. Record every scenario's outcome in `artifacts/e2e-baseline.md`, in the same order
   `e2e-author` declared them, so `code` (the next step) can read the bar it must
   clear.

**Run only the authored files.** Execute the scenarios `e2e-author` authored and nothing
else — never the project's full suite (`go test ./...`, `make test`, the full `./test`,
`npm test`) or unrelated packages; target the authored test files directly (e.g.
`go test ./test/ -run '<authored test names>'`). The full bar runs once, in `verification` (`deliver` re-runs that same bar only after merging a moved default branch changed the tree).

## When the run counts as laid (completed vs incomplete)

`outcome: completed` requires ALL of the following:

- the authored suite compiles and runs as authored — no build-level or wiring
  breakage;
- every scenario demonstrably RAN — each produced an observable outcome, not an
  assumption;
- every failure is semantic: an assertion-level red, recorded as one `fail` entry per
  scenario in the `scenarios` output array, each with `evidence` (runner output
  excerpt, observed-vs-expected).

Red is a legal workflow state between `e2e-author` and `code`, not a gate failure: a
completed baseline whose `scenarios` are all `fail` entries with evidence is the bar
working as designed — that document is exactly what `code` implements against.

A suite that is compile-broken, mis-wired, or otherwise un-runnable has NOT laid the
bar — nor has one whose scenarios pass vacuously against the pre-implementation tree
(a vacuous pass means the scenario does not actually probe the spec'd behavior,
usually because it was written against an imagined implementation). In either case
report `outcome: incomplete` with a precise explanation: which scenarios could not be
laid and why, and which scenarios passed vacuously and need re-authoring. Never
report `completed` with red results for an un-runnable or vacuous suite, and never
`gate_failed` — this step is non-gate (`gate: false` in the workflow frontmatter, no
`on_fail`; rule V5), so there is no gate verdict to fail with.

## Handoff

This step declares `handoff: true` in the workflow frontmatter: `code` (the next step,
`context_policy: inherit`) continues this session and implements against the
executable acceptance it just saw laid — the baseline document is the bar, and
`e2e-run` re-runs the same authored suite after `code` as the hard rerun gate.

## Output

Emit the final JSON object per the schema above: the baseline results document path,
the verbatim `authored_titles` echo (equal to `e2e-author`'s `scenario_titles`), and
one `fail` entry per authored scenario with assertion-level `evidence`. The schema admits
only `fail` in a COMPLETED baseline: if any scenario passes against the pre-implementation
tree, the bar is not laid — report `outcome: incomplete` (see above), listing the passing
scenarios (status `pass`) and why they need re-authoring; never `completed`.
