package owner

// unique_violation_test.go: regression for the transaction-race armor's
// SENSOR and its mapping branch in claimNew (02 §6.1, the uq_owner_gen_active
// partial unique index; owner.go claimNew INSERT → isUniqueViolation).
//
// Reachability analysis (measured, modernc.org/sqlite + WAL + deferred
// BEGIN, 2026-09-28): the real cross-connection claim race does NOT reach
// the unique-violation branch on this driver — a claimant whose read
// snapshot predates the winner's commit gets SQLITE_BUSY_SNAPSHOT
// ("database is locked (517)") on its INSERT instead, and a claimant
// whose reads postdate the commit is caught by the in-transaction
// re-check (the ALREADY_CLAIMED branch claim_race_test pins). The
// unique-violation branch is armor for the interleavings where the INSERT
// itself meets the index: a driver/tx mode that upgrades without a stale
// read snapshot (BEGIN IMMEDIATE), or a driver without WAL snapshot
// isolation. Two properties must therefore be pinned independently:
//
//  1. the SENSOR must keep matching the driver's REAL unique-violation
//     message (modernc: "constraint failed: UNIQUE constraint failed:
//     <index> (<code>)") — if the driver changes wording, the armor dies
//     silently and only this test notices;
//  2. claimNew's mapping branch must translate such a failure into the
//     SAME typed CORE-409 ALREADY_CLAIMED the re-check produces, writing
//     nothing. This is exercised with a REAL driver error raised through
//     SQLite's own constraint machinery (RAISE(ABORT) in a BEFORE INSERT
//     trigger whose message is the driver's unique-violation shape), so
//     claimNew runs its real code path — prechecks, re-check (which sees a
//     clean state), INSERT, sensor, typed refusal, rollback.

import (
	"errors"
	"strings"
	"testing"
)

// realUniqueViolation provokes the actual driver error the sensor exists
// to detect: a second active generation row for one work item refused by
// the uq_owner_gen_active partial unique index. Returned by value through
// err.Error() assertions only (no dependency on the concrete driver type).
func realUniqueViolation(t *testing.T) error {
	t.Helper()
	m, wi := seedClaimRace(t)
	if _, err := m.db.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at)
		VALUES (?1, 1, 'usr_worker', '2026-01-01T00:00:00Z')`, wi); err != nil {
		t.Fatalf("seed winner generation: %v", err)
	}
	_, err := m.db.Exec(`INSERT INTO owner_generations(work_item_id, generation, owner, started_at)
		VALUES (?1, 2, 'usr_other', '2026-01-01T00:00:01Z')`, wi)
	if err == nil {
		t.Fatal("the duplicate active-generation insert must fail")
	}
	return err
}

// TestIsUniqueViolationSensor pins the sensor against the driver's real
// unique-violation message and its negatives. The BUSY variants are the
// errors the measured cross-connection race actually produces (see the
// file preamble): they must NOT be classified as unique violations, or
// every lock-loss in production would masquerade as ALREADY_CLAIMED.
func TestIsUniqueViolationSensor(t *testing.T) {
	real := realUniqueViolation(t)
	if msg := real.Error(); !strings.Contains(strings.ToUpper(msg), "UNIQUE CONSTRAINT") {
		t.Fatalf("fixture did not produce a unique-violation-shaped driver error: %v", real)
	}
	if !isUniqueViolation(real) {
		t.Fatalf("the driver's real unique-violation error must be detected: %v", real)
	}
	// Case-insensitivity: the driver's wording has been observed both as
	// "UNIQUE constraint failed" and (older SQLite docs style) uppercase.
	if !isUniqueViolation(errors.New("constraint failed: unique constraint failed: owner_generations.work_item_id (2067)")) {
		t.Fatal("lowercase driver wording must still match")
	}
	// Negatives: no error; the lock errors the real race yields; a generic
	// constraint failure of another class; the typed answer itself.
	if isUniqueViolation(nil) {
		t.Fatal("nil must not be a unique violation")
	}
	for _, raw := range []string{
		"database is locked (5) (SQLITE_BUSY)",
		"database is locked (517)",
		"constraint failed: FOREIGN KEY constraint failed (787)",
		"CHECK constraint failed (1299)",
	} {
		if isUniqueViolation(errors.New(raw)) {
			t.Fatalf("non-unique driver error misclassified: %s", raw)
		}
	}
	if isUniqueViolation(errf("CORE-409", "ALREADY_CLAIMED", "an active generation exists")) {
		t.Fatal("the typed ALREADY_CLAIMED answer itself must not re-match the sensor")
	}
}

// TestClaimNewUniqueViolationMapsTypedAlreadyClaimed drives claimNew's
// real code path through the sensor branch: the WI is claimable (the
// in-transaction re-check sees no active generation), but the INSERT into
// owner_generations fails with a unique-violation-shaped driver error —
// the exact TOCTOU loss the armor describes. The conflicting insert is
// injected through SQLite's own constraint machinery (a BEFORE INSERT
// trigger raising the driver's unique-violation message), so the failure
// the branch sees is a real driver error, not a synthesized string.
func TestClaimNewUniqueViolationMapsTypedAlreadyClaimed(t *testing.T) {
	m, wi := seedClaimRace(t)
	if _, err := m.db.Exec(`CREATE TRIGGER trg_inject_unique_race
		BEFORE INSERT ON owner_generations
		WHEN NEW.work_item_id = 'wi_race'
		BEGIN
			SELECT RAISE(ABORT, 'UNIQUE constraint failed: owner_generations.work_item_id (injected conflicting claim)');
		END`); err != nil {
		t.Fatalf("install injection trigger: %v", err)
	}
	_, err := m.claimNew(wi, ClaimRequest{IdempotencyKey: "uv-race"}, "usr_worker", nil)
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-409" || ae.Reason != "ALREADY_CLAIMED" {
		t.Fatalf("unique-violation loss must surface as CORE-409 ALREADY_CLAIMED, got %v", ae)
	}
	// The losing transaction wrote NOTHING: no generation row, the WI
	// stays queued, no idempotency row (the loser never cached a claim).
	var gens, idem int
	var status string
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1`, wi).Scan(&gens); err != nil {
		t.Fatal(err)
	}
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM claim_idempotency WHERE work_item_id=?1`, wi).Scan(&idem); err != nil {
		t.Fatal(err)
	}
	if err := m.db.QueryRow(`SELECT status FROM work_items WHERE id=?1`, wi).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if gens != 0 || idem != 0 || status != "queued" {
		t.Fatalf("losing claim must write nothing: gens=%d idem=%d status=%s", gens, idem, status)
	}

	// The injection was the ONLY blocker: with the conflicting claim gone,
	// the same claim succeeds — the fixture (bundle, status, service)
	// was valid all along.
	if _, err := m.db.Exec(`DROP TRIGGER trg_inject_unique_race`); err != nil {
		t.Fatalf("drop injection trigger: %v", err)
	}
	if _, err := m.claimNew(wi, ClaimRequest{IdempotencyKey: "uv-race-2"}, "usr_worker", nil); err != nil {
		t.Fatalf("claim after removing the injected conflict: %v", err)
	}
	var active int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM owner_generations WHERE work_item_id=?1 AND ended_at IS NULL`, wi).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("exactly one active generation after the clean claim, got %d", active)
	}
}
