package workitem

// White-box regression test for the FA-6 cancel/claim race (02 §2.5/§6.5).
// Cancel checks the WI status on the shared connection BEFORE opening its
// transaction; a claim can commit in between, leaving the WI running with an
// active generation by the time the transaction starts. cancelTx is the
// transactional half, so this test drives it directly against exactly that
// state and asserts the in-transaction re-check: typed CORE-409, nothing
// written, no cancelled event.

import (
	"database/sql"
	"testing"

	"github.com/silgrid/polyforge/internal/core/testsupport"
)

func seedCancelRace(t *testing.T) *Manager {
	t.Helper()
	db, dir := testsupport.NewDB(t)
	if err := testsupport.SeedBase(db); err != nil {
		t.Fatal(err)
	}
	return NewManager(db, dir)
}

// TestCancelTxRefusesRunningWIWithActiveGeneration: the outer check saw
// "queued", a claim won the race, and the transaction now observes a running
// WI with an active generation — it must refuse instead of cancelling
// around the live generation (02 §2.3: cancel never ends an active row).
func TestCancelTxRefusesRunningWIWithActiveGeneration(t *testing.T) {
	m := seedCancelRace(t)
	d, err := m.Create(CreateRequest{Project: "p1", Goal: "race fixture", Reporter: "usr_worker"})
	if err != nil {
		t.Fatal(err)
	}
	// The claim that won the race: running + active generation row.
	if _, err := m.db.Exec(`UPDATE work_items SET status='running' WHERE id=?1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at)
		VALUES (?1, 1, 'usr_worker', '2026-01-01T00:00:00Z')`, d.ID); err != nil {
		t.Fatal(err)
	}

	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = m.cancelTx(tx, d.ID, "usr_worker")
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-409" {
		t.Fatalf("running WI must be refused CORE-409 inside the transaction, got %v", ae)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var status string
	var waiting, closed sql.NullString
	if err := m.db.QueryRow(`SELECT status, waiting_on, closed_at FROM work_items WHERE id=?1`, d.ID).
		Scan(&status, &waiting, &closed); err != nil {
		t.Fatal(err)
	}
	if status != "running" || waiting.Valid || closed.Valid {
		t.Fatalf("nothing may be written on refusal: status=%s waiting_on=%+v closed_at=%+v", status, waiting, closed)
	}
	var events int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM wi_events WHERE work_item_id=?1 AND event_type='cancelled'`, d.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("no cancelled event may land, got %d", events)
	}

	// Positive control: after the generation is ended (pause), the same
	// transactional half cancels the paused WI.
	if _, err := m.db.Exec(`UPDATE owner_generations SET ended_at='2026-01-01T00:02:00Z', end_reason='pause' WHERE work_item_id=?1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`UPDATE work_items SET status='paused' WHERE id=?1`, d.ID); err != nil {
		t.Fatal(err)
	}
	tx2, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	if err := m.cancelTx(tx2, d.ID, "usr_worker"); err != nil {
		t.Fatalf("paused WI without active generation must cancel: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	if s := wiStatusForTest(t, m, d.ID); s != "cancelled" {
		t.Fatalf("WI must be cancelled, got %s", s)
	}
}

func wiStatusForTest(t *testing.T, m *Manager, wiID string) string {
	t.Helper()
	var status string
	if err := m.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wiID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}
