package owner

// White-box regression test for the R2C-F4 reverse claim/cancel race
// (02 §2.3/§2.5/§6.5). Claim runs its prechecks (status validation, bundle
// check, active-generation absence) on the shared connection BEFORE opening
// the claim transaction; a Cancel can commit `cancelled` in that window.
// claimNew is the transactional half, so this test drives it directly with
// exactly the state the race produces — a WI the outer prechecks saw as
// queued that is now terminal — and asserts the in-transaction re-check:
// typed CORE-410, no generation inserted, no running write, no claimed
// event, no idempotency row. Mirrors internal/core/workitem's
// cancel_race_test.go (the forward direction of the same interleave).

import (
	"database/sql"
	"testing"

	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
)

// seedClaimRace builds a manager backed by a REAL registered service
// (claimNew signs the generation token through the transaction, so a nil
// service would panic) and one queued EXECUTION ticket.
func seedClaimRace(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc, err := service.Register(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(db, svc)
	for _, q := range []string{
		`INSERT INTO users(id, display_name, kind, role) VALUES ('usr_worker', 'worker', 'human', 'writer')`,
		`INSERT INTO projects(id, name) VALUES ('pj1', 'p1')`,
		// Execution ticket: bundle three-column triple must be all non-NULL
		// (02 §4.2 CHECK), or the row would be an unclaimable registration
		// ticket and the refusal under test would fire for the wrong reason.
		`INSERT INTO work_items(id, seq, project_id, goal, status, reporter, bundle_name, bundle_digest,
			workflow_budget_baseline, created_at, updated_at)
			VALUES ('wi_race', 1, 'pj1', 'race fixture', 'queued', 'usr_worker', 'demo',
			'` + digest64 + `', '{"requests":100,"tokens":null,"wall_minutes":null}',
			'2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return m, "wi_race"
}

const digest64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestClaimNewRefusesCancelledWI: the outer Claim prechecks observed
// `queued` (the only state in which they would have proceeded to claimNew),
// a Cancel committed `cancelled` in the window before the transaction
// opened, and the transactional half now observes the terminal state. It
// must refuse CORE-410 and write NOTHING — resurrecting the cancelled WI
// with a live generation would violate 02 §2.3/§6.5 (cancel acts only on
// WIs with no active generation) and §2.5 (no transition out of a terminal
// state).
func TestClaimNewRefusesCancelledWI(t *testing.T) {
	m, wi := seedClaimRace(t)
	// The cancel that won the race: queued -> cancelled, terminal row.
	if _, err := m.db.Exec(`UPDATE work_items SET status='cancelled', closed_at='2026-01-01T00:00:30Z',
		updated_at='2026-01-01T00:00:30Z' WHERE id=?1`, wi); err != nil {
		t.Fatal(err)
	}

	_, err := m.claimNew(wi, ClaimRequest{IdempotencyKey: "race-key"}, "usr_worker", nil)
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-410" {
		t.Fatalf("cancelled WI must be refused CORE-410 inside the claim transaction, got %v", ae)
	}

	// Nothing was written: no generation row, status stays cancelled, no
	// claimed event, no idempotency row.
	var gens, events, idem int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1`, wi).Scan(&gens); err != nil {
		t.Fatal(err)
	}
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM wi_events WHERE work_item_id=?1`, wi).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM claim_idempotency WHERE work_item_id=?1`, wi).Scan(&idem); err != nil {
		t.Fatal(err)
	}
	if gens != 0 || events != 0 || idem != 0 {
		t.Fatalf("refused claim may write nothing: gens=%d events=%d idem=%d", gens, events, idem)
	}
	var status string
	var closed sql.NullString
	if err := m.db.QueryRow(`SELECT status, closed_at FROM work_items WHERE id=?1`, wi).Scan(&status, &closed); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || !closed.Valid {
		t.Fatalf("WI must stay cancelled: status=%s closed_at=%+v", status, closed)
	}
}

// TestClaimNewRefusesRunningWIWithActiveGeneration: the symmetric loser
// state of two concurrent claims — the outer prechecks of the second saw
// "no active generation", the first claim committed running + generation 1
// in the window. The in-transaction re-check must answer the typed
// ALREADY_CLAIMED, not insert generation 2 underneath the live generation.
func TestClaimNewRefusesRunningWIWithActiveGeneration(t *testing.T) {
	m, wi := seedClaimRace(t)
	// The claim that won the race: running + active generation row.
	if _, err := m.db.Exec(`UPDATE work_items SET status='running' WHERE id=?1`, wi); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at)
		VALUES (?1, 1, 'usr_worker', '2026-01-01T00:00:00Z')`, wi); err != nil {
		t.Fatal(err)
	}

	_, err := m.claimNew(wi, ClaimRequest{IdempotencyKey: "race-key-2"}, "usr_worker", nil)
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-409" || ae.Reason != "ALREADY_CLAIMED" {
		t.Fatalf("running WI must be refused CORE-409 ALREADY_CLAIMED inside the claim transaction, got %v", ae)
	}
	var gens int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1`, wi).Scan(&gens); err != nil {
		t.Fatal(err)
	}
	if gens != 1 {
		t.Fatalf("no second generation may be inserted, got %d rows", gens)
	}
}

// TestClaimNewPositiveControl: against the state the outer prechecks also
// saw (still queued), the transactional half claims normally — generation 1
// opened, status flipped to running, token signed, idempotency row cached.
func TestClaimNewPositiveControl(t *testing.T) {
	m, wi := seedClaimRace(t)
	resp, err := m.claimNew(wi, ClaimRequest{IdempotencyKey: "ok-key"}, "usr_worker", nil)
	if err != nil {
		t.Fatalf("queued execution ticket must claim: %v", err)
	}
	if resp.Generation != 1 {
		t.Fatalf("first generation must be 1, got %d", resp.Generation)
	}
	if status := wiStatus(t, m, wi); status != "running" {
		t.Fatalf("WI must be running, got %s", status)
	}
	var active int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1 AND ended_at IS NULL`, wi).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("exactly one active generation expected, got %d", active)
	}
	var idem int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM claim_idempotency WHERE work_item_id=?1`, wi).Scan(&idem); err != nil {
		t.Fatal(err)
	}
	if idem != 1 {
		t.Fatalf("idempotency row expected, got %d", idem)
	}
}
