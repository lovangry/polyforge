// Package schema holds the SQLite Core schema (02-aihub-core.md#§4) and the
// migration framework (§11). Design authority: docs/design/polyforge-vnext/02-aihub-core.md.
//
// INV-1 mechanical assertion: no table or column in this schema carries any
// workflow execution state (current_step / step_attempt / review_round /
// budget_consumed / …). The only execution-authority store is the Capsule's
// runtime-state/execution.json (03-execution-capsule.md#§4.2). The audit test
// in this package asserts this mechanically.
package schema

// DDLV1 is the complete v1 Core schema. First boot installs it as-is
// (02 §11: 一期从无到有，首次启动建库 v1，无存量迁移).
//
// Statement order matters for FOREIGN KEY dependencies under
// `foreign_keys=ON`: referenced tables are created before referencing ones.
const DDLV1 = `
CREATE TABLE IF NOT EXISTS core_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS service_secret_epochs (
  epoch        INTEGER PRIMARY KEY,
  mode         TEXT NOT NULL CHECK (mode IN ('grace','rollback')),
  created_at   TEXT NOT NULL,
  retired_at   TEXT,
  rotated_by   TEXT REFERENCES users(id)
);

CREATE TABLE IF NOT EXISTS users (
  id           TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  kind         TEXT NOT NULL CHECK (kind IN ('human','machine')),
  role         TEXT NOT NULL DEFAULT 'writer' CHECK (role IN ('admin','writer'))
);

CREATE TABLE IF NOT EXISTS api_keys (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id),
  project_id TEXT REFERENCES projects(id),
  key_hash   TEXT NOT NULL,
  revoked_at TEXT
);

CREATE TABLE IF NOT EXISTS machines (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id),
  runtime_version TEXT,
  last_heartbeat_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
  id        TEXT PRIMARY KEY,
  name      TEXT NOT NULL UNIQUE,
  visible   INTEGER NOT NULL DEFAULT 1,
  scenario  TEXT
);

CREATE TABLE IF NOT EXISTS project_members (
  project_id TEXT NOT NULL REFERENCES projects(id),
  user_id    TEXT NOT NULL REFERENCES users(id),
  role       TEXT NOT NULL CHECK (role IN ('viewer','writer','maintainer')),
  created_at TEXT NOT NULL,
  PRIMARY KEY (project_id, user_id)
);

CREATE TABLE IF NOT EXISTS work_items (
  id           TEXT PRIMARY KEY,
  seq          INTEGER NOT NULL UNIQUE,
  project_id   TEXT NOT NULL REFERENCES projects(id),
  goal         TEXT NOT NULL,
  content      TEXT,
  parent_id    TEXT REFERENCES work_items(id),
  status       TEXT NOT NULL DEFAULT 'queued'
               CHECK (status IN ('queued','running','paused','blocked',
                                 'wrapped','failed','cancelled')),
  priority     TEXT NOT NULL DEFAULT 'normal'
               CHECK (priority IN ('low','normal','high','urgent')),
  milestone    TEXT,
  labels       TEXT NOT NULL DEFAULT '[]',
  attrs        TEXT NOT NULL DEFAULT '{}',
  reporter     TEXT NOT NULL REFERENCES users(id),
  assignee     TEXT REFERENCES users(id),
  waiting_on   TEXT,
  bundle_name  TEXT,
  bundle_digest TEXT,
  workflow_budget_baseline TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  closed_at    TEXT,
  -- 三列同 NULL（登记票）或同非 NULL（执行票）：02 §4.2 CHECK，语义与设计文档逐字一致；
  -- 仅位置按 SQLite 语法移至列定义之后（表级约束不得先于列定义，设计文档排版笔误）。
  CHECK ((bundle_name IS NULL) = (bundle_digest IS NULL)
         AND (bundle_name IS NULL) = (workflow_budget_baseline IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_wi_status      ON work_items(status, priority DESC, created_at);
CREATE INDEX IF NOT EXISTS idx_wi_assignee    ON work_items(assignee) WHERE status IN ('running','paused','blocked');
CREATE INDEX IF NOT EXISTS idx_wi_waiting_on  ON work_items(waiting_on) WHERE waiting_on IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_wi_parent     ON work_items(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_wi_milestone  ON work_items(project_id, milestone) WHERE milestone IS NOT NULL;

CREATE TABLE IF NOT EXISTS wi_dependencies (
  blocking_wi_id TEXT NOT NULL REFERENCES work_items(id),
  blocked_wi_id  TEXT NOT NULL REFERENCES work_items(id),
  kind           TEXT NOT NULL CHECK (kind IN ('blocks','supersedes','related')),
  created_at     TEXT NOT NULL,
  PRIMARY KEY (blocking_wi_id, blocked_wi_id, kind),
  CHECK (blocking_wi_id <> blocked_wi_id)
);

CREATE TABLE IF NOT EXISTS owner_generations (
  work_item_id TEXT NOT NULL REFERENCES work_items(id),
  generation   INTEGER NOT NULL,
  owner        TEXT NOT NULL REFERENCES users(id),
  machine_id   TEXT REFERENCES machines(id),
  started_at   TEXT NOT NULL,
  ended_at     TEXT,
  end_reason   TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_owner_gen_active
  ON owner_generations(work_item_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_owner_gen_seq
  ON owner_generations(work_item_id, generation);

CREATE TABLE IF NOT EXISTS claim_idempotency (
  idempotency_key TEXT PRIMARY KEY,
  work_item_id    TEXT NOT NULL REFERENCES work_items(id),
  caller_identity TEXT NOT NULL,
  response_json   TEXT NOT NULL,
  created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS workflow_budget_increments (
  work_item_id TEXT NOT NULL REFERENCES work_items(id),
  dimension    TEXT NOT NULL CHECK (dimension IN ('requests','tokens','wall_minutes')),
  seq          INTEGER NOT NULL,
  amount       INTEGER NOT NULL CHECK (amount > 0),
  granted_by   TEXT NOT NULL REFERENCES users(id),
  granted_at   TEXT NOT NULL,
  note         TEXT,
  PRIMARY KEY (work_item_id, seq)
);

CREATE TABLE IF NOT EXISTS wi_repo_inventory (
  work_item_id TEXT NOT NULL REFERENCES work_items(id),
  position     INTEGER NOT NULL,
  name         TEXT NOT NULL,
  url          TEXT NOT NULL,
  subdir       TEXT,
  base         TEXT,
  PRIMARY KEY (work_item_id, position)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_inventory_name ON wi_repo_inventory(work_item_id, name);

CREATE TABLE IF NOT EXISTS wi_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  work_item_id TEXT NOT NULL REFERENCES work_items(id),
  created_at   TEXT NOT NULL,
  actor        TEXT REFERENCES users(id),
  event_type   TEXT NOT NULL,
  payload      TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_events_wi ON wi_events(work_item_id, id);

CREATE TABLE IF NOT EXISTS capsule_revisions (
  work_item_id TEXT NOT NULL REFERENCES work_items(id),
  seq          INTEGER NOT NULL,
  parent       INTEGER,
  generation   INTEGER NOT NULL,
  manifest_sha TEXT NOT NULL,
  data_bytes   INTEGER NOT NULL,
  published_at TEXT NOT NULL,
  PRIMARY KEY (work_item_id, seq),
  CHECK ((seq = 0 AND parent IS NULL) OR (seq > 0 AND parent = seq - 1))
);

CREATE TABLE IF NOT EXISTS capsule_heads (
  work_item_id TEXT PRIMARY KEY REFERENCES work_items(id),
  generation   INTEGER NOT NULL,
  seq          INTEGER NOT NULL,
  updated_at   TEXT NOT NULL,
  FOREIGN KEY (work_item_id, seq) REFERENCES capsule_revisions(work_item_id, seq)
);

CREATE TABLE IF NOT EXISTS capsule_tombstones (
  work_item_id TEXT PRIMARY KEY REFERENCES work_items(id),
  tombstoned_at TEXT NOT NULL,
  hard_delete_at TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','reverted','hard_deleted')),
  confirm_wi_id TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS memories (
  id           TEXT PRIMARY KEY,
  project_id   TEXT NOT NULL REFERENCES projects(id),
  owner_user_id TEXT NOT NULL REFERENCES users(id),
  work_item_id TEXT REFERENCES work_items(id),
  type         TEXT NOT NULL,
  title        TEXT,
  visibility   TEXT NOT NULL,
  authority_path TEXT NOT NULL,
  content_sha  TEXT NOT NULL,
  content_len  INTEGER NOT NULL,
  source_markdown_sha TEXT,
  html_sha     TEXT,
  tags         TEXT NOT NULL DEFAULT '[]',
  related      TEXT NOT NULL DEFAULT '[]',
  idempotency_key TEXT UNIQUE,
  status       TEXT NOT NULL DEFAULT 'active',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
  mem_id UNINDEXED, body,
  tokenize='trigram'
);
`
