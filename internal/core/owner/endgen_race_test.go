package owner

// White-box regression test for the FA-1 takeover/end-generation race
// (02 §2.3/§6.2). EndGeneration validates the active generation and the
// caller's token BEFORE opening its transaction; a concurrent force-takeover
// can end that very generation in the window between the checks and the
// guarded UPDATE. endGenerationTx is the transactional half, so this test
// drives it directly with a generation row that a takeover has already
// ended — the exact state the race produces — and asserts the guard:
// CORE-403 stale_generation, nothing terminalized, no event.

import (
	"database/sql"
	"testing"

	"github.com/silgrid/polyforge/internal/core/store"
)

func seedEndGenRace(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := NewManager(db, nil)
	for _, q := range []string{
		`INSERT INTO users(id, display_name, kind, role) VALUES ('usr_old', 'old owner', 'human', 'writer')`,
		`INSERT INTO users(id, display_name, kind, role) VALUES ('usr_admin', 'admin', 'human', 'admin')`,
		`INSERT INTO projects(id, name) VALUES ('pj1', 'p1')`,
		`INSERT INTO work_items(id, seq, project_id, goal, status, reporter, created_at, updated_at)
			VALUES ('wi_race', 1, 'pj1', 'race fixture', 'running', 'usr_old', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return m, "wi_race"
}

func wiStatus(t *testing.T, m *Manager, wiID string) string {
	t.Helper()
	var status string
	if err := m.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// TestEndGenerationTxRefusesConcurrentlyEndedGeneration: the caller passed
// every pre-transaction check for generation 1, but a takeover ended that
// row before the transaction opened. The guarded UPDATE matches 0 rows and
// the WI must NOT be terminalized by the fenced owner.
func TestEndGenerationTxRefusesConcurrentlyEndedGeneration(t *testing.T) {
	m, wi := seedEndGenRace(t)
	// Generation 1: already ended by a takeover (the race's loser state);
	// generation 2 is the active one under the new owner.
	if _, err := m.db.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at, ended_at, end_reason)
		VALUES (?1, 1, 'usr_old', '2026-01-01T00:00:00Z', '2026-01-01T00:01:00Z', 'takeover')`, wi); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at)
		VALUES (?1, 2, 'usr_admin', '2026-01-01T00:01:00Z')`, wi); err != nil {
		t.Fatal(err)
	}

	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// The fenced owner's transactional half for generation 1.
	err = m.endGenerationTx(tx, wi, 1, EndGenerationRequest{Reason: "wrap"}, "usr_old")
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-403" || ae.Reason != "stale_generation" {
		t.Fatalf("concurrently-ended generation must be refused as fencing, got %v", ae)
	}
	// Release the single connection before any out-of-transaction read.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Nothing was terminalized and no event landed.
	if s := wiStatus(t, m, wi); s != "running" {
		t.Fatalf("WI must stay running, got %s", s)
	}
	var events int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM wi_events WHERE work_item_id=?1 AND event_type='wrapped'`, wi).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("no wrapped event may land for a fenced generation, got %d", events)
	}

	// Positive control: the same transactional half for the ACTIVE
	// generation 2 succeeds and terminalizes.
	tx2, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	if err := m.endGenerationTx(tx2, wi, 2, EndGenerationRequest{Reason: "wrap"}, "usr_admin"); err != nil {
		t.Fatalf("active generation must end: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	if s := wiStatus(t, m, wi); s != "wrapped" {
		t.Fatalf("WI must be wrapped, got %s", s)
	}
	var reason sql.NullString
	if err := m.db.QueryRow(`SELECT end_reason FROM owner_generations WHERE work_item_id=?1 AND generation=2`, wi).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !reason.Valid || reason.String != "wrap" {
		t.Fatalf("generation 2 end_reason = %+v", reason)
	}
}

// TestEndGenerationTxDuplicateCloseIsIdempotentRefusal: a second close of
// the SAME generation (e.g. a retried request whose checks passed against a
// still-active row that the first close already ended) must be refused the
// same way instead of double-transitioning the WI.
func TestEndGenerationTxDuplicateCloseIsIdempotentRefusal(t *testing.T) {
	m, wi := seedEndGenRace(t)
	if _, err := m.db.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at)
		VALUES (?1, 1, 'usr_old', '2026-01-01T00:00:00Z')`, wi); err != nil {
		t.Fatal(err)
	}
	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := m.endGenerationTx(tx, wi, 1, EndGenerationRequest{Reason: "pause"}, "usr_old"); err != nil {
		t.Fatalf("first close must succeed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx2, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = m.endGenerationTx(tx2, wi, 1, EndGenerationRequest{Reason: "fail"}, "usr_old")
	if err == nil {
		t.Fatal("duplicate close must be refused")
	}
	if ae, ok := err.(*APIError); !ok || ae.Code != "CORE-403" || ae.Reason != "stale_generation" {
		t.Fatalf("duplicate close must read CORE-403 stale_generation, got %v", err)
	}
	// Release the single connection before the out-of-transaction read.
	if err := tx2.Rollback(); err != nil {
		t.Fatal(err)
	}
	if s := wiStatus(t, m, wi); s != "paused" {
		t.Fatalf("WI must stay paused, got %s", s)
	}
}
