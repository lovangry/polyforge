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
