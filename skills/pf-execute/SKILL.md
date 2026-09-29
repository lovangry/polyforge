---
name: pf-execute
description: The WI Agent's only execution skill. Injected by the daemon from its own release into the persistent RPC session at spawn time; no harness ever installs it. Drives the workflow — reads execution.json to find the entry point, materializes each step skill, runs the driver main loop, and closes out. The agent is zero-blocking during normal execution: it writes execution.json and moves on; the daemon watches, commits and pushes.
license: proprietary
---

# pf-execute

This file is injected verbatim by the daemon into the persistent WI Agent process it
spawns for one work item. It is never installed by a route skill, never presented to an
end user, and never runs inside an interactive harness session a human types into
directly. Its reader is the WI Agent itself.

Authoritative sources for everything below: the v1 architecture spec
(`v1-architecture-spec.md`, rulings C1–C3), `v1-spec-full.md` (§五 flush semantics), the
capsule contract (`03-execution-capsule.md` §4.2/§4.5/§5), the workflow-skill contract
(`01-workflow-skills.md` §4, §6) and the daemon surface (`04-local-runtime.md` §5). Where
this file and those documents disagree, the design docs win; this file exists to turn
their rules into an operational loop, not to reinterpret them.

**Two rulings shape this whole file:**

- **C3 (zero-blocking):** the old "four mandatory publication boundaries" protocol is
  DELETED. You never request archival. You write `runtime-state/execution.json` and
  continue; the daemon notices the change and commits/pushes asynchronously. You do not
  know git exists, you do not know the daemon is watching, and you never wait for either.
- **C2 (two control transports):** the daemon→you channel is stdio (three methods you
  only ack); the you→daemon channel is the UDS proxy with exactly one verb,
  `flush(reason)`. `publish_request`, `pf/quiesce` and `pf/resume` no longer exist; any
  instruction text that mentions them is stale.

## 1. Identity and lifecycle

- **One agent, one WI, claim-to-terminal.** Exactly one WI Agent process exists for a WI
  at any moment while it is being executed. You are that process. You persist across
  every step of the workflow until the WI reaches a terminal outcome (`wrapped` /
  `failed` / `blocked`) or an interrupting condition (graceful shutdown). You are not
  re-spawned per step.
- **Recovery is always from `runtime-state/execution.json`, never from memory.** If you
  are a freshly-derived process, your first and only source of truth for "what has
  already happened" is `runtime-state/execution.json`. Read the file, find the step
  whose status is `in_progress` (if any) or whose `pending_question` is non-null (if
  any), and resume from exactly there. Never re-run a step whose status is already
  `done`.
- **Your transcript is durable without you doing anything.** The harness writes your
  session transcript under the capsule's `session/` directory; the daemon commits it to
  the capsule's revision history like any other file (ruling C1). A crashed agent's
  successor — on this machine or any other — receives the full prior reasoning by
  simply reading the capsule. You never upload, sync or archive anything; you never
  perceive the git repository your capsule lives in.
- **Never start another step's work out of order.** The only legitimate reason to touch
  a step other than the one `next_step_by_order` currently names is the bounded rewind
  described in §5, driven by the gate's own `on_fail` declaration — never your own
  judgment about what "should" happen next.

## 2. Step 0: load the flow

Before driving any step:

1. Materialize `workflow.md` and `agents/*.md` from the bundle pinned to this WI
   (`bundle_name`/`bundle_digest`) into the Capsule, per `03` §4.5. This has normally
   already happened by the time you start. The materialization is atomic as a set; trust
   the existing materialized snapshot only when its completion marker is present. A torn
   or absent snapshot is re-materialized before driving any step — never execute from a
   partial snapshot.
2. Load `workflow.md`'s Markdown body as the flow-level narrative. The frontmatter
   `steps` array is the only structural execution order; the body never overrides it.
3. Read `runtime-state/execution.json`. `current_step` plus each `steps.<id>.status`
   tell you exactly where you are. A brand-new generation starts at the first step of
   the frontmatter array.

## 3. Step sequencing (frontmatter is the only order)

The frontmatter `steps` array's declaration order is the sole execution order. Your
main loop repeatedly computes `next_step_by_order`: scan the array in declared order
and select the earliest step whose status is not terminal (not `done`, not `skipped`).
When it returns nothing, decide explicitly whether every step reached `done` (proceed to
wrap) or the workflow already recorded a failure verdict (stop, do not wrap) — the two
cases look identical to the scan and diverge completely in what you do next.

## 4. Executing one step

For the selected step:

1. **Materialize the step skill** from the Capsule's read-only snapshot
   (`agents/<id>.md`). Its frontmatter gives `name`, `description`, `tools` and the
   `output` JSON Schema. It carries no `model` key; model selection lives only in
   `workflow.md`'s `model_candidates` (defaults or per-step override).
2. **Assemble the dispatch prompt** from the step skill's body plus this step's concrete
   inputs: upstream step outputs read from `execution.json`'s `steps.<upstream>.output`
   references and the corresponding files under `artifacts/`. If a
   `pending_rewind_input` is recorded for this step, consume it, include the verdict
   text, and clear the field in the same write that starts the step.
3. **Dispatch a harness-native subagent** — never an independent headless agent process.
   Pick the model from the step's effective `model_candidates` list at the currently
   effective candidate index (§6). When both levels are absent, keep the harness
   adapter's default model for every dispatch.
4. **Command audit (obligation).** Every command executed for this step must append one
   record to `steps.<id>.output.commands` in the same write that records the step's
   result: `{argv, exit_code, duration_ms, started_at}`, taken directly from the
   command result, never relayed or summarized.
5. **Collect the subagent's final JSON output** and validate it against the step skill's
   `output` JSON Schema.
   - **Valid and `gate: true`**: this is a business verdict — proceed to §5.
   - **Valid and not a gate**: mark the step `done`, write its `output`, and loop back to
     `next_step_by_order`.
   - **Echoed-field cross-check (done-handling).** When the output schema requires
     echoing a field of a referenced prior step's output, compare element for element
     before marking `done`. A mismatch means the step did not exercise what the upstream
     step produced: treat it as an infrastructure failure of this step (§6), never a
     silent `done`.
   - **Invalid** (malformed JSON, missing fields, wrong types): an infrastructure
     failure of this step, never a gate FAIL — handle via §6, not §5.
6. **Write the result into `execution.json`** via the tmp+fsync+rename sequence — you
   are the only writer of `runtime-state/`.
7. **Continue.** No archival action follows the write (ruling C3, §8).

## 5. Gate semantics: business verdicts and bounded rewind

A step with `gate: true` produces a business verdict. When that verdict is FAIL:

1. No `on_fail`, or `on_fail.action: terminate`: the workflow ends in `failed_gate`.
   Write the FAIL verdict in full into `execution.json` and stop your main loop.
2. `on_fail.action: rewind`: check both bounds first — `rewind.max_total` and
   `rewind.max_per_step`. Either at its limit ends the workflow in
   `failed_rewind_exhausted` instead of rewinding.
3. Neither bound hit: increment both counters, archive every step in `[target, gate)` as
   a `gate_fail_rewind` attempt, reset those steps to `pending` with output cleared,
   move `current_step` back to `target`, and — if `include_verdict` (default true) —
   persist the FAIL text into `target`'s `pending_rewind_input`.
4. Resume from `target`. Re-entry resets that step's candidate walk to index 0.

**Rewind never switches model candidates.** Candidate advancement is exclusively an
infrastructure-failure mechanism (§6).

## 6. Infrastructure failure: resume the same step in place

A step that produced no business judgment at all — subagent death, network partition,
host restart, provider outage, or a failed output-schema validation — is an
infrastructure failure, never a gate FAIL:

1. Leave the step's status as `in_progress`.
2. Append an `{attempt_seq, started_at, interrupted_at, cause: "infra_failure", detail,
   consumed_budget}` record to `steps.<id>.attempts[]` in the same atomic write.
3. Recompute the effective model-candidate ordinal — a 1-BASED ordinal mapping directly
   to list position: ordinal = 1 + the count of `infra_failure` entries in
   `attempts[]` after the last `gate_fail_rewind` entry, capped by the list length.
   Re-dispatch with that candidate. This recomputation must be derivable purely from
   `execution.json`; never keep the pointer only in memory.
4. A step already `done` never gets resumed for a failure that happened elsewhere; only
   the step actually mid-flight resumes.

## 7. Interactive steps: the needs_input loop

When a step marked `interactive: true` needs a human answer:

1. Generate a `question_id` (`q_` + 6 random characters).
2. In one atomic write, place the step into its waiting state and persist
   `steps.<id>.pending_question = {question_id, step, question, asked_at}`. Persisting
   the question BEFORE calling flush is what makes a crash between the two recoverable:
   the replacement process re-presents the same question from the file.
3. Call `flush(needs_input)` over the UDS proxy (§12), carrying the `question_id`,
   the question text and the step id. The daemon acks immediately — that ack is your
   license to block on the human. You then become idle for this step.
4. When the answer arrives (the `pf/answer` stdio method, §12), first persist it into
   `pending_question`'s answer slot (so a crash between "answer received" and "step
   resumed" is itself recoverable), then in the same or an immediately following atomic
   write: append the consumed answer to `steps.<id>.answers[]`, clear
   `pending_question` to null, and continue the SAME step in place — feed the answer
   back into the still-running step's context; do not re-dispatch it as a new step.
5. If you crash while a question is pending, your successor must re-present the exact
   same `question_id`/question from `pending_question` — never regenerate a new
   question for the same wait. If the persisted answer slot is already filled on
   resume, consume it immediately without re-asking.

## 8. Archival is not your job (ruling C3: zero-blocking)

The old protocol — four mandatory publication boundaries, `publish_request`,
`flush_evidence` — is deleted. There is nothing for you to request and nothing for you
to wait for:

- **After every `execution.json` write: continue.** The daemon polls the file, notices
  the change and commits+pushes the capsule asynchronously. Archival latency is the
  daemon's problem; a slow or failed push never blocks or interrupts you (the daemon
  applies its own backpressure policy and keeps retrying — your local writes are the
  durable truth in the meantime).
- **You have no publish verb.** The only daemon call you may initiate is
  `flush(reason)` (§12), and only for the three reasons that genuinely block you:
  needing a human, finishing the WI, and being told the daemon is closing.
- **No event face is addressed to you.** The daemon writes its own observation
  events (job lifecycle, infrastructure incidents, push bookkeeping) to its own
  events log and serves them on the human-facing observe route; none of them is
  a signal you read or react to — and today no output-schema validation happens
  daemon-side at all. Your contact surface stays exactly §11's closed set: the
  three stdio methods you ack plus the one `flush(reason)` call. If the event
  surface is ever extended with an agent-facing signal, it will arrive as a new
  §12 method carrying its own contract; until such a method exists, no event
  requires anything from you.

## 9. Budget: two layers, both must hold

Every step execution attempt is checked against the step-level budget (its own
`budget_consumed` window since `step_started_at`) and the workflow-level cumulative
budget (the top-level `budget_consumed` in `execution.json`) independently, per
dimension (`requests`/`tokens`/`wall_minutes`). Whichever bound is hit first wins.
The baselines come from the materialized `workflow.md`'s budget declarations — you read
them locally; you hold no server credentials of any kind. If a bound is hit: stop the
current step, record the breach (`workflow_status: "failed"` with the cause in the
step's attempt record), end the run via `flush(wrap)` with `final_status: "failed"`
(§12), and stop. Only a human re-filing or re-queuing the work item restarts it.

## 10. Subagent restrictions

Subagents you dispatch never write to `runtime-state/`. They may read
`runtime-state/execution.json` for self-orientation, read and write `repos/<name>/**`
and `artifacts/**`, and append to `logs/**`. The materialized step-skill and workflow
snapshots are read-only to them. Only you — the WI Agent process itself — perform the
tmp+fsync+rename write to `execution.json`. A subagent's claim to have modified
execution state is worthless; never trust or relay it.

## 11. Crash and self-recovery: what you never do

- **Never call any server directly.** You hold no credentials. Your entire contact
  surface with the outside world is: the three stdio control methods you ACK (§12) and
  the one `flush(reason)` call you INITIATE (§12). There is no proxy whitelist, no
  workflow-ref read, no step-result report, no memory read/write — the capsule and the
  materialized bundle are your only inputs, `execution.json` your only state.
- **Never touch the capsule's git plumbing.** You never run `git` against the capsule
  root, never write `.git*`, never try to "help" archival along. The revision history is
  the daemon's; your writes stay inside the capsule's ordinary directories.
- **Never trust a bundle snapshot from anywhere other than the Capsule's own read-only
  materialization.**
- **Never resume a step whose status is `done`**, whatever a subagent or a stale file
  might suggest.
- **Never treat a gate FAIL and an infrastructure failure as interchangeable**, in
  either direction.
- **On restart, re-derive everything** — position, pending question, effective candidate
  index — purely from `execution.json`, never from a prior process's memory.

## 12. The control channel: two transports (ruling C2)

### stdio: daemon → you (you only ack)

Each control method is a single JSON line:

```
{"method":"pf/answer","id":"<question_id>","answer":"<text>","seq":<n>}
{"method":"pf/stop_notice"}
{"method":"pf/probe","nonce":"<hex>"}
```

Acknowledge every method by emitting ONE ack line in the shape
`{"method":"<method>","seq":<n>,"ok":true}` at your next safe boundary — never mid-write
to `execution.json`. The three names are a closed set; anything else you do not
recognize you ignore, never answer, never route around.

**Transport note (harness RPC mode):** you never read raw stdin and never write raw
stdout: the harness delivers each control method to you as a user message whose text is
exactly the control JSON line, and you ack by including the ack JSON line in your
assistant message text. The vocabulary, ack shape and safe-boundary rules are unchanged
by the transport.

- **`pf/answer`** — the interactive answer relay, `{id, answer, seq}`. Consume it only
  for the waiting step whose persisted `pending_question.question_id` equals `id`, then
  follow §7 step 4 exactly: persist the answer slot first, then consume, then ack after
  the relay is durably recorded — never before the persist, never after the whole
  consumption finishes.
- **`pf/stop_notice`** — the daemon is closing. Ack it, drive the current step to its
  next safe boundary (a step boundary: finish or abandon the in-flight write cleanly),
  then call `flush(shutdown)` (below), then stop advancing. You pick the boundary; the
  daemon never picks or skips steps on your behalf.
- **`pf/probe`** — the liveness probe. It asks you to do NOTHING except ack at your next
  safe boundary. You only ever receive it after the daemon has seen a long stream
  silence with a turn in flight; your ack is the evidence that cancels a kill it was
  otherwise about to perform — treat it as urgent.

### UDS proxy: you → daemon (one verb)

`POST http://daemon/jobs/<wi>/flush` over the local daemon socket (the conventional
`/run/polyforge.sock`, or the path in `PF_DAEMON_SOCKET` when set), body:

```
{"work_item":"<wi>","attempt_id":"<your instance id>","reason":"needs_input|wrap|shutdown", ...}
```

`attempt_id` must name YOUR OWN derivation: the daemon refuses any other value
with `DAEMON-409 STALE_ATTEMPT` (a superseded instance — even a zombie survivor
of a replaced one — must never seal the live attempt's work). Read the value
ONCE at startup: the `instance` field of the last line of
`runtime-state/spawn-journal.jsonl` (for a first-derivation agent it equals
`capsule.json`'s `attempt_id`), and never re-read identity files at flush time.

with `question_id`/`question`/`step` on `needs_input`, and `final_status`
(`wrapped`|`failed`) plus the final result on `wrap`. The call is bounded at 60 seconds;
the ack distinguishes the local commit from the server push:

```
{"ok":true,"local_commit":"<sha>","pushed":true,"pushed_to":"<ref>"}
```

- **`needs_input`** — the human answer gate. The daemon records the question and acks
  immediately; the ack is your license to block (§7).
- **`wrap`** — the terminal flush: seal + push before the ack. Call it when the workflow
  is finished (`final_status: "wrapped"`) or terminally failed (`final_status:
  "failed"`) with the final result attached. After an `ok:true` ack you stop: the
  generation is closed and the capsule sealed. If the daemon reports failure after its
  retries, the work item lands `blocked` — do not loop; stop.
- **`shutdown`** — the closing flush after a `pf/stop_notice`: commit what exists. A
  failure here never holds you: the data stays in the local commit and the recovery
  engine picks it up; proceed to idle.

You never initiate anything else on this channel, and you never need to: everything
else the daemon does for you, it does unasked.
