package core

// Audit-2026-10-13 repro tests (polyforge WI wi_erdKWiSz5Sfv). These
// tests assert the EXPECTED behavior from the audit findings; they are
// expected to FAIL on the current main (0834378-lineage) for exactly
// the reported reason. Once fixed they become the regression tests.

import (
	"testing"
	"time"
)

// B1: RevokeAPIKey must write the revocation TIME into revoked_at.
// Reported: internal/aihub/core/admin.go RevokeAPIKey binds keyID to
// BOTH placeholders of `UPDATE api_keys SET revoked_at=? WHERE id=?1`,
// so revoked_at stores the key id instead of a timestamp.
func TestReproB1RevokedAtIsTimestamp(t *testing.T) {
	env := newEnv(t)

	issued, err := env.Admin.CreateAPIKey(env.UserIDB, "b1-repro", "")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Precondition: revocation succeeds at the API level.
	if err := env.Admin.RevokeAPIKey(issued.Token.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	var revokedAt string
	if err := env.DB.QueryRow(`SELECT revoked_at FROM api_keys WHERE id=?1`,
		issued.Token.ID).Scan(&revokedAt); err != nil {
		t.Fatalf("read revoked_at: %v", err)
	}
	if revokedAt == "" {
		t.Fatal("B1: revoked_at is empty after revocation")
	}
	if revokedAt == issued.Token.ID {
		t.Fatalf("B1: revoked_at stores the KEY ID %q instead of a timestamp — "+
			"the audit-reported double binding of keyID to both placeholders", revokedAt)
	}
	if _, err := time.Parse(time.RFC3339, revokedAt); err != nil {
		t.Fatalf("B1: revoked_at %q is not an RFC3339 timestamp: %v", revokedAt, err)
	}
}
