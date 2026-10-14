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
