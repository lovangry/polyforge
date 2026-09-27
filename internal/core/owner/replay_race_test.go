package owner

// White-box regression test for the R3C-F1 replay/rollback interleave
// (02 §6.6). replayOrReclassify used to read the rollback boundary, the
// active generation and the current epoch and sign the re-issue token on
// the shared connection BEFORE opening its write transaction: a
// mode=rollback rotation committing in that window was invisible to the
// stale boundary check, and the cached pre-rollback claim was re-signed
// with the NEW live secret and committed — reviving the pre-rollback
// generation without the admin force-takeover §6.6 makes the only revival
// path. replayReauthorizeTx is the transactional half (all checks, the
// re-sign and both writes on one transaction), so this test drives it
// directly with exactly the state the interleave produces — an outer half
// that already passed its checks against the pre-rollback world — and
// asserts the in-transaction boundary re-check: typed CORE-403
// secret_rolled_back, cache row untouched, no reauthorize evidence.
// Mirrors claim_race_test.go / endgen_race_test.go / takeover_race_test.go.

import (
	"database/sql"
	"encoding/json"
	"testing"
)

func TestReplayReauthorizeTxRefusesRollbackCommittedInWindow(t *testing.T) {
	m, wi := seedClaimRace(t)

	// The outer half's own read: the cached claim the interleave starts
	// from, exactly as Claim's idempotency hit would hand it over — bound
	// to generation 1 and the (then current) epoch 1, with the boundary
	// check about to observe R=0 because no rollback has committed yet.
	resp, err := m.claimNew(wi, ClaimRequest{IdempotencyKey: "race-key"}, "usr_worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.SecretEpoch != 1 {
		t.Fatalf("fixture: cached claim must bind epoch 1, got %d", resp.SecretEpoch)
	}
	var cachedBefore string
	if err := m.db.QueryRow(`SELECT response_json FROM claim_idempotency
		WHERE idempotency_key='race-key' AND work_item_id=?1`, wi).Scan(&cachedBefore); err != nil {
		t.Fatal(err)
	}

	// The rollback that wins the race: it commits in the window AFTER the
	// outer half's boundary read (which saw R=0) and BEFORE the
	// transactional half opens. Epoch 2 is now the boundary R; epoch 1 and
	// its secret file are retired immediately (§6.6 rollback).
	if err := m.svc.RotateSecret("rollback", 0, ""); err != nil {
		t.Fatal(err)
	}

	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.replayReauthorizeTx(tx, wi, "race-key", *resp)
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-403" || ae.Reason != "secret_rolled_back" {
		t.Fatalf("rollback committed in the window must be refused inside the transaction, got %v", ae)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Nothing was written: the cache still holds the pre-rollback cached
	// response verbatim, and no reauthorize evidence row exists.
	var cachedAfter string
	if err := m.db.QueryRow(`SELECT response_json FROM claim_idempotency
		WHERE idempotency_key='race-key' AND work_item_id=?1`, wi).Scan(&cachedAfter); err != nil {
		t.Fatal(err)
	}
	if cachedAfter != cachedBefore {
		t.Fatal("refused replay may not rewrite the idempotency cache")
	}
	var reauth int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM generation_token_issues
		WHERE work_item_id=?1 AND issued_via='reauthorize'`, wi).Scan(&reauth); err != nil {
		t.Fatal(err)
	}
	if reauth != 0 {
		t.Fatalf("refused replay may not record reauthorize evidence, got %d", reauth)
	}
	// The generation was not revived with a live credential either: the
	// pre-rollback token is dead under the post-rollback world.
	if res := m.svc.CheckToken(resp.GenerationToken, wi, resp.Generation, ""); res.OK {
		t.Fatalf("pre-rollback token must not verify after the rollback: %+v", res)
	}
}

// TestReplayReauthorizeTxResignsUnderGraceRotation: the positive control —
// the SAME transactional half, driven with a grace rotation instead of a
// rollback (a rotation that is legal to re-sign across, §6.6 step 4),
// re-signs the cached claim under the current epoch, rewrites the cache
// and records the reauthorize evidence atomically.
func TestReplayReauthorizeTxResignsUnderGraceRotation(t *testing.T) {
	m, wi := seedClaimRace(t)
	resp, err := m.claimNew(wi, ClaimRequest{IdempotencyKey: "grace-key"}, "usr_worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.svc.RotateSecret("grace", 0, ""); err != nil {
		t.Fatal(err)
	}

	tx, err := m.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.replayReauthorizeTx(tx, wi, "grace-key", *resp)
	if err != nil {
		t.Fatalf("grace-rotation replay must re-sign: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if out.Generation != resp.Generation {
		t.Fatalf("re-sign must not bump generation: %d", out.Generation)
	}
	if out.SecretEpoch == resp.SecretEpoch {
		t.Fatal("re-sign must move the cached claim to the current epoch")
	}
	if res := m.svc.CheckToken(out.GenerationToken, wi, out.Generation, ""); !res.OK {
		t.Fatalf("re-signed token must verify: %+v", res)
	}
	// The cache rewrite landed with the new token, and the evidence row
	// with the same epoch.
	var cached string
	if err := m.db.QueryRow(`SELECT response_json FROM claim_idempotency
		WHERE idempotency_key='grace-key' AND work_item_id=?1`, wi).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	var stored ClaimResponse
	if err := json.Unmarshal([]byte(cached), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.GenerationToken != out.GenerationToken || stored.SecretEpoch != out.SecretEpoch {
		t.Fatalf("cache must carry the re-signed token: %+v vs %+v", stored, out)
	}
	var epoch int
	var null sql.NullString
	if err := m.db.QueryRow(`SELECT secret_epoch, issued_at FROM generation_token_issues
		WHERE work_item_id=?1 AND issued_via='reauthorize'`, wi).Scan(&epoch, &null); err != nil {
		t.Fatal(err)
	}
	if epoch != out.SecretEpoch || !null.Valid {
		t.Fatalf("reauthorize evidence must record the re-signed epoch: epoch=%d issued_at=%+v", epoch, null)
	}
}

// TestReplayFullFunctionAfterRollbackRefused: end-to-end through Claim
// itself — the cache hit enters replayOrReclassify and the one-transaction
// decision must refuse with secret_rolled_back once the rollback has
// committed before the replay begins (the §6.6 boundary rule, now enforced
// on the transaction's own snapshot).
func TestReplayFullFunctionAfterRollbackRefused(t *testing.T) {
	m, wi := seedClaimRace(t)
	if _, err := m.claimNew(wi, ClaimRequest{IdempotencyKey: "e2e-key"}, "usr_worker", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.svc.RotateSecret("rollback", 0, ""); err != nil {
		t.Fatal(err)
	}
	_, err := m.Claim(wi, ClaimRequest{IdempotencyKey: "e2e-key"}, "usr_worker", nil)
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Code != "CORE-403" || ae.Reason != "secret_rolled_back" {
		t.Fatalf("post-rollback replay must be refused secret_rolled_back, got %v", ae)
	}
	// Fixture sanity: the rollback boundary is the second epoch.
	if R, err := m.svc.RollbackBoundary(); err != nil || R != 2 {
		t.Fatalf("fixture: expected rollback boundary 2, got %d %v", R, err)
	}
}
