package service_test

// Tests for RotateSecret parameter handling (fix #13) and the ShareKey
// derivation (fix #4). Design authority: docs/design/polyforge-vnext/
// 02-aihub-core.md §6.6.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
)

func newSvc(t *testing.T) *service.Service {
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
	return svc
}

// TestRotateSecretRollbackRejectsNonzeroGrace: mode=rollback with a nonzero
// grace_seconds is a contradictory request and must be refused CORE-400.
func TestRotateSecretRollbackRejectsNonzeroGrace(t *testing.T) {
	svc := newSvc(t)
	err := svc.RotateSecret("rollback", 30, "")
	if err == nil || !strings.HasPrefix(err.Error(), "CORE-400") {
		t.Fatalf("expected CORE-400, got %v", err)
	}
	// grace_seconds=0 is fine for rollback.
	if err := svc.RotateSecret("rollback", 0, ""); err != nil {
		t.Fatalf("rollback with grace_seconds=0 should succeed: %v", err)
	}
}

// TestRotateSecretGraceHonorsCallerGraceSeconds: a caller-supplied
// grace_seconds on mode=grace is persisted and used as SweepRetired's
// default, instead of being silently discarded.
func TestRotateSecretGraceHonorsCallerGraceSeconds(t *testing.T) {
	svc := newSvc(t)
	if err := svc.RotateSecret("grace", 5, ""); err != nil {
		t.Fatal(err)
	}
	got := svc.StoredGraceSeconds()
	if got != 5*time.Second {
		t.Fatalf("expected stored grace of 5s, got %v", got)
	}

	// A zero/unset grace_seconds on a later rotation must NOT reset the
	// previously stored explicit choice back to a bare default overwrite;
	// it is treated as "use the default for THIS rotation" without erasing
	// history — but since setGraceSeconds always persists the effective
	// value, the stored value tracks the LAST rotation's effective grace.
	if err := svc.RotateSecret("grace", 0, ""); err != nil {
		t.Fatal(err)
	}
	if got := svc.StoredGraceSeconds(); got != service.DefaultGraceSeconds*time.Second {
		t.Fatalf("expected default grace after an unspecified rotation, got %v", got)
	}
}

// TestRotateSecretUnknownMode still rejects an unrecognised mode.
func TestRotateSecretUnknownMode(t *testing.T) {
	svc := newSvc(t)
	err := svc.RotateSecret("bogus", 0, "")
	if err == nil || !strings.HasPrefix(err.Error(), "CORE-400") {
		t.Fatalf("got %v", err)
	}
}

// --- FA-3: EpochDisposition classifies an epoch for post-hoc token
// classification (02 §6.6): rolled_back vs retired vs live. ---

func TestEpochDisposition(t *testing.T) {
	svc := newSvc(t)
	e1, err := svc.CurrentEpoch()
	if err != nil {
		t.Fatal(err)
	}
	// Live epoch (no rollback, not retired).
	if d, err := svc.EpochDisposition(e1); err != nil || d != "live" {
		t.Fatalf("current epoch: %s %v", d, err)
	}
	// Grace-rotate then sweep: epoch 1 is retired but NOT rolled back.
	if err := svc.RotateSecret("grace", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.SweepRetired(-time.Second); err != nil {
		t.Fatal(err)
	}
	if d, err := svc.EpochDisposition(e1); err != nil || d != "retired" {
		t.Fatalf("swept grace epoch: %s %v", d, err)
	}
	// A later rollback makes every epoch <= R rolled_back, permanently.
	if err := svc.RotateSecret("rollback", 0, ""); err != nil {
		t.Fatal(err)
	}
	if d, err := svc.EpochDisposition(e1); err != nil || d != "rolled_back" {
		t.Fatalf("post-rollback epoch: %s %v", d, err)
	}
	e2, err := svc.CurrentEpoch()
	if err != nil {
		t.Fatal(err)
	}
	if d, err := svc.EpochDisposition(e2); err != nil || d != "live" {
		t.Fatalf("post-rollback current epoch: %s %v", d, err)
	}
}

// --- fix #4: ShareKey is derived from the actual rotatable secret, not a
// hardcoded public string; a token minted under one epoch's key must not
// validate under a different epoch's key.

func TestShareKeyDiffersAcrossEpochsAndIsSecretDerived(t *testing.T) {
	svc := newSvc(t)
	epoch1, err := svc.CurrentEpoch()
	if err != nil {
		t.Fatal(err)
	}
	key1, err := svc.ShareKey(epoch1)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RotateSecret("grace", 0, ""); err != nil {
		t.Fatal(err)
	}
	epoch2, err := svc.CurrentEpoch()
	if err != nil {
		t.Fatal(err)
	}
	if epoch2 == epoch1 {
		t.Fatal("rotation must bump the epoch")
	}
	key2, err := svc.ShareKey(epoch2)
	if err != nil {
		t.Fatal(err)
	}
	if string(key1) == string(key2) {
		t.Fatal("share keys derived from different epochs' secrets must differ")
	}

	// A hand-rolled "public constant" key (the pre-fix scheme) must not
	// equal the real derived key: nobody without the secret file can
	// reproduce it.
	forged := []byte("pf-share-" + string(rune('0'+epoch1)))
	if string(forged) == string(key1) {
		t.Fatal("share key must not equal the old forgeable public-constant scheme")
	}
}

// --- R3C-F2: retirement accounts for how an epoch was superseded, not
// only for its creation mode (02 §6.6). ---

// TestSweepRetiredRetiresSupersededRollbackEpoch: a rollback-CREATED epoch
// (the boundary R itself, live so post-rollback claims work) that later
// becomes non-current after a grace rotation must be swept once past the
// grace window measured from its supersession. The previous WHERE selected
// mode='grace' rows only, so the rollback epoch's secret file stayed live
// forever and CheckToken kept accepting its tokens beyond any grace.
func newSvcDir(t *testing.T) (*service.Service, string) {
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
	return svc, dir
}

// secretFilePath is the on-disk secret file of an epoch (02 §4.11:
// <data_dir>/service.secret.<epoch>, never in SQLite).
func secretFilePath(epoch int, dataDir string) string {
	return filepath.Join(dataDir, fmt.Sprintf("service.secret.%d", epoch))
}

func TestSweepRetiredRetiresSupersededRollbackEpoch(t *testing.T) {
	svc, dataDir := newSvcDir(t)
	// Epoch 1 (grace) is the initial epoch; rollback creates epoch 2 = R.
	if err := svc.RotateSecret("rollback", 0, ""); err != nil {
		t.Fatal(err)
	}
	e2, err := svc.CurrentEpoch()
	if err != nil {
		t.Fatal(err)
	}
	if R, err := svc.RollbackBoundary(); err != nil || R != e2 {
		t.Fatalf("rollback epoch must be the boundary, got epoch=%d R=%d %v", e2, R, err)
	}
	// A fresh post-rollback claim would sign under epoch 2; its token is
	// live while epoch 2 is current.
	tok, epoch, err := svc.SignToken("wi_f2", 1, "mac_a")
	if err != nil {
		t.Fatal(err)
	}
	if epoch != e2 {
		t.Fatalf("post-rollback signing must use the rollback epoch, got %d", epoch)
	}
	if res := svc.CheckToken(tok, "wi_f2", 1, "mac_a"); !res.OK {
		t.Fatalf("rollback-epoch token must verify while current: %+v", res)
	}

	// Grace rotation supersedes epoch 2; inside the window the token is
	// still accepted (§6.6 step 3), and a real-window sweep keeps it.
	if err := svc.RotateSecret("grace", 0, ""); err != nil {
		t.Fatal(err)
	}
	e3, err := svc.CurrentEpoch()
	if err != nil {
		t.Fatal(err)
	}
	if res := svc.CheckToken(tok, "wi_f2", 1, "mac_a"); !res.OK || res.Epoch != e2 {
		t.Fatalf("grace-window rollback-epoch token must still verify: %+v", res)
	}
	if err := svc.SweepRetired(svc.StoredGraceSeconds()); err != nil {
		t.Fatal(err)
	}
	if res := svc.CheckToken(tok, "wi_f2", 1, "mac_a"); !res.OK {
		t.Fatalf("inside the grace window the rollback-created epoch must stay live: %+v", res)
	}

	// Past the window the sweep retires the rollback-created epoch too:
	// its row is retired, its secret file is gone, and the token no longer
	// matches. The current epoch stays untouched.
	if err := svc.SweepRetired(-time.Second); err != nil {
		t.Fatal(err)
	}
	if d, err := svc.EpochDisposition(e2); err != nil || d != "retired" {
		t.Fatalf("superseded rollback epoch must be retired, got %s %v", d, err)
	}
	if _, err := os.Stat(secretFilePath(e2, dataDir)); !os.IsNotExist(err) {
		t.Fatalf("retired rollback epoch's secret file must be removed, stat err=%v", err)
	}
	if d, err := svc.EpochDisposition(e3); err != nil || d != "live" {
		t.Fatalf("current epoch must stay live, got %s %v", d, err)
	}
	res := svc.CheckToken(tok, "wi_f2", 1, "mac_a")
	if res.OK {
		t.Fatalf("swept rollback-epoch token must no longer verify: %+v", res)
	}
}
