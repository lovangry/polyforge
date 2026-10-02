package core

// machines_test.go: the M3 heartbeat face at the Core level — upsert
// idempotency (register once, refresh forever), the keep-vs-replace
// semantics of the liveness projection (mode, live_wi_ids), the admin
// read model, and the guarded machines-column migration for databases
// created before the face was wired.

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestHeartbeatRegistersOnceThenRefreshes(t *testing.T) {
	env := newEnv(t)
	h1, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "m-reg", RuntimeVersion: "v1.5"})
	if err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	if !h1.Registered || h1.Key == nil || h1.UserID == "" {
		t.Fatalf("first heartbeat must register: %+v", h1)
	}
	first := h1.Key.Token.ID

	// Repeat: same identity, no new key, refresh only.
	h2, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "m-reg"})
	if err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}
	if h2.Registered || h2.Key != nil || h2.UserID != h1.UserID {
		t.Fatalf("repeat must refresh in place: %+v", h2)
	}

	// Exactly one api key was ever issued to the machine identity.
	keys, err := env.Admin.ListAPIKeys()
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	n := 0
	for _, k := range keys {
		if k.UserID == h1.UserID {
			n++
			if k.ID != first {
				t.Fatalf("unexpected extra key %s", k.ID)
			}
		}
	}
	if n != 1 {
		t.Fatalf("machine identity holds %d keys, want 1", n)
	}

	// Timestamp refresh: backdate, heartbeat, observe the stamp move to
	// ~now (server-stamped; the payload has no client timestamp field).
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := env.DB.Exec(`UPDATE machines SET last_heartbeat_at=? WHERE id='m-reg'`, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "m-reg"}); err != nil {
		t.Fatalf("refresh heartbeat: %v", err)
	}
	var at string
	if err := env.DB.QueryRow(`SELECT last_heartbeat_at FROM machines WHERE id='m-reg'`).Scan(&at); err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatalf("stamp %q unparseable: %v", at, err)
	}
	if age := time.Since(parsed); age < -time.Minute || age > time.Minute {
		t.Fatalf("heartbeat did not refresh last_heartbeat_at (age %s)", age)
	}
}

func TestHeartbeatLivenessProjectionKeepVsReplace(t *testing.T) {
	env := newEnv(t)
	// Register with a full projection.
	if _, err := env.Admin.Heartbeat(MachineHeartbeat{
		MachineID: "m-live", Mode: "interactive",
		LiveWIIDs: []string{"wi_b", "wi_a"}, RuntimeVersion: "v1.5",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	ms, err := env.Admin.Machines()
	if err != nil || len(ms) != 1 {
		t.Fatalf("machines: %v %+v", err, ms)
	}
	if ms[0].Mode != "interactive" || ms[0].RuntimeVersion != "v1.5" {
		t.Fatalf("projection lost: %+v", ms[0])
	}
	// live_wi_ids is stored (order preserved) and read back.
	if len(ms[0].LiveWIIDs) != 2 || ms[0].LiveWIIDs[0] != "wi_b" || ms[0].LiveWIIDs[1] != "wi_a" {
		t.Fatalf("live_wi_ids round-trip: %+v", ms[0].LiveWIIDs)
	}

	// Empty mode / nil live_wi_ids: KEEP the stored values.
	if _, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "m-live"}); err != nil {
		t.Fatalf("keep heartbeat: %v", err)
	}
	ms, _ = env.Admin.Machines()
	if ms[0].Mode != "interactive" || len(ms[0].LiveWIIDs) != 2 {
		t.Fatalf("empty payload must keep the stored projection: %+v", ms[0])
	}

	// Empty (non-nil) live_wi_ids: REPLACE with nothing held live.
	if _, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "m-live", LiveWIIDs: []string{}}); err != nil {
		t.Fatalf("replace heartbeat: %v", err)
	}
	ms, _ = env.Admin.Machines()
	if len(ms[0].LiveWIIDs) != 0 {
		t.Fatalf("empty slice must clear live_wi_ids: %+v", ms[0].LiveWIIDs)
	}
	if ms[0].Mode != "interactive" {
		t.Fatalf("empty mode must keep stored mode: %+v", ms[0])
	}

	// A new mode replaces; a second machine appears in the same listing.
	if _, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "m-live", Mode: "drain"}); err != nil {
		t.Fatalf("mode replace: %v", err)
	}
	if _, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "m-two", Mode: "batch"}); err != nil {
		t.Fatalf("second machine: %v", err)
	}
	ms, _ = env.Admin.Machines()
	if len(ms) != 2 {
		t.Fatalf("machines rows: %d", len(ms))
	}
	if ms[0].Mode != "drain" {
		t.Fatalf("mode not replaced: %+v", ms[0])
	}
	// Oldest heartbeat first: m-live's last refresh predates m-two's row.
	if ms[0].ID != "m-live" || ms[1].ID != "m-two" {
		t.Fatalf("listing order (oldest heartbeat first): %s, %s", ms[0].ID, ms[1].ID)
	}
}

func TestHeartbeatBadInput(t *testing.T) {
	env := newEnv(t)
	if _, err := env.Admin.Heartbeat(MachineHeartbeat{MachineID: "  "}); !IsCode(err, "CORE-400") {
		t.Fatalf("blank machine_id must be CORE-400: %v", err)
	}
}

// TestMachinesColumnsMigrationFromLegacyDatabase opens a database whose
// machines table predates the M3 heartbeat wiring (no mode/live_wi_ids)
// and asserts Open migrates it in place and the face works on it.
func TestMachinesColumnsMigrationFromLegacyDatabase(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "aihub.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// The pre-wiring machines shape plus the users row it references.
	if _, err := raw.Exec(`CREATE TABLE users (
  id TEXT PRIMARY KEY, display_name TEXT NOT NULL, email TEXT,
  kind TEXT NOT NULL CHECK (kind IN ('human','machine')),
  role TEXT NOT NULL DEFAULT 'writer' CHECK (role IN ('admin','writer')),
  created_at TEXT NOT NULL
);`); err != nil {
		t.Fatalf("users table: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE machines (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id),
  runtime_version TEXT,
  last_heartbeat_at TEXT NOT NULL
);`); err != nil {
		t.Fatalf("machines table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO users(id, display_name, kind, created_at)
		VALUES ('mac_old', 'machine mac_old', 'machine', 't')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO machines(id, user_id, runtime_version, last_heartbeat_at)
		VALUES ('m-legacy', 'mac_old', 'v1', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	raw.Close()

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open through core: %v", err)
	}
	defer db.Close()
	admin := NewAdminManager(db)
	ms, err := admin.Machines()
	if err != nil {
		t.Fatalf("read after migration: %v", err)
	}
	if len(ms) != 1 || ms[0].ID != "m-legacy" {
		t.Fatalf("legacy row preserved: %+v %v", ms, err)
	}
	if ms[0].Mode != "" || len(ms[0].LiveWIIDs) != 0 {
		t.Fatalf("legacy row reads as unlived: %+v", ms[0])
	}
	// And the face works on the migrated table.
	h, err := admin.Heartbeat(MachineHeartbeat{MachineID: "m-legacy", Mode: "interactive", LiveWIIDs: []string{"wi_x"}})
	if err != nil || h.Registered {
		t.Fatalf("heartbeat on migrated row: %+v %v", h, err)
	}
	ms, _ = admin.Machines()
	if ms[0].Mode != "interactive" || len(ms[0].LiveWIIDs) != 1 || ms[0].LiveWIIDs[0] != "wi_x" {
		t.Fatalf("projection on migrated row: %+v", ms[0])
	}
}

// TestMachinesMigrationIdempotentReopen: a second Open on a migrated (or
// fresh) database is a no-op — the guards hold.
func TestMachinesMigrationIdempotentReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	db.Close()
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	var names []string
	rows, err := db2.Query(`SELECT name FROM pragma_table_info('machines')`)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	got := strings.Join(names, ",")
	for _, want := range []string{"mode", "live_wi_ids"} {
		if !strings.Contains(got, want) {
			t.Fatalf("machines columns after reopen lack %s: %s", want, got)
		}
	}
}
