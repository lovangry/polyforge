# Upgrade guide

This document describes how an aihub database moves from an old schema to
a new one. The upgrade is executed by `.polyforge/deploy.sh`; there is no
startup auto-migration (`Open` installs the schema with
`CREATE TABLE IF NOT EXISTS` only), and the new server does **not** work
on an un-upgraded database: the old `projects.identifier_hash` column is
`TEXT NOT NULL` without a default and the new code no longer writes it,
so creating a project fails until the upgrade SQL has run.

## The deploy.sh flow (seconds-level downtime)

- **Online backup before the window**: rsync of `memory/` and `git/`, a
  SQLite backup-API snapshot of `aihub.db` (consistent even under WAL
  writes), and an off-host copy. The service keeps running throughout.
  Once that backup has verified (step 5), deploy.sh writes a `.verified`
  marker into it on both sides, keeps the newest 3 verified
  `aihub-backup-<version>-<timestamp>` dirs (this run's included) on the
  server and in `BACKUP_ROOT`, prunes older verified ones and prints what
  it pruned; `--dry-run` prints what it would prune. A backup dir without
  the marker — a failed run's, and **every backup made before this
  change** — is never counted and never pruned: each run lists them as
  "unverified, kept; remove by hand". Delete the pre-marker ones yourself
  once a marked backup exists. Nothing else is ever touched. The backup
  never copies `bootstrap-admin.key` (a one-time handoff file; the key is
  stored in the DB as a hash) or a stray `.verified`.
- **The switch window is the only service interruption**, in this order:
  1. the old aihub container exits gracefully (drain ≤ `DRAIN_SECS`, default
     15s — the compose file's `stop_grace_period` is also 15s, longer than
     aihub's own 10s HTTP drain; the new default applies from the next
     deploy);
  2. deploy.sh applies the upgrade SQL (see below), one transaction per
     script;
  3. the new container starts and HTTP is polled.
  The actual downtime is printed and warned past `MAX_DOWNTIME_SECS`. It
  is still seconds: the SQL is a handful of DDL statements.
- **Post-swap verification**: row counts must be >= the frozen baseline,
  except tables dropped by a `DROP TABLE` in an upgrade.sql applied this
  run, which are excluded from the post-swap row-count check.
- **Automatic rollback**: if anything from the window on fails (the SQL
  transaction fails, the new container does not come up, verification
  fails, unexpected exit), deploy.sh first runs the `rollback.sql` of
  every version applied in this run — in descending version order, one
  transaction each — and only then recreates the old image and verifies
  the old version answers. The old binary never sees the post-upgrade
  schema; no backup restore is needed. A script that failed was fully
  rolled back by its own transaction, so nothing of it is applied.

## Where the SQL lives

Every release tag that changes the schema ships a directory:

    deploy/sql/<TAG>/upgrade.sql    the schema delta, applied as ONE transaction
    deploy/sql/<TAG>/rollback.sql   restores the pre-release shape for auto-rollback
    deploy/sql/<TAG>/probe.sql      the structural applied-check (run-once)

deploy.sh reads the files **from the target git tag at deploy time**
(`git -C .polyforge/.. show <TAG>:deploy/sql/<TAG>/...`, with a
working-tree fallback), the same way the legacy rollback Dockerfile is
fetched. release.sh is not involved — the SQL is not a release asset. A
release tag without a `deploy/sql/<TAG>/` directory carries no schema
change; its switch window is just the container recreate.

The candidate versions come from the deployer's **local** git tags, so the
target tag must exist locally — deploy.sh refuses in preflight otherwise
(`git fetch --tags`). The running version's tag is not required.

**File format**: each file is a sequence of `;`-terminated statements.
Comments are fine before or between statements, but **nothing may follow
the last statement's `;`** — a trailing comment after it is read as an
incomplete final statement and the runner refuses the file.

**Downgrades**: a `--force` downgrade whose range (target, running] contains
a `deploy/sql` version is refused in preflight — deploy.sh runs rollback SQL
only inside its own auto-rollback. Roll such a schema back by hand (each
crossed version's `rollback.sql`, newest first, service stopped).

**Run-once**: `probe.sql` is a structural check answered read-only against
the live database before the window (e.g. "does the column that will be
dropped still exist?"). `0` means the upgrade already ran and the version
is skipped — re-deploying the same version (`--force` included) never
applies the SQL twice. No bookkeeping table is written to the production
database.

**Multi-version gaps**: when the running version is several schema-changing
releases behind, deploy.sh applies every not-yet-applied version's SQL in
ascending version order inside the one window (probe-reported applied
versions are skipped).

## Requirements

The server's `python3` (stdlib `sqlite3`) must link a SQLite library
**≥ 3.35** — the first release with `ALTER TABLE ... DROP COLUMN`. deploy.sh
checks this in preflight, before the window, and refuses the deploy with
nothing changed when the linked library is older. SQL execution itself uses
the deployer's ssh + `python3` stdlib-sqlite3 path; the `sqlite3` CLI is
neither used nor required.

**Minimum database baseline.** The `deploy/sql/` chain starts at v2.4.0
and assumes the columns that the old in-process Go migrations added:
`machines.mode` / `machines.live_wi_ids` (heartbeat face, first in v0.4.2)
and `memories.fs_mtime_ns` / `memories.fs_size` (memory freshness stamp,
polyforge#129, first in v1.12.0). Those migrations were deleted in v2.2.0
(3d7a857) and `Open` runs none, so a database must either have been
created by v2.2.0 or later, or have been opened at least once by a server
between v1.12.0 and v2.1.x. A database last served by anything older is
not upgradable by deploy.sh: its first heartbeat or memory write fails
with `no such column`. Bring it through a v2.1.x server first (or add the
four columns by hand) before deploying a current version.

## First instance: v2.4.0 (polyforge#149, audit subtraction C)

The whole schema delta of #149: the delivery ledger, the dead tables and
columns are gone. The file `deploy/sql/v2.4.0/upgrade.sql` is the source
of truth; its content:

```sql
DROP TABLE IF EXISTS wi_ship_ledger;
DROP TABLE IF EXISTS core_meta;
ALTER TABLE projects DROP COLUMN scenario;
ALTER TABLE projects DROP COLUMN identifier_hash;
ALTER TABLE work_items DROP COLUMN bundle_digest;
ALTER TABLE work_items DROP COLUMN workflow_budget_baseline;
ALTER TABLE work_items DROP COLUMN session_owner;
```

The matching `deploy/sql/v2.4.0/rollback.sql` re-adds the five dropped
columns with defaults (`identifier_hash TEXT NOT NULL DEFAULT ''`; the
others nullable as before) and rebuilds `wi_ship_ledger` and `core_meta`
as empty tables from the pre-#149 (85cf926) DDL: old-version code runs
correctly on the result, existing rows carry the defaults.
`deploy/sql/v2.4.0/probe.sql` counts the dead shapes (0 = already applied).

## v2.5.0 (polyforge#157, repo inventory column)

The per-work-item repo inventory (02 §4.6) moves from the
`wi_repo_inventory` table into one JSON column, `work_items.repo_inventory`
(`[{"name": "<project repo>"}]`, names only — URLs are resolved from the
project at claim time; NULL = no inventory). Production held 0 rows in
the table, so nothing is carried over. The v2.5.0 server reads and writes
the column, so it does not work until this SQL has run. The file
`deploy/sql/v2.5.0/upgrade.sql` is the source of truth; its content:

```sql
DROP TABLE IF EXISTS wi_repo_inventory;
ALTER TABLE work_items ADD COLUMN repo_inventory TEXT;
```

The post-swap row-count check leaves `wi_repo_inventory` out (it is
dropped by this upgrade.sql). `deploy/sql/v2.5.0/rollback.sql` drops the
column (an inventory filed during the failed deploy is lost with it) and
rebuilds `wi_repo_inventory` with its unique index as an empty table from
the v2.4.x DDL. `deploy/sql/v2.5.0/probe.sql` answers 0 when the table is
gone and the column exists (a database created by v2.5.0 is already in
that shape).

## Work item dependencies (polyforge#161)

**Deploy aihub before upgrading the CLI.** `pf file create --after` sends
its blockers in the create body (`after: [...]`); an older aihub ignores
the unknown key and files the work item WITHOUT the dependency, silently.
`pf file dep add|rm|list` and `pf file show` use endpoints every aihub
already has.

- Semantics change: only a **wrapped** blocker releases its dependent. A
  failed or canceled blocker now keeps the dependent queued (before, any
  terminal status released it); remove the edge with `pf file dep rm` to
  proceed. Production held 0 dependency rows, so nothing existing changes.
- A `blocks` blocker must be an execution ticket (bundle bound);
  re-adding an existing edge is a no-op instead of a CORE-500.
- No window SQL: the new index `idx_dep_blocked` is in the DDL that
  `core.Open` runs on every start (`CREATE INDEX IF NOT EXISTS`), so it is
  built on the first boot of the new server. An older server ignores it,
  so a rollback needs nothing either.

## Optional: stale `waiting_on` cleanup (manual, NOT in the window)

The server's `waiting_on` value domain dropped the `gate` and
`runtime:*` branches (polyforge#149) while keeping `owner` and
`dependency:<slug>` live. Rows written before that change may still hold
a now-dead value. The cleanup is OPTIONAL — a stale value is inert (the
row is excluded from the executable queue segments, exactly as before) —
and its WHERE clause is deliberately bounded to the dead values: it must
never touch `owner` or `dependency:*` rows, which are live data. It is
deliberately NOT part of deploy.sh's window SQL:

```sql
UPDATE work_items SET waiting_on = NULL
WHERE waiting_on = 'gate' OR waiting_on LIKE 'runtime:%';
```

## No auto migration

`core.Open` never rewrites an existing database: the DDL is
`CREATE TABLE/INDEX IF NOT EXISTS` — a missing table or index is created,
nothing existing is altered (no column is added or changed). An old-schema
database is not repaired at boot, and the new server cannot create
projects on it (the dead `projects.identifier_hash NOT NULL` column has
no default). The upgrade is always the deploy.sh window SQL described
above — applied transactionally, probe-guarded and rolled back
automatically by the same tool when the deploy fails.

## Delivery notes (polyforge#149, audit subtraction C)

One row per deletion: the audit item, the commit (the message identifies
the plan unit) and the externally visible change.

| audit item | what was removed | externally visible change |
|---|---|---|
| **C1** — delivery ledger removed end-to-end | `core/ship_ledger.go` + its tests, the `wi_ship_ledger` DDL, the `POST /work-items/{id}/ship-ledger` route + `hRecordShipLedger` + `Server.Ships`, `api.ShipLedgerRequest/Entry`, `ErrDaemonShipLedger` (DAEMON-514), `aihubclient.RecordShipLedger`, the daemon Hub method and the ship-bridge record call (`git show aff1909`) | the ledger endpoint answers 404; the ship bridge no longer records anything — delivery truth is the git branch |
| **C9** — terminate-blocked door removed whole-chain | the `POST /work-items/{id}/terminate-blocked` route + handler, `core.TerminateBlocked`, `aihubclient.TerminateBlockedWorkItem`, the `pf wrap --fail` blocked-specific CLI branch, the daemon parked-close terminate special case (`git show abd463c`) | blocked work items recover ONLY via continue (`pf resume`) or abandon (`pf cancel`); `pf wrap --fail` on a parked WI fails (no door) |
| **C12** — session_owner removed | the `work_items.session_owner` DDL column and every core read/write/scan, the `SessionClaimRequest/AttachRequest/SessionClaim.Owner` wire fields, the `SessionSnapshot` view field, the CLI render owner display, the runtime Owner initializers (`git show e34202a`) | no session answer or view carries an owner field; the machine id (`session_machine`) stays |
| **B9** — bundle digest chain removed, name kept | the `work_items.bundle_digest` DDL column, the create-request digest field, the `WorkItem/SessionClaim` digest fields, the `bundleRef` stand-in, the PATCH digest half + whitelist entry, every carrier `BundleDigest` field (capsule, bridge, daemon, driver, runtime) and materialize's verification branch (CAP-405) + `nameDerivedDigest`/`bundleEntries` (`git show 146b053`) | create/PATCH carry the name only; a PATCH body with a digest key is refused CORE-400; materialization never verifies digests |
| dead-code (with C1/B9's schema halves) — dead tables, columns, endpoint, header | `core_meta`, `projects.scenario`/`identifier_hash`, `work_items.workflow_budget_baseline`, `api.WorkItem.BudgetBaseline`, the `memories/rebuild-index` route + `hRebuildIndex`, the `X-API-Key` fallback, the phantom "owner" role comment (`git show 1c5e7a3`) | a fresh database has none of the dead shapes; X-API-Key no longer authenticates (Bearer only); the memory rebuild runs at boot only |
| waiting_on dead branches (kept-face of the rewind-fix Requirement) | only the `gate` and `runtime:*` branches of `validWaitingOn`; `owner` and `dependency:<slug>` stay live (`git show 0cde108`) | PATCH `waiting_on='gate'`/`'runtime:*'` is refused CORE-400; the optional cleanup SQL above is bounded to those values |
| **D3** — goal limit counts characters | `MaxGoalBytes` → `MaxGoalRunes`; create and PATCH checks switch to rune counting (`git show 8919250`) | a 500-character CJK goal (1500 bytes) is accepted; 501 characters is refused |
| **D5** — owner is the reporter, gate scope pinned | no production change (code was already reporter-based); behavior pinned by the new server-suite test (`git show b267598`) | the reporter may continue/abandon only; ForceEnd stays admin/maintainer (no widening) |

## Work machines: readable capsule directories and ship branches (polyforge#144)

From this release a work machine names what a human sees by the work
item's slug (`<project>#<seq>`, e.g. `polyforge#140`), and the two on-disk
or on-remote names derived from it use the `#`-free form
`<project>-<seq>`:

- **capsule directory**: a NEW claim lands at
  `<workspace>/capsules/<project>-<seq>` (was `capsules/<wi_id>`);
- **ship branch**: `pf ship` pushes `polyforge/<project>-<seq>` (was
  `polyforge/<wi_id>`). (Pre-#166: `pf ship` is removed — see the
  polyforge#166 section below; the `deliver` step pushes the same branch.)

The id stays the machine identifier everywhere else (API, `state.json`
keys, `capsule.json`, the external git-dir, the capsule remote).

**Nothing is renamed or migrated.** A work item that is running, paused or
end_pending when the daemon is upgraded keeps the capsule directory its
`state.json` record names and continues there (restart recovery, attach,
`pf resume`). A blocked work item resumed after the upgrade is re-claimed
into the new `capsules/<project>-<seq>` directory (cloned from its capsule
chain, as every re-claim is); its old `capsules/<wi_id>` worktree and the
id-keyed external git-dir `<data-dir>/git/<wi_id>.git` are moved aside
together, as one unit, to `capsules/<wi_id>.quarantined.<ts>` and
`git/<wi_id>.git.quarantined.<ts>` (never deleted). Apart from that
re-claim, the daemon never looks for an id-named directory again:
`pf ship` and `pf watch` of a closed work item find only
`<workspace>/capsules/<project>-<seq>`, and there is no "accept both
names" fallback for directories or branches.

**Before upgrading a work machine:**

1. Let every work item claimed on it finish — or at least close it — and
   **ship every closed work item you still intend to ship** before the
   upgrade. After it, `pf ship` of a work item whose capsule is still
   `capsules/<wi_id>` answers `DAEMON-404`, and a work item that was
   already shipped would otherwise open a second PR from the new
   `polyforge/<project>-<seq>` branch beside the old `polyforge/<wi_id>`
   one.
2. Upgrade the binary and restart the daemon as usual.

**Leftovers after the upgrade.** Existing `polyforge/wi_*` branches and
their PRs are left untouched. An id-named capsule directory of a closed
work item is no longer found by any daemon face; to ship it anyway, rename
it by hand while the daemon holds no job for it
(`mv capsules/<wi_id> capsules/<project>-<seq>`), or bridge its commits
manually; otherwise delete it once it is no longer needed.

## `pf ship` removed: delivery is the workflows' `deliver` step (polyforge#166)

The daemon no longer pushes deliveries: the `pf ship` verb, the daemon's
`POST /jobs/{wi}/ship` route and the `DAEMON-512` push-preflight code are
gone (`pf ship` now answers "unknown command"). The delivery lanes
(`feature-full`, `bug-fix`, `autopilot`, `chore`) instead end with a
`deliver` workflow step that merges the latest origin default branch,
pushes only the feature branch `polyforge/<project>-<seq>`, opens or reuses
its PR, and auto-merges it (`gh pr merge --merge`) when review and tests
passed with no LLM-resolved conflict (autopilot, which has no review step,
on verification's pass) — otherwise it asks a human-only
question answered via `pf answer`. The section above describes the
pre-#166 `pf ship`; a closed work item that was never shipped before the
upgrade has to be bridged manually (push its sealed capsule commits by
hand).

`deliver` runs at the end of EVERY work item on those four lanes, so before
upgrading check each work machine and each task repo:

- the machine's git push identity and `gh` login work for the step agents
  (they inherit the daemon's environment: `HOME`, SSH agent, `gh`'s login);
- a task repo whose origin is not on GitHub, or a machine without a usable
  `gh`, gets no PR: the step pushes the feature branch only and then asks
  the human question (merge by hand, or don't merge) — it does not block;
- the `polyforge/<project>-<seq>` branches it pushes are the only refs it
  writes; nothing merges without a PR (`gh pr merge --merge`) or a human's
  answer.

## Workflow structure simplification (polyforge#165)

`feature-full` drops from 11 to 9 steps: `e2e-baseline` merges into
`e2e-author` and `e2e-run` merges into `verification`; every delivery
bundle now runs `verification → review → deliver` (`chore` swapped its
`review` and `verification`). No migration is shipped; what an in-flight
work item meets depends on how its driver next starts:

- **Same-daemon recovery** (daemon restart, crash recovery, a paused work
  item re-attached by its holder) relaunches from the capsule's own
  materialized bundle snapshot, never from the checkout: the work item
  finishes on the bundle it was claimed on.
- **Re-claim** (`pf resume` of a blocked work item, or a claim by another
  machine) clones the capsule fresh and materializes the daemon's
  CURRENT `--workflows-dir` checkout. The `execution.json` of every
  claimed `feature-full` work item carries an entry for all 11 old steps
  (the driver seeds them at the first start, whatever the position), so the
  driver refuses the start and the work item lands `blocked` with the
  reason `execution.json records step(s) "e2e-baseline", "e2e-run" that
  workflow bundle "feature-full" no longer has …`. Resuming it again
  repeats the refusal; cancel it and re-file. A `chore` work item keeps
  its step ids and continues in the new order.

Therefore update the workflows checkout only when no `feature-full` work
item is claimed and unfinished (queued ones are fine: they have no
`execution.json` yet), or accept that a blocked one must be re-filed.
`pf queue` lists them.

## Step `kind` removed; `.polyforge.yaml` is `steps.<project>.<step-id>.models` (polyforge#169)

The step `kind` (`qa`/`spec`/`author`/`code`/`gate`/`review`) is gone: the
shipped bundles no longer declare it and the driver no longer validates
it. Its only use was grouping steps for the workspace yaml's model lists.
The workspace yaml is now keyed by **step id**, under a new top-level
`steps:` section:

```yaml
# Precedence: pf submit --models > steps.<project>.<step-id>.models > bundle step model_candidates > steps.<project>.default.models > bundle defaults
steps:
  <project>:
    default:   { models: [...] }
    <step-id>: { models: [...] }
```

A step-id key governs the step with that id in every bundle that has one
(`fix` covers bug-fix's and autopilot's `fix`). A step entry's only key is
`models` (polyforge#159 briefly added `harness`; polyforge#175 removed it
again — a residual `harness` key is refused with the retired-harness hint,
see "Per-step harness" below); any other key is refused, naming it. YAML anchors and aliases work as usual
(`models: &x [...]`, `{ models: *x }`).

**No backward compatibility — edit all three workspace files together
with the deploy.** A file that still has the top-level `models:` section
is refused at every claim (DAEMON-414: "top-level `models:` was replaced
by `steps:` (polyforge#169) — move models.<project>.<key>: [...] to
steps.<project>.<key>.models: [...] (see docs/upgrade.md)"), the work
item stays queued, and `pf doctor` reports the same under `workspace
yaml`. Deploying this version therefore requires rewriting osp-ws,
gmi-ws and yf-ws's `.polyforge.yaml` (below) at the same time; until a
file is rewritten, no work item of that workspace's projects is claimed
on that machine.

Typo protection: every non-`default` key must be a step id of the
workflows the daemon runs (its `--workflows-dir`). Any other key refuses
the claim with DAEMON-414 before anything is claimed — the work item
stays queued — and `pf doctor` names every unknown key
(`steps.<project>.<key>`). A workflows dir with no readable bundle
refuses with "no workflows found in <dir>". So a retired kind that is
not also a step id (`qa`, `author`, `gate`) refuses; `code`, `spec` and
`review` load but mean only the step with that id. Likewise every
project key under `steps:` must be a project cli.toml maps to that
workspace, and a `models` list's ids are trimmed and must be non-empty,
unique strings (the `pf submit --models` rules) written as plain
strings — numbers, YAML tags and aliased items (`[&m a, *m]`) are
refused. YAML merge keys (`<<`) are refused at every level; share lists
with anchors instead (`models: &x [...]` / `{ models: *x }`, as in the
blocks below). The file must hold exactly one YAML document.

Step-id keys are checked against the workflows dir at every claim, not
once: before deploying a workflows release that removes or renames a
step id named in a workspace yaml, delete or rename that key first
(`pf doctor` flags it) — otherwise every claim in that workspace is
refused (DAEMON-414), and a work item whose claim was already open when
the daemon restarted (claiming-journal replay) ends `blocked` instead.
The same holds for the `models:` → `steps:` rewrite above.

Old bundle snapshots: a capsule claimed before the upgrade carries a
materialized bundle whose frontmatter still says `kind: …`. The parser
ignores the field, so same-daemon recovery of such a capsule loads it
unchanged. The model lists such a work item uses were composed at claim
time and snapshotted, so it keeps them (recovery never re-reads the
yaml).

### Old kind → step ids (current bundles)

| old kind | step ids it covered |
|---|---|
| `qa` | `grill` (feature-full), `grill-review` (feature-full), `root-cause` (bug-fix) |
| `spec` | `spec` (feature-full), `grill` (autopilot) |
| `author` | `plan` (feature-full, chore), `e2e-author` (feature-full) |
| `code` | `code` (feature-full, chore), `repro` (bug-fix), `fix` (bug-fix, autopilot), `deliver` (feature-full, bug-fix, chore, autopilot), `release`, `deploy` (release) |
| `gate` | `verification` (feature-full, bug-fix, chore, autopilot), `verify` (release) |
| `review` | `review` (feature-full, bug-fix, chore) |

`grill` is one key for both lanes (it was `qa` in feature-full and `spec`
in autopilot).

### The three workspaces

(These lists are the #169 rewrite; since polyforge#175 every candidate
also needs its `pi:` / `claude:` prefix — see that section's rewrite.)

All three declare only `default` and `code` today, in the old `models:`
shape. Besides the shape change, the old `code:` (a kind) covered every
kind:code step; as a step id it governs only the `code` step
(feature-full, chore). To keep today's behaviour, give each old kind:code
step id (`code`, `fix`, `repro`, `deliver`, `release`, `deploy`) the same
list through a YAML anchor. Note that today the `code` list also
overrides `release` and `deploy`'s own bundle pins
(`[sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5]`); dropping
those two keys lets the bundle pins govern instead. Replace each file's
header precedence comment (it states the pre-#164 order `project <kind>
> project default > workflow step model_candidates`) with the line below.

osp-ws (`/root/code/aicoding/osp-ws/.polyforge.yaml`):

```yaml
# Precedence: pf submit --models > steps.<project>.<step-id>.models > bundle step model_candidates > steps.<project>.default.models > bundle defaults
steps:
  polyforge:
    default: { models: &osp-default [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra] }
    code: { models: &osp-code [sub2api-glm/glm-5.3, sub2api-glm/glm-5.3-flash, sub2api-claude/pi-claude-opus-5] }
    fix: { models: *osp-code }
    repro: { models: *osp-code }
    deliver: { models: *osp-code }
    release: { models: *osp-code }
    deploy: { models: *osp-code }
  pi:
    default: { models: *osp-default }
    code: { models: *osp-code }
    fix: { models: *osp-code }
    repro: { models: *osp-code }
    deliver: { models: *osp-code }
    release: { models: *osp-code }
    deploy: { models: *osp-code }
  tsshd:
    default: { models: *osp-default }
    code: { models: *osp-code }
    fix: { models: *osp-code }
    repro: { models: *osp-code }
    deliver: { models: *osp-code }
    release: { models: *osp-code }
    deploy: { models: *osp-code }
```

gmi-ws (`/root/code/aicoding/gmi-ws/.polyforge.yaml`):

```yaml
# Precedence: pf submit --models > steps.<project>.<step-id>.models > bundle step model_candidates > steps.<project>.default.models > bundle defaults
steps:
  ieops:
    default: { models: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra] }
    code: { models: &gmi-code [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
    fix: { models: *gmi-code }
    repro: { models: *gmi-code }
    deliver: { models: *gmi-code }
    release: { models: *gmi-code }
    deploy: { models: *gmi-code }
```

(gmi-ws's code list equals the release bundle's own `release`/`deploy`
pins, so dropping those two keys changes nothing there. Also remove its
2026-10-14 header note that flash "stays eligible for lighter kinds via
the other chains": kinds no longer exist; flash is eligible wherever a
list naming it governs.)

yf-ws (`/root/code/aicoding/yf-ws/.polyforge.yaml`):

```yaml
# Precedence: pf submit --models > steps.<project>.<step-id>.models > bundle step model_candidates > steps.<project>.default.models > bundle defaults
steps:
  yingfang:
    default: { models: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5, sub2api-openai/gpt-6-astra] }
    code: { models: &yf-code [sub2api-glm/glm-5.3-flash, sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
    fix: { models: *yf-code }
    repro: { models: *yf-code }
    deliver: { models: *yf-code }
    release: { models: *yf-code }
    deploy: { models: *yf-code }
```

(yf-ws's header also says "a project default overrides even steps that
pin their own models" — untrue since polyforge#164; drop it with the old
precedence comment.)

Run `pf doctor` after the edit: `workspace yaml` must pass.

## Per-step harness: pi or Claude Code (polyforge#159)

> **Superseded in part by polyforge#175** — see "Model candidates name
> their harness: `<harness>:<model>`" below. The `harness` key (yaml and
> bundle), the separate harness chain, the id-shape compatibility rule
> and the "model/harness mismatch" refusal described in this section
> are **removed**: every candidate now carries its harness as a
> mandatory `pi:` / `claude:` prefix, and a step's chain may cross
> harnesses. What a claude step runs, its credentials and `claude_bin`
> below still hold.

A step can now run on **Claude Code** instead of pi. The choice is a
second per-step property next to `models`, in the same `steps:` entries:

```yaml
# Models: pf submit --models > steps.<project>.<step-id>.models > bundle step model_candidates > steps.<project>.default.models > bundle defaults
# Harness: steps.<project>.<step-id>.harness > bundle step harness > steps.<project>.default.harness > bundle defaults.harness > pi
steps:
  polyforge:
    default: { models: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
    review:  { models: [claude-opus-5-5, claude-sonnet-5], harness: claude }
```

- `harness` is a plain string, `pi` or `claude` (closed set; anything
  else — `claude-code`, a list, a number — refuses the claim with
  DAEMON-414 naming `steps.<project>.<key>.harness`). Omitted everywhere
  = pi: **no existing file needs an edit**, every step keeps running on
  pi exactly as before.
- An entry may carry `harness` alone (`fix: { harness: claude }`); its
  model list then falls through the model chain as usual.
- The two properties resolve **independently**, each down its own chain
  (first present level wins). There is no per-work-item harness:
  `pf submit --models` sets the candidates of every step, each step keeps
  its own harness. A bundle may declare `harness` on a step or under
  `defaults` (the shipped bundles do not).
- Like the candidates, the harness is resolved at claim time and kept by
  the daemon (state.json); a yaml edit governs the next claim, never an
  in-flight work item.

### Which models a harness can run (compatibility) — removed by polyforge#175

Read off the model id, no registry:

| harness | model ids it runs |
|---|---|
| `claude` | official Claude ids: a `claude-` prefix and no `/` (`claude-opus-5-5`, `claude-sonnet-5`); aliases such as `opus` / `sonnet` are not accepted |
| `pi` | every other id — the `provider/model` form (`sub2api-glm/glm-5.3`, `anthropic/claude-…`) |

One list may mix both kinds: each step walks only the ids its harness
runs, in order, and the model fallback (D7: switch on auth / billing /
unknown model, same-model retry on rate limit / 5xx) skips the others.
A step whose harness runs **none** of a non-empty candidate list is a
configuration error: it refuses every claim of the project with
DAEMON-414 before anything is claimed (the work item stays queued),
e.g.

```
model/harness mismatch: polyforge/bug-fix/review: harness claude (steps.polyforge.review.harness) cannot serve any of [sub2api-glm/glm-5.3, …] (steps.polyforge.default.models)
```

So setting `harness: claude` without giving the step claude ids is
caught at once (above). An empty or absent list is not a mismatch: the
CLI's own default model runs. A `pf submit --models` list is checked the
same way against the work item's bundle at submit time (and, for a work
item without a bundle at submit, at claim time).

**Behavior change for bare `claude-…` ids.** Before #159 every id ran on
pi; a bare `claude-…` id (no `provider/` prefix) in a list now routes to
Claude Code. If such an id is the only candidate of a pi step, the claim
is refused (above) unless the step's harness is `claude`; in a mixed list
pi simply skips it. Provider-qualified ids (`anthropic/claude-…`,
`sub2api-claude/…`) stay on pi.

### What a claude step runs

```
claude -p --bare --output-format stream-json --verbose --include-partial-messages
  --session-id <uuid> | --resume <uuid>  [--model <claude id>]
  --tools Bash,Read,Edit,Write,Glob,Grep,TodoWrite,Agent --allowedTools <same>
  --permission-mode dontAsk --permission-prompts none
  --strict-mcp-config --disable-slash-commands
```

with the capsule as cwd and the step prompt (the same text pi gets) on
stdin. `--bare`, `--strict-mcp-config` and `--disable-slash-commands`
give the step a clean context: no hooks, plugins, MCP servers, skills or
CLAUDE.md of the human's. Background tasks are off
(`CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1`, `--bare`, and no background
tool in `--tools`). The native session stays at
`~/.claude/projects/…/<uuid>.jsonl` and is copied into the capsule's
`session/` at every commit boundary; an answered question resumes the
same session (`--resume`).

**Credentials are the human's.** Log Claude Code in on the work machine
beforehand; the daemon never reads, writes or manages a credential — the
step's credential authority layer is its own
`<harness-root>/claude-config/settings.json` (polyforge#171; see the
later section), and an explicit daemon-env `ANTHROPIC_AUTH_TOKEN` is
what the step inherits when its settings carry none (the daemon
injects none itself). `--bare` does not read the interactive login: the
settings env block is applied on a bare step, and the adapter hard-sets
`CLAUDE_CONFIG_DIR` to the claude-config dir, so the daemon CAN see that
layer from the machine it runs on.

New cli.toml keys (flat, optional):

```toml
claude_bin = "/root/.local/bin/claude"     # default: claude on the daemon's PATH (or --claude-bin)
```

`pf doctor` adds a `harness claude` check whenever any step resolves to
claude: the binary must resolve on the daemon's PATH (else fail); the
version should be a verified one (2.1.296); when no credential is
visible — neither the daemon env nor the claude-config
`settings.json` env block (the daemon reports set/unset, never a
value) — it warns too. Credential values are never shown. The check
lists every claude step with the level its harness and models came
from. `workspace yaml` now also runs the compatibility check above.

## CC harness auth + directory unification (polyforge#171, #159 closeout)

The v2.5.2 per-step claude harness carried the credential through a
cli.toml key AND scattered the step harness directories over the data
dir and the daemon user's `~/.claude`. The closeout unifies them: the
`settings.json` of the harness root's `claude-config` dir is the single
credential authority layer, and all three step-harness directories
derive from one root.

### The daemons move

- `--harness-sessions-root` is REMOVED. Its old semantics ("the pi
  session dir itself") made an upgrade silently re-home the live
  transcript sync, so the flag was renamed instead: the new daemon
  refuses the old value with the unknown-flag error.

- `--harness-root` (env `POLYFORGE_HARNESS_ROOT`, default
  `<data-dir>`, higher than DataDir) is now the PARENT root of the
  three harness dirs:

      <harness-root>/step-agent            pi step agent dir (the dedicated minimal set)
      <harness-root>/step-agent-sessions   pi step sessions (the live-transcript sync's source)
      <harness-root>/claude-config         claude config dir (settings.json, projects/, debug/)

  The default keeps the byte-identical step-session and step-agent dirs
  under `<data-dir>`; only the claude dir is new and only the explicit
  override moves all three together.

### The credential key moves

- cli.toml's `anthropic_auth_token` is HARD-DELETED (no deprecation
  path). A residual key fails the boot with the unknown-key parse
  error — that is the intended migration story, not a bug:

      pf daemon: config ~/.config/polyforge/cli.toml: unknown key "anthropic_auth_token" in …

- Migrate (manual, ONCE): the claude step reads its credentials from
  the settings `env` block only — copy ONE credential pair from wherever
  it lives today (the daemon user's `~/.claude/settings.json` or a
  cli.toml you never checked in) into
  `<harness-root>/claude-config/settings.json`:

      {
        "env": {
          "ANTHROPIC_AUTH_TOKEN": "…",
          "ANTHROPIC_BASE_URL": "…"        // optional — paired with the token
        }
      }

  Copy only the two keys; never write credentials into project-level or
  capsule settings (a capsule is sealed and distributed), and NEVER set
  `CLAUDE_CONFIG_DIR` inside the settings `env` block: the adapter
  hard-sets the step's config dir and a value there would override it.

- The adapter injects ZERO credentials henceforth: no token rides the
  environment or argv from the daemon. `ANTHROPIC_*` variables set in
  the daemon's environment pass through to claude (an env token is
  what the step sees when its settings carry none). The daemon-started
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and the claude-config
  `CLAUDE_CONFIG_DIR` are added, and the operator's inherited config-dir
  value is stripped (it is hard-set).

- Where the daemon's environment comes from (repo review 2026-10-16
  V-11): `pf daemon ensure` starts the daemon inside the
  `polyforge-daemon` tmux session, so the daemon — and every step it
  spawns — inherits the environment of that tmux SERVER, captured when
  the server first started, possibly from a human's shell. Any
  `ANTHROPIC_*` there reaches every claude step unchanged (and is
  visible in `/proc/<pid>/environ`). The settings `env` block is the
  intended single credential source; Claude Code's documentation
  suggests that block overrides a same-named process variable, but that
  is NOT verified by polyforge, and a variable set only in the process
  env (e.g. an `ANTHROPIC_BASE_URL` absent from settings.json) certainly
  applies. Either keep credentials out of the tmux server's environment,
  or set every `ANTHROPIC_*` a step needs in settings.json.

### The pairing table (generic wording)

| what the settings env block carries | what a claude step does |
| --- | --- |
| gateway-credential + gateway address | gateway profile |
| subscription long-lived token (`claude setup-token`, `sk-ant-oat01-`, ~365 days) with NO gateway address | subscription direct profile |
| subscription long-lived token with ANY gateway address | REJECTED combination |
| no token at all | the step cannot authenticate (it will not run) |

Notes: `claude login`'s interactive credentials do not serve headless
steps — mint a long-lived token with `claude setup-token` and put it in
the settings env block; it is valid about a year, so re-mint before it
expires. An `ANTHROPIC_BASE_URL` in the DAEMON environment is inherited
(the doctor notes it); the rejection clause above is the doctor's
pairing clause — token shape and gateway address TOGETHER, values
never printed.

### The doctor sees the authority layer

`pf doctor`'s `harness claude` now reports (daemon-reported
harness-root layout, polyforge#171):

- the three dirs' existence (present/missing) under the daemon's
  harness root;
- the claude-config `settings.json` env block's key facts
  (ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL / CLAUDE_CONFIG_DIR) as
  set/unset — values are read in-memory, NEVER printed;
- the pairing clause above whenever the shape says rejected, and a
  remove-it clause whenever the settings env block carries
  `CLAUDE_CONFIG_DIR` (it would override the adapter's value).

## Model candidates name their harness: `<harness>:<model>` (polyforge#175)

polyforge#159 made the harness a per-step setting (a `harness` key in the
workspace yaml and the bundles) and guessed each candidate's harness from
the shape of its id (`claude-…` → claude, anything else → pi). That
could not express one step trying Claude Code first and pi second, and
the same model can run on both harnesses (Opus through pi + sub2api, or
through Claude Code's own API), so the guess was unreliable. The harness
is now part of every candidate.

### The format

Every model candidate is `<harness>:<model>`:

- `harness` is `pi` or `claude` (closed set);
- the prefix is case-sensitive: `PI:` / `Claude:` are unknown prefixes;
- the string is split on the **first** `:` — the model keeps any `/` and
  any further `:` (`pi:ollama/llama3:8b` is pi running `ollama/llama3:8b`);
- the model part is exactly what that harness's `--model` gets
  (`claude:claude-opus-5-5` runs `claude … --model claude-opus-5-5`,
  `pi:sub2api-glm/glm-5.3` runs `pi … --model sub2api-glm/glm-5.3`).

The prefix is **mandatory** and is the whole truth: there is no inference
from the id any more (`pi:claude-opus-5-5` is a pi candidate), no
`harness` key and no "this harness serves none of the list" check. One
format everywhere:

| where | key |
|---|---|
| `.polyforge.yaml` | `steps.<project>.<default\|step-id>.models` |
| bundle frontmatter | step `model_candidates`, `defaults.model_candidates` |
| CLI | `pf submit <id> --models claude:claude-opus-5-5,pi:sub2api-glm/glm-5.3` |

The precedence of the model chain is unchanged: `pf submit --models` >
`steps.<project>.<step-id>.models` > bundle step `model_candidates` >
`steps.<project>.default.models` > bundle `defaults.model_candidates`.
A step with no list (or a declared-empty one) runs pi's own default model.

A chain may **mix harnesses**: the fallback walks the candidates in order
(D7 unchanged: switch on auth / billing / unknown model, same-model retry
on rate limit / 5xx), and each dispatch runs on its candidate's harness.
A switch always starts a fresh session; when the next candidate is on
another harness the previous dispatch's transcript (the other harness's
JSONL) rides the new prompt as read-only reference — the #159 mechanism.
Same-step continuations (an answered question, the first retry after a
crash) still resume the same session when the harness is unchanged.

### Refusals (no backward compatibility)

| what | where it refuses | message (example) |
|---|---|---|
| a candidate without a prefix, with an unknown prefix, or with an empty model | `.polyforge.yaml`: every claim of that workspace's projects, DAEMON-414 before anything is claimed (the work item stays queued); `pf doctor` `workspace yaml` fails the same | `steps.polyforge.code.models[1]: "sub2api-glm/glm-5.3" needs a harness prefix (pi:… or claude:…)` |
| the `harness` key in a yaml step entry | same as above | ``steps.polyforge.fix.harness: the `harness` key was removed (polyforge#175) — move it into each model candidate's prefix, e.g. [claude:claude-opus-5-5, pi:sub2api-glm/glm-5.3] (see docs/upgrade.md)`` |
| an unprefixed `model_candidates` entry, or a `harness` key under a bundle step or `defaults` | the bundle fails to load: a claim of a work item on it is refused before it is made (DAEMON-414, the work item stays queued; `pf submit` gets the refusal directly and auto-pull skips such work items, logging the refusal once; should the bundle only be known after the claim, the claim ends `blocked` with the same text); `pf doctor` `workspace yaml` names file and step. The bundle's step ids still count as legal yaml keys | `workflow bundle bug-fix refused: …/bug-fix/workflow.md: step root-cause: model_candidates[0]: "sub2api-claude/pi-claude-opus-5" needs a harness prefix (pi:… or claude:…)` |
| `pf submit --models` with an unprefixed candidate | the CLI, before any request (the daemon route refuses the same with DAEMON-400) | `submit: --models: "sub2api-glm/glm-5.3" needs a harness prefix (pi:… or claude:…)` |

So, **as with #169, edit all three workspace files together with the
deploy**: until a file is rewritten, no work item of that workspace's
projects is claimed on that machine. The five shipped bundles are
already rewritten (every candidate `pi:` — they ran on pi before).

In-flight work items are unaffected: their candidate lists were
snapshotted at claim time in the daemon's `state.json`; a record written
before this version (bare ids plus the #159 `step_harness`) is read with
each bare id on that step's old harness (pi when none), keeping exactly
the candidates the step walked before, so recovery continues. A
`pf submit --models` list given before the upgrade to a work item that
is later re-claimed on the same machine (the daemon keeps it in
`manual-models.json`, e.g. `pf resume` of a blocked work item) still has
bare ids: the re-claim drops it — one stderr line and a
`manual_models_dropped` event name the list — and the step chains fall
back to the yaml/bundle levels. To pin a manual chain again, submit the
work item with a prefixed `--models` once it is queued. One edge
case changes harness: an in-flight step whose #159 harness was `claude`
but which had **no** candidate list (Claude Code's own default model)
recovers on pi's default model — the new format cannot express "claude
with its default model"; give such a step a `claude:` candidate.

### Before / after

```yaml
# before (#159)
steps:
  polyforge:
    default: { models: [sub2api-glm/glm-5.3, sub2api-claude/pi-claude-opus-5] }
    review:  { models: [claude-opus-5-5, claude-sonnet-5], harness: claude }

# after (#175)
steps:
  polyforge:
    default: { models: [pi:sub2api-glm/glm-5.3, pi:sub2api-claude/pi-claude-opus-5] }
    review:  { models: [claude:claude-opus-5-5, claude:claude-sonnet-5] }
    # new: one step, two harnesses — Claude Code's API first, pi + glm as fallback
    fix:     { models: [claude:claude-opus-5-5, pi:sub2api-glm/glm-5.3] }
```

```
# before                                        # after
pf submit wi_x --models a/b,claude-opus-5-5     pf submit wi_x --models pi:a/b,claude:claude-opus-5-5
```

A bundle step: `model_candidates: [sub2api-glm/glm-5.3]` becomes
`model_candidates: [pi:sub2api-glm/glm-5.3]`; a `harness: claude` on the
step is deleted and its candidates get `claude:`.

`pf doctor`: `harness claude` runs only when some resolved candidate is
`claude:…`, and lists each such step's whole chain (each candidate's
harness is its prefix) with the level it came from; `workspace yaml`
also loads the daemon's installed bundles. The #171 claude-config
authority-layer checks are unchanged.

### Rewriting the three workspaces (examples — edit the files yourself)

The rule: a bare id ran on its step's harness — `harness: claude` when
the entry (or the bundle step) said so, otherwise pi — and #159 silently
**skipped** ids the step's harness could not run (`claude-…` ids on a pi
step, everything else on a claude step). Prefix each id accordingly. An
id that was being skipped can now either be dropped (exactly today's
behaviour) or kept with its real harness (it will then actually run, in
its place in the chain — a behaviour change that adds Claude spend; the
examples below keep today's behaviour and show that alternative as a
comment). Keep each file's header comment but drop any `harness`
wording.

osp-ws (`/root/code/aicoding/osp-ws/.polyforge.yaml`):

```yaml
steps:
  polyforge:
    default:
      models: [pi:sub2api-glm/glm-5.3, pi:sub2api-claude/pi-claude-opus-5, pi:sub2api-openai/gpt-6-astra]
    smoke:
      models: [claude:claude-opus-5-5]   # the smoke bundle's step ran on claude
    fix:
      models: [claude:claude-opus-5-5]   # was: harness: claude + [claude-opus-5-5]
    code:
      models: [pi:sub2api-glm/glm-5.3-flash, pi:sub2api-glm/glm-5.3, pi:sub2api-claude/pi-claude-opus-5]
  pi:
    default:
      models: [pi:sub2api-glm/glm-5.3, pi:sub2api-claude/pi-claude-opus-5, pi:sub2api-openai/gpt-6-astra]
    smoke:
      models: [claude:claude-opus-5-5]
    code:
      models: [pi:sub2api-glm/glm-5.3-flash, pi:sub2api-glm/glm-5.3, pi:sub2api-claude/pi-claude-opus-5]
  tsshd:
    default:
      models: [pi:sub2api-glm/glm-5.3, pi:sub2api-claude/pi-claude-opus-5, pi:sub2api-openai/gpt-6-astra]
    smoke:
      models: [claude:claude-opus-5-5]
    code:
      models: [pi:sub2api-glm/glm-5.3-flash, pi:sub2api-glm/glm-5.3, pi:sub2api-claude/pi-claude-opus-5]
```

(`smoke` is the step id of a smoke bundle that is not among the five
shipped ones: if that bundle ran the step on pi rather than claude, the
bare `claude-opus-5-5` was being skipped there — write the list that
bundle step should really walk. The bundle itself must also be
rewritten: no `harness` key, prefixed `model_candidates`.)

gmi-ws (`/root/code/aicoding/gmi-ws/.polyforge.yaml`):

```yaml
steps:
  ieops:
    default:
      # today's behaviour: #159 skipped claude-opus-5-5 on these (pi) steps.
      # Alternative — keep it as a real Claude Code fallback (adds Claude spend):
      #   models: [pi:sub2api-glm/glm-5.3, claude:claude-opus-5-5, pi:sub2api-openai/gpt-6-astra]
      models: [pi:sub2api-glm/glm-5.3, pi:sub2api-openai/gpt-6-astra]
    smoke:
      models: [claude:claude-opus-5-5]
    code:
      # today's behaviour: claude-sonnet-5-5 was skipped likewise.
      # Alternative (adds Claude spend):
      #   models: [pi:sub2api-glm/glm-5.3, claude:claude-sonnet-5-5]
      models: [pi:sub2api-glm/glm-5.3]
```

yf-ws (`/root/code/aicoding/yf-ws/.polyforge.yaml`):

```yaml
steps:
  yingfang:
    default:
      models: [pi:sub2api-glm/glm-5.3, pi:sub2api-claude/pi-claude-opus-5, pi:sub2api-openai/gpt-6-astra]
    smoke:
      models: [claude:claude-opus-5-5]
    code:
      models: [pi:sub2api-glm/glm-5.3-flash, pi:sub2api-glm/glm-5.3, pi:sub2api-claude/pi-claude-opus-5]
```

Run `pf doctor` after the edit: `workspace yaml` must pass, and
`harness claude` must pass wherever a `claude:` candidate remains.

## Work machines: harnesses reference the release; the daemon is supervised (polyforge#174)

Re-running `tools/install.sh` on a machine installed before this change
migrates it in place; a second run changes nothing.

### What the re-run changes

- **pi**: the `git:git@github.com:silgrid/polyforge.git@<ver>` entry in
  pi's settings `packages` (a second, full clone of the repo under
  `~/.pi/agent/git/`) is replaced, in place, by one local entry naming
  `~/.local/share/polyforge/release/current` — the checkout install.sh
  already keeps. pi loads a local package where it lies (no copy); the
  release's `package.json` declares the skill AND the `pf-chat`
  extension. Other packages are untouched; the previous settings file is
  kept as `settings.json.bak-polyforge-<timestamp>`. `pi` itself is no
  longer invoked. Restart the pi sessions (pi-web) to load it. The old
  clone is no longer referenced; remove it once pi runs fine:
  `rm -rf ~/.pi/agent/git/github.com/silgrid/polyforge`.
- **Claude Code**: the copied `~/.claude/skills/polyforge` directory is
  removed and replaced by a symlink to
  `release/current/skills/polyforge`. The SessionStart hook keeps its
  registered path (`~/.claude/skills/polyforge/hooks/claude-session-start.sh`,
  now resolved through the link). Claude Code 2.1.296 accepts symlinked
  skill dirs; confirm once in an interactive session that `polyforge` is
  listed among the skills.
- **daemon**: the upgrade holds `~/.local/share/polyforge/ensure.lock`
  (a harness session starting meanwhile starts nothing), shuts the daemon
  down gracefully (the old `nohup` one included; a loop's daemon takes its
  loop with it on that clean exit) and waits until daemon and loop are
  gone, swaps the binary, then runs `pf daemon ensure`: from now on the
  daemon runs inside the restart loop (a failed exit is restarted within
  ~5s; log
  `~/.local/share/polyforge/daemon.log`, appended, rotated at 10 MiB). When
  the binary is already current, a live daemon is left alone and an
  unsupervised one is reported with the fix:
  `pf ops shutdown && while pf ops status >/dev/null 2>&1 || ! flock -n ~/.local/share/polyforge/daemon.lock true; do sleep 2; done; pf daemon ensure`.
- **order and failure safety** (repo review 2026-10-16 V-3): everything
  that can fail for an outside reason — download, SHA check, the release
  clone into its own `release/<VER>` directory — runs before anything is
  stopped or replaced; such a failure exits non-zero and says that pf,
  `release/current` and the running daemon are unchanged. The new binary
  is staged next to the installed one (`<bin>.new.<pid>`, already
  executable) and swapped by one rename: `pf` is never missing, partial
  or non-executable.
  An abort after the daemon was asked to stop reports the state it
  leaves (daemon stopped, the binary in place, where `release/current`
  points) and what to run: re-run the installer, or `pf daemon ensure`.
- **running jobs refuse the upgrade**: a binary upgrade needs the
  graceful shutdown, which terminates every live agent tree. While
  `pf ops status` lists a job that is running, recovering or sealing
  (`paused` and `end_pending` have no agent tree and do not count), or
  while the daemon's job list cannot be read, install.sh lists the
  jobs, changes nothing (the fetched `release/<VER>` checkout is kept for
  the next run) and exits non-zero. Wait for them, pause them
  (`pf pause <wi>`), or override — the interrupted work items recover at
  the next daemon start:
  `curl ... | PF_INSTALL_FORCE=1 bash -s -- <api-key>`.
- **after a container / machine restart** tmux is gone too: the next
  Claude Code session start (SessionStart: startup/resume/clear/compact) or
  pi session start (pi-web replays it when it resumes its sessions) runs
  `pf daemon ensure` and brings the daemon back. Until then auto-pull does
  not run — the accepted cost of not touching the container's startup.
- **`pf ops shutdown` still stops** the daemon for good: the loop restarts
  only a failed exit (crash, panic, kill); a clean exit (`pf ops shutdown`,
  SIGTERM) ends the loop too. Start it again with `pf daemon ensure`.

### v1 leftovers

`pf doctor` (and the installer's harness rows) now warn about v1
polyforge artifacts still installed — the v1 aihub bridge as a pi package
(`GMISWE/ieops-aihub`) or at `~/.pi/agent/extensions/polyforge/`, v1
Claude skills (`using-polyforge`, `pf-*`) and v1 Claude plugins — and
print one command line that first backs them up into
`~/polyforge-v1-backup-<timestamp>/`, then removes them. Nothing is
removed automatically. Restart the harness sessions afterwards.

## Hub git calls ignore the operator's git config (repo review 2026-10-16 V-5)

The daemon's git calls
that talk to aihub with the session credential — the capsule chain push, the
capsule clone and the carrier ls-remote/fetch/push — run with no system or
global git config (`GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`) and
with every inherited `GIT_*` variable stripped. So `http.proxy`,
`http.sslCAInfo`, `url.*.insteadOf` and `credential.helper` from
`~/.gitconfig` do NOT apply to them: give the daemon a proxy through
`https_proxy` / `HTTPS_PROXY` in its environment, and since `GIT_SSL_CAINFO`
is stripped too, an aihub behind an internal CA needs that CA in the system
trust store. The capsule commit itself still reads the global config, so
`user.name` / `user.email` must be set for the daemon user; task-repo git
(inside `repos/<name>`) also still uses the global config.

Before this change an operator's `~/.gitconfig` could reroute the
credentialed push (`url.*.insteadOf`) or hand the session secret to a
storing `credential.helper`. Check before upgrading a work machine whose
aihub access depends on a git-config proxy or CA setting: move it to
`https_proxy` / the system trust store first, or the chain push and
re-claim clone will fail.

## Bundle `quick-fix` renamed to `autopilot` (repo review 2026-10-16 V-7)

The light remediation bundle is now `workflows/autopilot/` (frontmatter
`name: autopilot`). Behaviour is unchanged — the new name says what the
old one hid: with no grill question asked and its gates passing, its
`deliver` step merges without any human. Step ids are unchanged (`grill`,
`fix`, `verification`, `deliver`), so `.polyforge.yaml` entries keyed
`steps.<project>.fix` (and the other step ids) resolve exactly as before;
nothing in the workspace yaml names a bundle.

The daemon resolves a work item's bundle by name in its workflows
checkout, and the old name no longer exists there. Before deploying:

- **Queued, never-claimed work items bound to `quick-fix`**: re-bind them,
  `pf file edit <wi> --bundle autopilot`. aihub accepts a `bundle_name`
  PATCH exactly while the work item is queued and has never been claimed
  (the R2 window); list them with `pf queue`.
- **Work items that were already claimed** (running, paused, blocked, or
  queued again after a claim) cannot be re-bound — `bundle_name` froze at
  the first claim. Let them finish before the upgrade, or cancel them and
  file again with `--bundle autopilot`.

