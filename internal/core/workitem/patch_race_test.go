package workitem

// White-box regression test for the R3C-F3 patch/end-generation(block)
// interleave (02 §2.5, R2C-F10). Patch reads the WI status on the shared
// connection BEFORE applying the update: an end-generation(block) that
// commits `blocked` + waiting_on in that window used to sail past the
// blocked-clearing guard (it judged a stale `queued`), and the
// unconditional UPDATE then cleared waiting_on on a blocked row — a
// blocked WI with no waiting object, dissolving the §2.5 invariant.
// patchTx is the transactional half, so this test drives it directly with
// exactly the state the interleave produces — a caller whose outer read
// saw `queued` while the row is now blocked with a waiting object — and
// asserts the in-transaction re-check: typed CORE-400 BLOCKED_NEEDS_WAITING_ON,
// nothing written. Mirrors cancel_race_test.go.

import (
	"database/sql"
	"testing"

	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/testsupport"
)

func seedPatchRace(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := testsupport.SeedBase(db); err != nil {
		t.Fatal(err)
	}
	return NewManager(db, dir)
}

// TestPatchTxRefusesWaitingOnClearAfterConcurrentBlock: the outer Patch
// read observed `queued` (the only status in which the clearing patch
// would have passed the precheck), an end-generation(block) committed
// blocked + waiting_on='owner' in the window, and the transactional half
// now observes the blocked row. It must refuse and write nothing.
func TestPatchTxRefusesWaitingOnClearAfterConcurrentBlock(t *testing.T) {
	m := seedPatchRace(t)
	d, err := m.Create(CreateRequest{Project: "p1", Goal: "patch race fixture", Reporter: "usr_worker"})
	if err != nil {
		t.Fatal(err)
	}
	// The end-generation(block) that won the race (02 §5.1a leaves exactly
	// this row shape behind: status=blocked + non-empty waiting_on).
	if _, err := m.db.Exec(`UPDATE work_items SET status='blocked', waiting_on='owner' WHERE id=?1`, d.ID); err != nil {
		t.Fatal(err)
	}

	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// The clearing patch's validated SET list, as Patch's parsing hands it
	// to the transactional half for {"waiting_on": ""}.
	err = m.patchTx(tx, d.ID, []string{"waiting_on=NULL"}, nil, true)
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-400" || ae.Reason != "BLOCKED_NEEDS_WAITING_ON" {
		t.Fatalf("blocked row must refuse the waiting_on clear inside the transaction, got %v", ae)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Nothing was written: the blocked row keeps its waiting object.
	var status string
	var waiting sql.NullString
	if err := m.db.QueryRow(`SELECT status, waiting_on FROM work_items WHERE id=?1`, d.ID).
		Scan(&status, &waiting); err != nil {
		t.Fatal(err)
	}
	if status != "blocked" || !waiting.Valid || waiting.String != "owner" {
		t.Fatalf("refused patch may change nothing: status=%s waiting_on=%+v", status, waiting)
	}

	// Positive control A: the same transactional half on a QUEUED ticket
	// applies the clear (unblock-adjacent flows and ordinary PATCHes stay
	// legal on non-blocked statuses).
	d2, err := m.Create(CreateRequest{Project: "p1", Goal: "queued control", Reporter: "usr_worker"})
	if err != nil {
		t.Fatal(err)
	}
	tx2, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.patchTx(tx2, d2.ID, []string{"waiting_on=NULL"}, nil, true); err != nil {
		t.Fatalf("clearing on a queued WI must apply: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatal(err)
	}
	if s := wiStatusForTest(t, m, d2.ID); s != "queued" {
		t.Fatalf("queued control: status changed, got %s", s)
	}

	// Positive control B: a NON-clearing patch on the blocked row (re-pointing
	// the waiting object, R2C-F10's legal branch) still applies in-transaction.
	tx3, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.patchTx(tx3, d.ID, []string{"waiting_on=?1"}, []any{"runtime:mac_a"}, false); err != nil {
		t.Fatalf("re-pointing waiting_on on a blocked WI must apply: %v", err)
	}
	if err := tx3.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := m.db.QueryRow(`SELECT status, waiting_on FROM work_items WHERE id=?1`, d.ID).
		Scan(&status, &waiting); err != nil {
		t.Fatal(err)
	}
	if status != "blocked" || !waiting.Valid || waiting.String != "runtime:mac_a" {
		t.Fatalf("re-pointed blocked row wrong: status=%s waiting_on=%+v", status, waiting)
	}
}

// TestPatchTxRefusesTerminalWonRace: the symmetric interleave — the outer
// read saw a non-terminal status, a cancel/wrap committed a terminal one in
// the window, and the transactional half must not touch the terminal row.
func TestPatchTxRefusesTerminalWonRace(t *testing.T) {
	m := seedPatchRace(t)
	d, err := m.Create(CreateRequest{Project: "p1", Goal: "terminal race fixture", Reporter: "usr_worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`UPDATE work_items SET status='cancelled', closed_at='2026-01-01T00:00:30Z' WHERE id=?1`, d.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = m.patchTx(tx, d.ID, []string{"goal=?1"}, []any{"patched too late"}, false)
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-410" {
		t.Fatalf("terminal row must be refused CORE-410 inside the transaction, got %v", ae)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var goal string
	if err := m.db.QueryRow(`SELECT goal FROM work_items WHERE id=?1`, d.ID).Scan(&goal); err != nil {
		t.Fatal(err)
	}
	if goal != "terminal race fixture" {
		t.Fatalf("refused patch may not rewrite the goal, got %q", goal)
	}
}
