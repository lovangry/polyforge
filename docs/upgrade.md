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
- **The switch window is the only service interruption**, in this order:
  1. the old aihub container exits gracefully (drain ≤ `DRAIN_SECS`);
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
`CREATE TABLE IF NOT EXISTS` and gains nothing else. An old-schema
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
(`feature-full`, `bug-fix`, `quick-fix`, `chore`) instead end with a
`deliver` workflow step that merges the latest origin default branch,
pushes only the feature branch `polyforge/<project>-<seq>`, opens or reuses
its PR, and auto-merges it (`gh pr merge --merge`) when review and tests
passed with no LLM-resolved conflict (quick-fix, which has no review step,
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
(`fix` covers bug-fix's and quick-fix's `fix`). A step entry's only key
is `models` for now (any other key — `harness` included — is refused,
naming it). YAML anchors and aliases work as usual
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
`review` load but mean only the step with that id.

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
| `spec` | `spec` (feature-full), `grill` (quick-fix) |
| `author` | `plan` (feature-full, chore), `e2e-author` (feature-full) |
| `code` | `code` (feature-full, chore), `repro` (bug-fix), `fix` (bug-fix, quick-fix), `deliver` (feature-full, bug-fix, chore, quick-fix), `release`, `deploy` (release) |
| `gate` | `verification` (feature-full, bug-fix, chore, quick-fix), `verify` (release) |
| `review` | `review` (feature-full, bug-fix, chore) |

`grill` is one key for both lanes (it was `qa` in feature-full and `spec`
in quick-fix).

### The three workspaces

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
