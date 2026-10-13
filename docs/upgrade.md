# Upgrade guide

This document describes how to upgrade an existing aihub database from
the pre-#149 (audit subtraction C) schema to the current one. The server
runs **no automatic migration** (`Open` installs the schema with
`CREATE TABLE IF NOT EXISTS` only): an old database keeps its old shape
until an operator applies the SQL below by hand.

## Before you upgrade

1. **Back up first.** Stop the aihub server, then copy the whole data
   directory (the `aihub.db` file and its WAL sidecars) somewhere safe:

   ```sh
   systemctl stop aihub   # or however you stop it
   cp -a /var/lib/aihub /var/lib/aihub.backup-$(date +%Y%m%d)
   ```

   The SQL below drops tables and columns; the only way back is the
   backup.

2. Apply the SQL with the `sqlite3` CLI (or your operator tool of
   choice) against the data file:

   ```sh
   sqlite3 /var/lib/aihub/aihub.db
   ```

3. Restart the server and check the log for schema errors; the
   smoke test is a `GET /api/v1/projects/<project>/ready-queue` plus
   filing and claiming one throw-away work item.

## The upgrade SQL (mandatory)

This is the whole schema delta of polyforge#149 (audit subtraction C):
the delivery ledger, the dead tables and columns are gone. The statements
are idempotent-safe to run once; re-running them on an upgraded database
fails loudly on the missing shapes (which is the desired signal that the
work is already done).

```sql
DROP TABLE IF EXISTS wi_ship_ledger;
DROP TABLE IF EXISTS core_meta;
ALTER TABLE projects DROP COLUMN scenario;
ALTER TABLE projects DROP COLUMN identifier_hash;
ALTER TABLE work_items DROP COLUMN bundle_digest;
ALTER TABLE work_items DROP COLUMN workflow_budget_baseline;
ALTER TABLE work_items DROP COLUMN session_owner;
```

What each statement removes:

- `wi_ship_ledger` — the polyforge#123 delivery ledger (audit item C1).
  Delivery truth is the git branch; nothing reads the table.
- `core_meta` — a key/value table no code path ever touched.
- `projects.scenario`, `projects.identifier_hash` — dead columns from
  the v0 project face.
- `work_items.bundle_digest` — the bundle digest chain (audit item B9):
  the reference is `bundle_name` only.
- `work_items.workflow_budget_baseline` — a never-written budget column.
- `work_items.session_owner` — the session owner column (audit item
  C12); the machine id (`session_machine`) stays.

## Optional: stale `waiting_on` cleanup

The server's `waiting_on` value domain dropped the `gate` and
`runtime:*` branches (polyforge#149) while keeping `owner` and
`dependency:<slug>` live. Rows written before that change may still hold
a now-dead value. The cleanup is OPTIONAL — a stale value is inert (the
row is excluded from the executable queue segments, exactly as before) —
and its WHERE clause is deliberately bounded to the dead values: it must
never touch `owner` or `dependency:*` rows, which are live data.

```sql
UPDATE work_items SET waiting_on = NULL
WHERE waiting_on = 'gate' OR waiting_on LIKE 'runtime:%';
```

## No auto migration

`core.Open` never rewrites an existing database: the DDL is
`CREATE TABLE IF NOT EXISTS` and gains nothing else. An old-schema
database left un-upgraded simply keeps its dead shapes (the server
reads and writes only the live columns) — nothing recreates or repairs
them. The upgrade is always this document's manual SQL, applied by an
operator, after a backup.

## Delivery notes (polyforge#149, audit subtraction C)

One row per deletion, each with the audit item, the commit (the message
identifies the plan unit), and its `git diff --stat` line counts
(insertions/deletions across the whole unit):

| audit item | what was removed | externally visible change | lines |
|---|---|---|---|
| **C1** — delivery ledger removed end-to-end | `core/ship_ledger.go` + its tests, the `wi_ship_ledger` DDL, the `POST /work-items/{id}/ship-ledger` route + `hRecordShipLedger` + `Server.Ships`, `api.ShipLedgerRequest/Entry`, `ErrDaemonShipLedger` (DAEMON-514), `aihubclient.RecordShipLedger`, the daemon Hub method and the ship-bridge record call (`git show aff1909`) | the ledger endpoint answers 404; the ship bridge no longer records anything — delivery truth is the git branch | 1422 insertions / 577 deletions (13 files; the insertions are dominated by the w149 e2e acceptance suite, 1421 lines, landing with this unit) |
| **C9** — terminate-blocked door removed whole-chain | the `POST /work-items/{id}/terminate-blocked` route + handler, `core.TerminateBlocked`, `aihubclient.TerminateBlockedWorkItem`, the `pf wrap --fail` blocked-specific CLI branch, the daemon parked-close terminate special case (`git show abd463c`) | blocked work items recover ONLY via continue (`pf resume`) or abandon (`pf cancel`); `pf wrap --fail` on a parked WI fails (no door) | 90 insertions / 251 deletions (10 files) |
| **C12** — session_owner removed | the `work_items.session_owner` DDL column and every core read/write/scan, the `SessionClaimRequest/AttachRequest/SessionClaim.Owner` wire fields, the `SessionSnapshot` view field, the CLI render owner display, the runtime Owner initializers (`git show e34202a`) | no session answer or view carries an owner field; the machine id (`session_machine`) stays | 53 insertions / 84 deletions (16 files) |
| **B9** — bundle digest chain removed, name kept | the `work_items.bundle_digest` DDL column, the create-request digest field, the `WorkItem/SessionClaim` digest fields, the `bundleRef` stand-in, the PATCH digest half + whitelist entry, every carrier `BundleDigest` field (capsule, bridge, daemon, driver, runtime) and materialize's verification branch (CAP-405) + `nameDerivedDigest`/`bundleEntries` (`git show 146b053`) | create/PATCH carry the name only; a PATCH body with a digest key is refused CORE-400; materialization never verifies digests | 245 insertions / 516 deletions (36 files) |
| dead-code (with C1/B9's schema halves) — dead tables, columns, constants, endpoint, header | `core_meta`, `projects.scenario`/`identifier_hash`, `work_items.workflow_budget_baseline`, `api.WorkItem.BudgetBaseline`, `ErrCoreLockHeld`/`ErrMemUpstream`/`ErrMemUnavailable`, the `memories/rebuild-index` route + `hRebuildIndex`, the `X-API-Key` fallback, the phantom "owner" role comment (`git show 1c5e7a3`) | a fresh database has none of the dead shapes; X-API-Key no longer authenticates (Bearer only); the memory rebuild runs at boot only | 40 insertions / 90 deletions (8 files) |
| waiting_on dead branches (kept-face of the rewind-fix Requirement) | only the `gate` and `runtime:*` branches of `validWaitingOn`; `owner` and `dependency:<slug>` stay live (`git show 0cde108`) | PATCH `waiting_on='gate'`/`'runtime:*'` is refused CORE-400; the optional cleanup SQL above is bounded to those values | 119 insertions / 3 deletions (3 files) |
| **D3** — goal limit counts characters | `MaxGoalBytes` → `MaxGoalRunes`; create and PATCH checks switch to rune counting (`git show 8919250`) | a 500-character CJK goal (1500 bytes) is accepted; 501 characters is refused | 37 insertions / 7 deletions (3 files) |
| **D5** — owner is the reporter, gate scope pinned | no production change (code was already reporter-based); behavior pinned by the new server-suite test (`git show b267598`) | the reporter may continue/abandon only; ForceEnd stays admin/maintainer (no widening) | 128 insertions / 0 deletions (1 file) |
