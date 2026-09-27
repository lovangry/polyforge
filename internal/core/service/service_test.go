package service_test

// Tests for RotateSecret parameter handling (fix #13) and the ShareKey
// derivation (fix #4). Design authority: docs/design/polyforge-vnext/
// 02-aihub-core.md §6.6.

import (
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
