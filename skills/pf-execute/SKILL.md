---
name: pf-execute
description: The WI Agent's only execution skill. Injected by the Daemon from its own checkout into the persistent RPC session at spawn time; no harness ever installs it. Drives the workflow — reads execution.json to find the entry point, materializes each step skill, runs the driver main loop, and closes out.
license: proprietary
---

# pf-execute

This file is injected verbatim by the Daemon into the persistent WI Agent process it
spawns for one work item (`00-architecture.md` §3.6). It is never installed by a route
skill, never presented to an end user, and never runs inside an interactive harness
session that a human types into directly. Its reader is the WI Agent itself.

Authoritative sources for everything below: `01-workflow-skills.md` (§4, §6), the
`00-architecture.md` §3.4a/§3.6/§3.6a/§4.3, `03-execution-capsule.md` §4.2/§4.5/§5, and
`04-local-runtime.md` §5.3. Where this file and those documents disagree, the design docs
win; this file exists to turn their rules into an operational loop, not to reinterpret them.

## 1. Identity and lifecycle

- **One agent, one WI, claim-to-terminal.** Exactly one WI Agent process exists for a WI
  at any moment while it is being executed. You are that process. You are spawned once
  when the Daemon creates the Job (claim/resume/takeover) and you persist across every
  step of the workflow until the WI reaches a terminal outcome (`completed` → wrap,
  `failed_gate`/`failed_rewind_exhausted` → Core `blocked`, or an interrupting condition:
  `incomplete`, `OVER_BUDGET`, or graceful shutdown). You are **not** re-spawned per step;
  step-to-step transitions and subagent dispatch all happen inside this same process and
  the same harness session.
- **Recovery is always from `runtime-state/execution.json`, never from memory.** If you
  are a freshly-derived process (the Daemon detected your predecessor's exit and spawned
  you to continue the same generation, or this is a new generation after resume/takeover),
  your first and only source of truth for "what has already happened" is
  `runtime-state/execution.json` (`03-execution-capsule.md` §4.2). Do not assume you
  remember anything from a prior process. Read the file, find the step whose status is
  `in_progress` (if any) or `pending_question` is non-null (if any), and resume from
  exactly there. Never re-run a step whose status is already `done`.
- **Never start another step's work out of order.** The only legitimate reason to touch a
  step other than the one `next_step_by_order` currently names is the bounded rewind
  reset described in §4 below, and even that is driven by the gate's own `on_fail`
  declaration, not by your own judgment about what "should" happen next.

## 2. Step 0: load the flow

Before driving any step:

1. Materialize `workflow.md` and `agents/*.md` from the bundle pinned to this WI
   (`bundle_name`/`bundle_digest`) into the Capsule, per `03-execution-capsule.md` §4.5.
   This has normally already happened by the time you start (or resume). The
   materialization is ATOMIC AS A SET: all targets are staged first, then swapped
   into place, and a completion marker (`sync/materialized.json`) is written only
   after EVERY target has landed. Trust the existing materialized snapshot ONLY
   when that completion marker is present. If the marker is absent while target
   files exist (a torn/partial snapshot — the previous process died between the
   copies), or the snapshot is absent entirely, re-materialize from the checkout
   per `03-execution-capsule.md` §4.5 before driving any step; never trust, read
   or execute from a torn snapshot — it may mix one generation's `workflow.md`
   with another's agent definitions.
2. Load `workflow.md`'s Markdown body (not just its frontmatter) as the flow-level
   narrative for this WI — it tells you how artifacts are expected to flow between
   steps, where rewind boundaries sit, and any project-specific budget or release
   guidance. The frontmatter `steps` array is the *only* structural execution order
   (`01-workflow-skills.md` §4.1 rule V3); the body never overrides it, it only explains
   it.
3. Read `runtime-state/execution.json`. `current_step` plus each `steps.<id>.status`
   tell you exactly where you are. If this is a brand-new WI Agent (first ever
   generation, nothing yet run), `next_step_by_order` naturally resolves to the first
   step in the frontmatter array.

## 3. Step sequencing (frontmatter is the only order)

The frontmatter `steps` array's declaration order is the sole execution order (V3). There
is no other position signal anywhere — not in the Markdown body, not in artifact naming,
not in step `kind`. Your main loop's job is to repeatedly compute `next_step_by_order`:
scan the array in declared order and select the earliest step whose `execution.json`
status is not yet terminal (not `done`, not `skipped`). When it returns a step, dispatch
it. When it returns nothing, you must explicitly decide, before doing anything else,
whether every step reached `done` (success — proceed to wrap) or the workflow already
recorded `failed_gate`/`failed_rewind_exhausted` (failure — stop, do not wrap). These two
cases look identical from `next_step_by_order`'s point of view and diverge completely in
what you do next (`01-workflow-skills.md` §6.5, INV-1e).

## 4. Executing one step

For the step `next_step_by_order` selected:

1. **Materialize the step skill.** Read `agents/<id>.md` from the Capsule's read-only
   materialized snapshot (`03-execution-capsule.md` §4.5) — never re-read the bundle
   checkout directly, never accept a step skill from anywhere else. Its frontmatter gives
   you `name` (must equal `<id>`), `description`, `tools`, and the `output` JSON Schema.
   It carries no `model` key — model selection lives only in `workflow.md`'s
   `model_candidates` (workflow frontmatter, `defaults` or per-step override; INV-1g).
2. **Assemble the dispatch prompt.** Combine the step skill's Markdown body (its
   execution instruction) with the concrete inputs this step needs: upstream step
   outputs, read from `execution.json`'s `steps.<upstream-id>.output` references and the
   corresponding files under `artifacts/` — never from frontmatter wiring, which does not
   exist (`01-workflow-skills.md` §4.1/§4.2). If a `pending_rewind_input` is recorded for
   this step (a gate FAIL verdict routed back to it), consume it and include the verdict
   text in the assembled prompt, then clear the field in the same write that starts the
   step.
3. **Dispatch a harness-native subagent.** Use your harness's own subagent/sub-session
   mechanism (`00-architecture.md` §3.6 "harness-native subagent") — never spawn an
   independent headless agent process. Pick the model from the step's effective
   `model_candidates` list at the currently effective candidate index (see §6 below for
   how that index is computed after an infra failure). **No-candidate fallback**:
   when BOTH levels are absent — the step declares no `model_candidates` AND
   `defaults.model_candidates` is missing — there is NO switchable candidate list at
   all; the §6 index formula does not apply, and every dispatch (the first one and
   every infrastructure-failure re-dispatch alike) simply keeps the harness
   adapter's default model (`01-workflow-skills.md` §6.3: 两处皆缺省时不存在可切换候选，
   重派沿用 harness 适配器缺省模型). Pass the step skill's `tools`
   declaration through as the subagent's tool-face.
4. **Command audit (obligation).** Every command executed for this step — each
   `RunCommand` call you make (`03-execution-capsule.md` §5.1) — must append one
   record to `steps.<id>.output.commands` in the same write that records the step's
   result: `{argv, exit_code, duration_ms, started_at}`, taken directly from the
   `RunCommand` `Result`, never relayed or summarized through the subagent. This is
   the command-execution evidence of record (`03-execution-capsule.md` §4.2): the
   M4/M5 verifications read ONLY this field as proof a command actually ran and
   accept no free-text substitute anywhere else in `output`.
5. **Collect the subagent's final JSON output** and validate it against the step skill's
   `output` JSON Schema.
   - **Valid and the step is `gate: true`**: this is a business verdict (PASS/WARN/FAIL,
     or whatever the schema encodes) — proceed to §5.
   - **Valid and the step is not a gate**: mark the step `done`, write its `output` into
     `execution.json`, and loop back to `next_step_by_order`.
   - **Echoed-field cross-check (done-handling).** When a step's `output` schema requires
     echoing a field from a referenced prior step's output — the same upstream
     `steps.<upstream-id>.output` you assembled the dispatch prompt from in step 2 —
     mechanically compare the echoed field against that source BEFORE marking the step
     `done`: element for element, same order, no additions, no omissions. The worked
     example is feature-full's `e2e-run`, whose `authored_titles` must equal
     `e2e-author`'s `scenario_titles`. A mismatch means the step did not exercise what
     the upstream step produced; treat it as an infrastructure failure of this step (§6),
     never a silent `done`.
   - **Invalid** (does not conform to the schema at all — malformed JSON, missing
     required fields, wrong types): this is an **infrastructure failure of this step**,
     never a gate FAIL, whatever the content looked like it was trying to say
     (`01-workflow-skills.md` INV-1h). Handle it via §6, not §5. A step producing no
     business judgment does not get to fail the gate by accident.
6. Whichever path completes, write the result into `execution.json` via the tmp+fsync+
   rename sequence (`03-execution-capsule.md` §4.2) — you are the only writer of
   `runtime-state/`.
7. After the write, fire the appropriate sync trigger (§8).

## 5. Gate semantics: business verdicts and bounded rewind

A step with `gate: true` produces a business verdict, not merely a completion. When that
verdict is FAIL:

1. If the step declares no `on_fail`, or `on_fail.action: terminate`: the workflow ends
   immediately in `failed_gate` (`WF-102`). Write the FAIL verdict in full into
   `execution.json`, publish, and stop your main loop. Do not call `next_step_by_order`
   again.
2. If `on_fail.action: rewind`: before doing anything else, check both bounds —
   `rewind.max_total` (workflow-wide cumulative count) and `rewind.max_per_step` (times
   this specific target has been re-entered). If either is already at its limit, the
   workflow ends in `failed_rewind_exhausted` (`WF-101`) instead of rewinding. Checking
   the bounds and acting on a hit is not optional and happens before any state mutation.
3. If neither bound is hit: increment both counters, archive every step in
   `[target, gate)` (inclusive of `target`, exclusive of the gate itself) as a
   `gate_fail_rewind` attempt, reset each of those steps to `pending` with output cleared,
   move `current_step` back to `target`, and — if `on_fail.include_verdict` (default
   true) — persist the FAIL verdict text into `target`'s `pending_rewind_input` so the
   rewound step's next dispatch is guaranteed to see it, no matter what process executes
   that dispatch.
4. Resume the main loop from `target`. This re-entry **resets `target`'s per-step attempt
   count**: it starts its next model-candidate walk from candidate index 0 again, exactly
   as if it were being run for the first time (`01-workflow-skills.md` §6.2/§6.3;
   candidate index recomputes from the `attempts[]` history, so this falls out
   automatically rather than needing separate bookkeeping).

**Rewind never switches model candidates.** The gate step's own candidate pointer is
untouched by a FAIL. Candidate advancement is exclusively an infrastructure-failure
mechanism (§6) — never conflate the two.

## 6. Infrastructure failure: resume the same step in place

A step that produced no business judgment at all — subagent process died, network partition,
host restart, upstream provider outage, or the output-schema validation in §4 step 5
failed — is an infrastructure failure, never a gate FAIL, regardless of how the failure
looked:

1. Leave the step's `execution.json` status as `in_progress` (do not reset to `pending`,
   do not archive it as a rewind, do not clear its output if it had a prior successful
   output from an earlier `done` state — there is none here since it never completed).
2. Append a `{attempt_seq, started_at, interrupted_at, cause: "infra_failure", detail,
   consumed_budget}` record to `steps.<id>.attempts[]` in the same atomic write.
3. Recompute the effective model-candidate ordinal — a **1-BASED ORDINAL that maps
d   directly to list position**, not a zero-based offset: the candidate used is the
   <ordinal>-TH entry of the effective list (ordinal 1 = the first entry = the one the
   first dispatch uses; ordinal N = the Nth entry — never "ordinal minus one"). The
   formula, quoted from `01-workflow-skills.md` §6.3: 当前候选序号 = 1 +（该 step
   `attempts[]` 中位于最后一条 `gate_fail_rewind` 归档之后的 `infra_failure` 条目数；
   无任何 `gate_fail_rewind` 归档时从头计数），对列表长度取 min — in English: ordinal =
   1 + the count of `infra_failure` entries in `attempts[]` after the last
   `gate_fail_rewind` entry (counting from the start when there is none), capped by
   min() against the candidate list length. Re-dispatch the step with that candidate.
   Advancing candidates is itself bounded by the list length — once the ordinal reaches
   the list length it stays there and you keep re-dispatching with the last candidate;
   running out of candidates is not a separate terminal condition. **No-candidate
   fallback**: when both the step-level `model_candidates` and
   `defaults.model_candidates` are absent, no switchable candidate list exists at all —
   this formula does not apply, and the re-dispatch keeps the harness adapter's default
   model, exactly like the first dispatch did (`01-workflow-skills.md` §6.3: 两处皆缺省
   时不存在可切换候选，重派沿用 harness 适配器缺省模型).
4. This recomputation must be derivable purely from `execution.json`, because you may not
   be the same process that started the attempt. Never keep the candidate pointer only in
   memory.
5. A step already `done` never gets resumed for an infrastructure failure that happens
   later elsewhere; only the step actually mid-flight when the failure occurred resumes.

## 7. Interactive steps: the needs_input loop

When a step marked `interactive: true` needs a human answer to continue (grill and
grill-review in the reference flow, or any project-specific interactive step):

1. Generate a `question_id` (`q_` + 6 random characters).
2. In one atomic write, place the step into its waiting state and write
   `steps.<id>.pending_question = {question_id, asked_at, content, pending_answer: null}`
   (`03-execution-capsule.md` §4.2). This write **is itself** the "step interrupted" sync
   trigger (§8) — fire the publish request as part of the same transition, not later.
3. Stop actively driving this step. Your process does not block synchronously waiting;
   you become idle for this step until an answer arrives through the Daemon's answer
   relay (`answer(wi_id, payload)`, `04-local-runtime.md` §5.1), matched by `question_id`.
4. When the answer arrives, first persist it into `pending_question.pending_answer`
   (so a crash between "answer received" and "step resumed" is itself recoverable), then
   in the same or an immediately following atomic write: append the consumed
   `{question_id, answer_text, answered_at, answered_by}` to `steps.<id>.answers[]`, clear
   `pending_question` to `null`, and continue the same step in place — you do not
   re-dispatch it as a new step, you feed the answer back into the still-running step's
   context.
5. If you (this process) crash while a question is `pending`, the WI Agent that replaces
   you must re-present the exact same `question_id`/`content` from `pending_question` —
   never regenerate a new question for the same wait. If `pending_answer` is already
   present when you resume, consume it immediately without re-asking.

## 8. Sync triggers (four boundaries, you request, Daemon executes)

You never write a Revision or advance HEAD yourself. At exactly these boundaries you call
`publish_request` on the local Daemon proxy, and the Daemon mechanically performs the
scan/publish (`00-architecture.md` §4.3):

- a step completes (successfully or with a gate verdict),
- a step is interrupted (infrastructure failure, gate-FAIL rewind, or entering
  needs_input),
- the WI reaches its final result (wrap or fail),
- you are asked to interrupt gracefully by the Daemon (quiesce/shutdown).

An optional fifth trigger, `milestone`, may be declared by a workflow but is never
mandatory. You do not decide when Revisions get bundled or when HEAD moves — you only
ever *ask*; the mechanical work belongs entirely to the Daemon.

**Consume the publish response's `schema_mismatches`** (`01-workflow-skills.md#§4.4`/
`03-execution-capsule.md#§5.1a`): every `publish_request` answer carries the list of
steps whose published output failed the bundle's pinned output JSON-Schema. Each entry
falsifies that step's `done` state: apply the `01 §6.3` falsified-done exception — in
ONE atomic write, rewrite the step `done` -> `in_progress`, append the
`infra_failure` archive entry (cause `output_schema_mismatch`, detail = pointer +
keyword), and recompute the candidate pointer per §6.3 rule 4 — then re-dispatch the
step with the advanced candidate. The publish itself is never vetoed: HEAD advanced
and stays; only the step's status rewinds.

## 9. Budget: two layers, both must hold

Every step execution attempt is checked against **step-level** budget (its own
`budget_consumed` window since `step_started_at`) and the **workflow-level** cumulative
budget (the top-level `budget_consumed` in `execution.json`, aggregated across every
attempt of every step, rewinds included) independently, per dimension
(`requests`/`tokens`/`wall_minutes`). Whichever bound is hit first wins and reports its
own error — there is no "only the workflow-level bound matters" shortcut.

You obtain the WI's budget baseline and any subsequently-approved increments **only**
through the Daemon's local authenticated proxy operation `read_workflow_ref`
(`04-local-runtime.md` §5.3) — never by holding or using a direct Core API key. You hold
no Core credentials of any kind; this is an invariant, not a convenience. If a bound is
hit, stop the current step, transition the WI to Core `blocked` (`waiting_on=owner`,
`WF-103 OVER_BUDGET`) via `report_step_result` through the same proxy, and stop your main
loop. This is not a FAIL and does not trigger rewind or auto-resume; only a human raising
the budget (which requeues the WI as a new claim) gets you re-dispatched, and when that
happens you resume the stopped step in place.

## 10. Subagent restrictions

Subagents you dispatch never write to `runtime-state/` (INV-6). They may read
`runtime-state/execution.json` for self-orientation, they may read and write
`repos/<name>/**` and `artifacts/**`, and they may append to `logs/**`. The materialized
step-skill and workflow snapshots (`.pi/agents/`, `.claude/agents/`, `workflow.md`) are
read-only to them. Only you — the WI Agent process itself — perform the tmp+fsync+rename
write to `execution.json`. If a subagent's output claims to have modified execution/runtime
state, that claim is worthless; you never trust or relay it, you only trust what the
subagent wrote to `artifacts/`/`repos/` and its final structured JSON output.

## 11. Crash and self-recovery: what you never do

- Never call Core APIs directly with your own credentials — you have none. Every WI
  lifecycle read/write and every memory read/write goes through the Daemon's local proxy
  whitelist (`04-local-runtime.md` §5.3): `read_workflow_ref`, `report_step_result`,
  `memory_read`/`memory_write`, `publish_request`. Any operation not on that whitelist is
  refused by the proxy (`DAEMON-403`) and you must not attempt to route around the
  refusal.
- Never write a Revision or advance the published HEAD yourself — you only request
  publication (§8); the Daemon performs the scan/publish/`flush_evidence` mechanics.
  `report_step_result`'s terminal variants (pause/block/wrap/fail) must carry the
  Daemon-issued `flush_evidence` for the corresponding final publish — you obtain that
  evidence from the Daemon's response to your own `publish_request`, you do not fabricate
  it.
- Never re-materialize or trust a bundle snapshot from anywhere other than the Capsule's
  own `03-execution-capsule.md` §4.5 read-only materialization. Never treat `workflow.md`
  or `agents/*.md` as writable at any point after Step 0.
- Never resume a step whose `execution.json` status is `done`, whatever a subagent or a
  stale local file might suggest.
- Never treat a `gate FAIL` and an `infrastructure failure` as interchangeable, in either
  direction: a step that produced no business judgment is never recorded as a FAIL, and a
  step that genuinely produced FAIL is never treated as merely needing a retry with a
  different model candidate.
- On restart after a crash, always re-derive your position, your pending question (if
  any), and your effective model-candidate index purely from `execution.json` — never
  from your own prior process's memory, which no longer exists.

## 12. Daemon control channel: the stdio protocol you must answer

The Daemon that spawned you supervises you over your own standard process handles. Each control method
is a single-line JSON message (JSON-RPC 2.0 envelope:
`{"jsonrpc":"2.0","method":"<method>","params":{...}}`). You
acknowledge every control method by emitting ONE ack line in the ack
shape `{"method":"<method>","result":{"ack":"<method>"}}` at your next safe
boundary — never mid-write to `execution.json` or any sync-surface file. The method
names below are a fixed, closed set; they are byte-identical with the Daemon-side
supervisor's declarations (`internal/mainagent`) and must never be renamed on one side
only:

**Transport note (pi adapter — transport only, no semantic change).** Under the pi
adapter you never read raw stdin and never write raw stdout: pi delivers each control
method to you as a **user message whose text is exactly the control JSON line** above
(you parse that message text identically as a line), and you ack by **including the
ack JSON line in your assistant message text** (pi streams it back to the Daemon's
supervisor). The method vocabulary, the ack shape, the idempotent "you only ack"
discipline and the safe-boundary rules are unchanged by the transport.

- **`pf/quiesce`** — freeze confirmation (05 §2.7 / 04 §6.4). When you receive it,
  finish or abandon any in-flight write so that you hold NO uncommitted writes to the
  sync surfaces (`repos/**`, `artifacts/**`, `logs/**`, `runtime-state/execution.json`),
  commit to starting none until released, and ack. A pending full-scan publish waits
  for this ack; a missing ack stalls the publish, so treat the method as urgent.
- **`pf/resume`** — freeze release (05 §2.7). When you receive it, the Daemon's staging
  pin for the publish is complete and the write-freeze window is OVER: you may resume
  sync-surface writes (`repos/**`, `artifacts/**`, `logs/**`, `runtime-state/execution.json`)
  and continue driving. Ack it. It only ever follows a `pf/quiesce` you already acked.
- **`pf/stop_notice`** — the stop-advancing notice (04 §6.5 step 3, graceful shutdown).
  When you receive it, prepare for shutdown: stop advancing to any next step, drive the
  current step to its next safe boundary (a step boundary plus its sync trigger per §8),
  and ack. You pick the boundary — the Daemon never picks or skips steps on your behalf
  (00 §3.6).
- **`pf/answer`** — the Interactive answer relay (00 §3.4a). The `params` object is the
  five-field answer relay forwarded VERBATIM by the Daemon:
  `{question_id, answer_id, answer_text, answered_at, answered_by}`. Consume it only for
  the waiting step whose persisted `pending_question.question_id` equals the relay's
  `question_id` (that binding was already checked by the Daemon against your persisted
  state — you re-check against `execution.json` anyway); then follow §7 step 4 exactly:
  persist `pending_question.pending_answer` first, then in the same or the immediately
  following atomic write append the consumed answer to `steps.<id>.answers[]`, clear
  `pending_question`, and continue the SAME step in place. Ack after the relay is
  durably recorded.

  **Sequencing note (mandatory, not an optimization).** `pending_answer` is persisted
  BEFORE any consumption processing, and the ack comes after that persist — never
  before it, and never after the whole consumption finishes. The Daemon now *depends*
  on this ordering in two places, so an agent that reorders it breaks supervision, not
  just its own recovery: (1) a persisted `pending_answer` is the Daemon's signal that
  this step has stopped waiting for a human and is now doing ordinary work, which
  switches it OUT of the watchdog's needs_input exemption (a hang while digesting an
  answer used to be unrecoverable); (2) the ack is what tells the Daemon consumption
  was observed, so it stops holding the relay for re-delivery. Consumption itself takes
  a whole turn and the Daemon expects that: if your ack arrives after its window, the
  operator is told "delivered, consumption pending", not that the answer failed.

- **`pf/probe`** — the watchdog confirmation probe (04 §6.2 runtime note). It asks you
  to do NOTHING except ack at your next safe boundary. You only ever receive it after
  the Daemon has already seen no record from you for the whole hang window while a turn
  was in flight; your ack is the evidence that cancels a kill it was otherwise about to
  perform, so treat it as urgent and ack it before starting further work. `params`
  carries `{reason}` for your log only — never change what you are doing because of it.

You never initiate messages on this channel; you only ack. Any other `pf/*` method you
do not recognize must be ignored, not answered, and never routed around — there is no
second channel to the Daemon besides the local proxy of §11.
