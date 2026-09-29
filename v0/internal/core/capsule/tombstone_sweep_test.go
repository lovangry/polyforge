package capsule_test

// tombstone_sweep_test.go: SweepTombstones (05 §2.10 expiry sweep, the
// function body that was 0% while the surrounding tombstone semantics had
// tests). The sweep is the irreversible half of the tombstone lifecycle:
// once hard_delete_at elapses with the row still pending, the capsule
// directory is hard-deleted and the row is committed to 'hard_deleted'.
// The not-yet-expired case matters just as much: an early or repeated
// sweep must NOT delete a row still inside its 24h revert window — that
// would make RevertTombstone's WINDOW_ELAPSED guarantee a lie.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/core/owner"
)

// seedTombstonedWI drives one WI to a terminal wrap and tombstones it,
// leaving a sentinel capsule file behind (the sweep's RemoveAll target —
// asserting a file's disappearance proves deletion better than an empty
// dir's).
func seedTombstonedWI(t *testing.T, hh *h, goal string) string {
	t.Helper()
	wi, r := newClaimedWI(t, hh)
	fe := emptyFlushCapsule(r.Generation)
	if err := hh.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "wrap", FlushEvidence: fe}, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	if err := hh.cap.Tombstone(wi, wi); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(hh.dir, "capsules", wi)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sentinel.md"), []byte("sweep target"), 0o644); err != nil {
		t.Fatal(err)
	}
	return wi
}

func tombstoneRow(t *testing.T, hh *h, wi string) (status string, hardDeleteAt string) {
	t.Helper()
	if err := hh.db.QueryRow(`SELECT status, hard_delete_at FROM capsule_tombstones WHERE work_item_id=?1`, wi).
		Scan(&status, &hardDeleteAt); err != nil {
		t.Fatal(err)
	}
	return status, hardDeleteAt
}

// TestSweepTombstonesKeepsUnexpiredAndDeletesExpired: two terminal WIs,
// both tombstoned, both inside their 24h window.
//
//  1. sweep now → NOTHING is deleted: both rows stay pending and both
//     capsule files survive (the window is a revert guarantee, not a hint);
//  2. backdate one row's hard_delete_at past the present and sweep → that
//     capsule directory is hard-deleted and the row is 'hard_deleted',
//     while the still-fresh row is untouched on BOTH counts;
//  3. a repeated sweep is idempotent: no error, no state change.
func TestSweepTombstonesKeepsUnexpiredAndDeletesExpired(t *testing.T) {
	hh := setup(t)
	fresh := seedTombstonedWI(t, hh, "sweep: fresh window must be kept")
	due := seedTombstonedWI(t, hh, "sweep: elapsed window must be deleted")

	// 1. Inside the window: the sweep must keep everything.
	if err := hh.cap.SweepTombstones(); err != nil {
		t.Fatalf("sweep inside the window: %v", err)
	}
	for _, wi := range []string{fresh, due} {
		if st, _ := tombstoneRow(t, hh, wi); st != "pending" {
			t.Fatalf("%s must stay pending before its window elapses, got %s", wi, st)
		}
		if _, err := os.Stat(filepath.Join(hh.dir, "capsules", wi, "sentinel.md")); err != nil {
			t.Fatalf("%s capsule must survive a pre-window sweep: %v", wi, err)
		}
	}

	// 2. Elapse one window (simulated by backdating hard_delete_at, the
	// same approach as TestRevertTombstoneWindowElapsed).
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := hh.db.Exec(`UPDATE capsule_tombstones SET hard_delete_at=?1 WHERE work_item_id=?2`, past, due); err != nil {
		t.Fatal(err)
	}
	if err := hh.cap.SweepTombstones(); err != nil {
		t.Fatalf("sweep with an expired window: %v", err)
	}
	if st, _ := tombstoneRow(t, hh, due); st != "hard_deleted" {
		t.Fatalf("expired tombstone must be hard_deleted, got %s", st)
	}
	if _, err := os.Stat(filepath.Join(hh.dir, "capsules", due)); !os.IsNotExist(err) {
		t.Fatalf("expired capsule directory must be removed (stat err=%v)", err)
	}
	// The fresh row is untouched by the same sweep run.
	if st, at := tombstoneRow(t, hh, fresh); st != "pending" {
		t.Fatalf("fresh tombstone must stay pending through the same sweep, got %s", st)
	} else if at == past {
		t.Fatal("fresh tombstone's window was overwritten by the sweep")
	}
	if _, err := os.Stat(filepath.Join(hh.dir, "capsules", fresh, "sentinel.md")); err != nil {
		t.Fatalf("fresh capsule must survive: %v", err)
	}

	// 3. Idempotence: sweeping again changes nothing and errors not.
	if err := hh.cap.SweepTombstones(); err != nil {
		t.Fatalf("repeat sweep: %v", err)
	}
	if st, _ := tombstoneRow(t, hh, due); st != "hard_deleted" {
		t.Fatalf("repeat sweep changed the hard_deleted row: %s", st)
	}
	if st, _ := tombstoneRow(t, hh, fresh); st != "pending" {
		t.Fatalf("repeat sweep touched the fresh row: %s", st)
	}
}
