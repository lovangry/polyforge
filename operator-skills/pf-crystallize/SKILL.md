---
name: pf-crystallize
description: >
  Use when a wi has just been wrapped via /pf-stop --wrap and its operation flow should
  be captured as reusable, versioned SKILL BUNDLES for future wis of the same type.
  Dispatched automatically by pf-stop --wrap, not invoked directly by the user.
---

# pf-crystallize - Workflow Crystallization

## Usage

**Purpose**: Crystallize a just-wrapped wi's workflow into the **aihub skill registry**
(aihub#708): per-step bodies as CLOSED, VERSIONED bundles (entry + files + license +
provenance + runtime capability contract), published **private by default**. The
scenario-file PR below remains as the LEGACY compatibility branch.

**Pattern**: `/pf-crystallize <source_wi_id> <wi_type_name> [--project <name>]`

**Required**: `<source_wi_id>` (the wrapped wi) and `<wi_type_name>` - input-accept only
names matching the registry's name rule `^[a-z][a-z0-9-]{0,63}$` (lowercase; an
underscore-containing or uppercase entry must be normalized - lowercased, underscores to
hyphens, e.g. `code_change` -> `code-change` - before publication)

**Flags**:
- `--project <name>` - LEGACY branch only (project-scoped scenario file); ignored on the
  registry path (registry skills are owned and shared explicitly, never project-named)
- Not user-invokable directly - dispatched by `/pf-stop --wrap` after the user opts in

## When to use

Triggered automatically by `/pf-stop --wrap` after wrap completes. Input arguments:
- `source_wi_id`: the wi ID that was just wrapped
- `wi_type_name`: the new wi_type name entered by the user

Not for direct user invocation.

## Mechanic

### Step 1: Guiding questions

Collect:
- `wi_type`: from the trigger input, normalized to the registry name rule
  `^[a-z][a-z0-9-]{0,63}$` (lowercase; underscores -> hyphens) - anything else would pass
  a loose input guard and then fail publication at the registry CHECK
- `requires_human_session` (default false): does the captured flow need a human?
- License + provenance of the captured bodies:
  - derived from GMI's own skill bodies / scenario steps -> license `Proprietary` with a
    `source` provenance naming the origin path;
  - derived from MIT-licensed upstream content -> license `MIT` and PRESERVE the upstream
    notice (the bundle is what gets shared; an unlabelled bundle is a redistribution
    hazard).

**Early-exit guard**: if the user decides not to crystallize at this point (enters
"skip" / "no" / presses Enter), immediately output "Crystallization skipped." and end
without performing the subsequent steps.

### Step 2: Extract the step sequence

Combine two information sources:

**2a. pf_read_events (structured timeline)**
```
events = pf_read_events(
  work_item_id=source_wi_id,
  limit=100
)
```
Extract the steps by lifecycle. Legacy `step_completed` events carry `artifact_summary`;
workflow-v2 result events carry `artifact` + `evidence` plus the workflow identity
(`steps_version`, `step_attempt_id`) - read `artifact_summary` for legacy rows and
`artifact`/`evidence` for workflow-v2 rows, never one field for both.

**2b. AI in-context window**
If invoked within the same session, supplement the details missing from pf_read_events.
> Across sessions the in-context window is unavailable; rely on pf_read_events only, and
> output quality depends on how rich the events are.

Combining both, extract an ordered step list (each `## Step: <id>` in the legacy grammar;
on the registry path each step becomes one bundle).

### Step 3: Author and publish the registry bundles (aihub#708)

For each captured step:

1. **Author the body** - one self-contained prompt for the step's role (see the seeded
   reference set: grill-me / spec / plan / code-change / review / verification / ship /
   ci under `internal/skillregistry/seed.go`). Match the closed-bundle rules: the file
   set is exactly what you list, no runtime-fetch instructions, no symlinks, paths
   relative and clean.
2. **Declare the contract** - capabilities from the step's ROLE, closed vocabulary:
   authoring (produce), review / verification (gates), shipping, deterministic_operation.
   Gates must be their own steps, never mixed into a producer; the server derives grants
   from exactly these values and refuses a mixed gate+producer contract.
3. **Publish as a new skill version** - through the skill registry's authorized surface
   (`CreateSkill` + `PublishSkillVersion` in `pkg/client/skills.go`, the same calls the
   server's routes back), carrying:
   - the closed bundle (entry + files + license + provenance),
   - the contract,
   - `expected_latest` - the CAS token (0 for a brand-new skill); on 409 CAS, re-read and
     retry, never force,
   - `content_digest` - the content digest of (bundle, contract), so the publish is
     content-verified,
   - a **deterministic name**: the wi_type or the step-role name, matching
     `^[a-z][a-z0-9-]{0,63}$` (the registry's name rule).

   **Idempotency**: before publishing, compare the new content digest against the skill's
   existing version digests; a match means the content is already published - pin the
   matching version and publish nothing. Re-running crystallization over an unchanged
   capture must be a no-op, not a version bump.

   **Surface status (honest, as of Batch 4A)**: the registry's write surface exists as
   server routes + client functions; the MCP skill tools (`pf_create_skill`,
   `pf_publish_skill_version`...) do NOT exist yet, so an in-session agent cannot perform
   this step through MCP today. If you were dispatched to crystallize and the publication
   surface is unavailable, SAY SO and stop - do not fall back to raw HTTP (IR3) and do not
   silently degrade to the legacy branch. The MCP skill surface is tracked in
   docs/workflow-v2/04-unresolved-migration-references.md.

4. **Privacy is the default, sharing is explicit** - every published version is PRIVATE
   (the server enforces this; there is no publish-as-public path). Making a version
   reachable by others is a separate, deliberate act (`ShareSkillVersionWithProject` /
   `SetSkillVersionVisibility`), per exact version, revocable, and NEVER something
   crystallize does on its own. The captured bodies may quote worktree-derived content -
   treat them as confidential until the owner decides otherwise.

### Step 4: Record the recommended composition

The flow itself is NOT stored in the registry - flows are per-work-item generations. Record
the recommended composition as data the next author can pin:

```
pf_emit_event(
  work_item_id=<the live crystallize chore wi>,
  event_type="note",
  payload={
    "text": "crystallized <wi_type>: recommended flow",
    "flow": [
      {"step": "<id>", "skill_id": "<skill>", "skill_version": <exact version>},
      ...
    ]
  }
)
```

`pf_emit_event` authenticates from the state file of a LIVE claim, so record the composition
on a crystallize chore wi that is currently claimed - never on the wrapped source wi, which
is terminal and has no state file. If crystallize runs in-session BEFORE the source wi is
wrapped, emitting on the source attempt while its credentials are still live is also legal.

Pin **exact versions** (never `skill_version: 0`, the latest-accessible convenience):
composing this flow onto a future wi is an explicit `pf_update_workflow` at compose time
with `expected_steps_version` as its CAS token - there is no automatic fallback that
re-pins "the" flow for a wi_type. No silent latest: an unpinned version drifts; an exact
pin is reproducible.

### Step 5 (LEGACY): scenario-repo branch - kept for fleet compatibility

The pre-registry mechanic: open the crystallize CHORE wi (`/pf-work` silent mode, then
`/pf-work <slug>` to claim it) so every file write happens inside its claimed worktree
(IR1) - then write `{wi_type}[.{project}].md` (frontmatter `requires_human_session`,
`## Step:` sections, `@include: common/...` for shared bodies, common-skill extraction as
before) into the polyforge-coding worktree and ship via the chore wi's own claim - `pf_ship`
requires all of `work_item_id`, `repo`, `message`, `pr_title`, `pr_body` (plus the
optional `paths`/`pr_base`):
`pf_ship(work_item_id=<chore wi>, repo="polyforge-coding", message="crystallize <wi_type>",
paths=[the new .md files only], pr_title="Add <wi_type> step graph",
pr_body="<what the flow captures>", pr_base="main")`. Present the DRAFT for
confirmation before writing (Enter=confirm / notes=revise / cancel=skip), and keep the
pf-execute-compatible format: frontmatter only `requires_human_session`; `## Step: <word>`
at strict line start; `@include:` level params on the immediately following line; a
`## Step:` inside a code fence is not a step. **This branch remains exactly as before** for machines and wi_types still driven by scenario step graphs - see
docs/workflow-v2/01-authority-and-compatibility.md for which path a wi takes. When in
doubt, ASK which product the user wants; the registry path is the new authority, the
scenario file is compatibility, and writing both unasked duplicates the flow.

### Step 6: Finish

- Registry path: report the published (skill, version) pins and the recorded composition;
  suggest sharing as the explicit next act if the owner wants it visible to others.
- Legacy path: "Crystallized `{filename}` and opened PR `<url>`; takes effect on other
  machines after the PR merges and they run `polyforge init`."
- Then call `/pf-stop --wrap` to finish the crystallize chore wi (if one was opened).

### Step 7: User cancel path (cancel at any point before publication)

If the user cancels before anything was published or written:
- Do not publish or write anything
- Call `/pf-stop --fail` to terminate the already-claimed crystallize wi (reason: "user
  cancelled crystallization")
- Output: "Crystallization skipped."

## NL Triggers

- Invoked automatically by `pf-stop --wrap`, passing `source_wi_id` and `wi_type_name`
