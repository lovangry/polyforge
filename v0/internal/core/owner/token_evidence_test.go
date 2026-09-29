package owner_test

// R2C-F5 regressions (02 §6.6): the swept-vs-stale token classification
// must rest on RECORDED ISSUANCE EVIDENCE, not on the mutable
// claim_idempotency cache. §6.6 pins:
//
//   - secret_rotated_expired is the ONLY reason that makes the Daemon replay
//     the claim (reauthorize, §6.6 step 2); stale_generation means the
//     generation was genuinely taken over — no reauthorization, only local
//     seal-and-exit (§6.4); epoch ≤ R (rollback boundary) is
//     secret_rolled_back, permanent, the same judgment for token checks and
//     claim replays.
//   - §6.6 step 4 mandates SweepRetired DELETE the retired epoch's secret
//     file, so post-sweep the HMAC is unrecomputable and classification
//     needs the recorded epoch binding.
//
// The cache cannot be that record: the §6.6 reauthorize re-sign overwrites
// response_json (erasing the previously-swept token), and takeover-issued
// tokens never enter it. The generation_token_issues table (one immutable
// row per issuance) is the authority; the cache survives only as a
// fallback for pre-evidence tokens.

import (
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/core/owner"
)

// TestSweptTokenClassifiesAfterReauthorizeReplay: the lost-response replay
// case. The Daemon replays the claim after the sweep; the re-sign overwrites
// the cached response, so the swept-epoch token is no longer the cached one.
// A late write still carrying the swept token must STILL read
// secret_rotated_expired — never stale_generation, which would make the WI
// Agent seal locally and exit instead of reporting for reauthorization.
func TestSweptTokenClassifiesAfterReauthorizeReplay(t *testing.T) {
	h := newHarness(t)
	wi := newExecWI(t, h, "g")
	r := claim(t, h, wi, "k1", "usr_worker", "mac_a")
	oldToken := r.GenerationToken

	// Grace-rotate, then sweep BEFORE any replay: the token's epoch is
	// factually retired and its secret file is gone (§6.6 step 4).
	if err := h.svc.RotateSecret("grace", 0, "usr_admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SweepRetired(-time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, reason, err := h.own.CheckTokenForWI(wi, oldToken); err != nil || ok || reason != "secret_rotated_expired" {
		t.Fatalf("pre-replay classification: ok=%v reason=%q err=%v", ok, reason, err)
	}

	// The §6.6 reauthorize replay: same key, re-signs under the current
	// epoch and OVERWRITES the cached response — the exact mutation that
	// used to erase the swept token's classification evidence.
	r2, err := h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: "k1"}, "usr_worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Generation != r.Generation {
		t.Fatalf("replay must not bump generation: %d vs %d", r2.Generation, r.Generation)
	}
	// The cached response now carries the NEW token only.
	var cached string
	if err := h.db.QueryRow(`SELECT response_json FROM claim_idempotency WHERE idempotency_key='k1'`).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if want := `"` + r2.GenerationToken + `"`; !contains(cached, want) || contains(cached, `"`+oldToken+`"`) {
		t.Fatalf("setup: cache must now hold only the re-signed token")
	}

	// The swept token must STILL classify secret_rotated_expired after the
	// cache overwrite: the issuance-evidence row recorded at claim time is
	// immutable and survives the reauthorization.
	if _, ok, reason, err := h.own.CheckTokenForWI(wi, oldToken); err != nil || ok {
		t.Fatalf("swept token must not verify: ok=%v err=%v", ok, err)
	} else if reason != "secret_rotated_expired" {
		t.Fatalf("post-replay classification must stay secret_rotated_expired, got %q", reason)
	}
	// And the re-signed token verifies normally.
	if _, ok, reason, err := h.own.CheckTokenForWI(wi, r2.GenerationToken); err != nil || !ok || reason != "" {
		t.Fatalf("re-signed token must verify: ok=%v reason=%q err=%v", ok, reason, err)
	}
}

// TestTakeoverIssuedTokenClassifiesAfterSweep: the takeover-issued-token
// case. ForceTakeover mints a token that has NO claim_idempotency row at
// all; once its epoch is swept, the token must read secret_rotated_expired
// from its evidence row — previously it fell to the cache miss and read
// stale_generation, wrongly forbidding the reauthorization path.
func TestTakeoverIssuedTokenClassifiesAfterSweep(t *testing.T) {
	h := newHarness(t)
	wi := newExecWI(t, h, "g")
	r := claim(t, h, wi, "k1", "usr_worker", "mac_a")

	tk, err := h.own.ForceTakeover(wi, "usr_admin", "mac_b")
	if err != nil {
		t.Fatal(err)
	}
	// Setup check: the takeover token really has no cache entry (that is
	// the defect's premise), but it does have an evidence row — carrying the
	// token's sha256 digest, never the raw token.
	var rows int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM claim_idempotency WHERE work_item_id=?1`, wi).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	var cachedTokens int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM claim_idempotency WHERE work_item_id=?1
		AND response_json LIKE ?2`, wi, "%"+tk.GenerationToken+"%").Scan(&cachedTokens); err != nil {
		t.Fatal(err)
	}
	if cachedTokens != 0 {
		t.Fatalf("setup: takeover token must not live in the idempotency cache (rows=%d)", rows)
	}
	var evEpoch int
	var evToken string
	if err := h.db.QueryRow(`SELECT secret_epoch, token_sha FROM generation_token_issues
		WHERE work_item_id=?1 AND generation=?2`, wi, tk.Generation).Scan(&evEpoch, &evToken); err != nil {
		t.Fatalf("takeover token must have an evidence row: %v", err)
	}
	if evEpoch != tk.SecretEpoch {
		t.Fatalf("evidence epoch %d != issued epoch %d", evEpoch, tk.SecretEpoch)
	}
	if len(evToken) != 64 || contains(evToken, tk.GenerationToken[:16]) {
		t.Fatalf("evidence must store the token digest, not the token: %q", evToken)
	}

	// Sweep the takeover token's epoch.
	if err := h.svc.RotateSecret("grace", 0, "usr_admin"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SweepRetired(-time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, reason, err := h.own.CheckTokenForWI(wi, tk.GenerationToken); err != nil || ok {
		t.Fatalf("swept takeover token must not verify: ok=%v err=%v", ok, err)
	} else if reason != "secret_rotated_expired" {
		t.Fatalf("swept takeover token must read secret_rotated_expired, got %q", reason)
	}

	// Negative controls: a garbage token still reads stale_generation (no
	// evidence row, no cache row), and the DEAD generation's swept token —
	// even though its evidence row exists — stays stale_generation because
	// the evidence lookup is scoped to the ACTIVE generation: a fenced
	// owner must never get a reauthorization signal.
	if _, ok, reason, err := h.own.CheckTokenForWI(wi, "deadbeef"); err != nil || ok || reason != "stale_generation" {
		t.Fatalf("garbage token: ok=%v reason=%q err=%v", ok, reason, err)
	}
	if _, ok, reason, err := h.own.CheckTokenForWI(wi, r.GenerationToken); err != nil || ok || reason != "stale_generation" {
		t.Fatalf("dead generation's token must stay stale_generation, got ok=%v reason=%q err=%v", ok, reason, err)
	}
}

// TestTakeoverIssuedTokenRollbackReadsRolledBack: the same evidence path
// under mode=rollback — §6.6 pins epoch ≤ R as secret_rolled_back for
// token checks and claim replays alike, and a takeover token must reach
// that verdict through its evidence row even though the rollback deleted
// its secret file.
func TestTakeoverIssuedTokenRollbackReadsRolledBack(t *testing.T) {
	h := newHarness(t)
	wi := newExecWI(t, h, "g")
	claim(t, h, wi, "k1", "usr_worker", "mac_a")
	tk, err := h.own.ForceTakeover(wi, "usr_admin", "mac_b")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RotateSecret("rollback", 0, "usr_admin"); err != nil {
		t.Fatal(err)
	}
	if _, ok, reason, err := h.own.CheckTokenForWI(wi, tk.GenerationToken); err != nil || ok {
		t.Fatalf("rolled-back takeover token must not verify: ok=%v err=%v", ok, err)
	} else if reason != "secret_rolled_back" {
		t.Fatalf("pre-rollback takeover token must read secret_rolled_back, got %q", reason)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
