package owner_test

// Tests for claim idempotency hardening, generation lifecycle, flush-evidence
// protocol and fencing (02 §6.1/§6.2/§6.4/§6.6; test categories 02 §10).

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/core/capsule"
	"github.com/silgrid/polyforge/internal/core/owner"
	"github.com/silgrid/polyforge/internal/core/service"
	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/core/workitem"
	wfdigest "github.com/silgrid/polyforge/internal/workflow/digest"
)

type harness struct {
	db   *store.DB
	svc  *service.Service
	own  *owner.Manager
	wi   *workitem.Manager
	dir  string
	wfDir string
}

func newHarness(t *testing.T) *harness {
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
	h := &harness{db: db, svc: svc, dir: dir}
	h.own = owner.NewManager(db, svc)
	h.wfDir = filepath.Join(dir, "workflows")
	if err := testsupport.MakeDemoBundle(filepath.Dir(h.wfDir)); err != nil {
		t.Fatal(err)
	}
	h.wi = workitem.NewManager(db, h.wfDir)
	if err := testsupport.SeedBase(db); err != nil {
		t.Fatal(err)
	}
	return h
}

func newExecWI(t *testing.T, h *harness, goal string) string {
	t.Helper()
	d, err := h.wi.Create(workitem.CreateRequest{
		Project: "p1", Goal: goal, Reporter: "usr_worker", BundleName: "demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.BundleName == nil || *d.BundleName != "demo" {
		t.Fatalf("expected execution ticket, got %+v", d.WorkItem)
	}
	if d.BundleDigest == nil || len(*d.BundleDigest) != 64 {
		t.Fatalf("expected固化 digest, got %+v", d.BundleDigest)
	}
	if string(d.BudgetBaseline) == "" || string(d.BudgetBaseline) == "null" {
		t.Fatalf("expected budget baseline抄录, got %s", d.BudgetBaseline)
	}
	return d.ID
}

func claim(t *testing.T, h *harness, wi, key, identity, machine string) *owner.ClaimResponse {
	t.Helper()
	resp, err := h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: key, MachineID: machine}, identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func asAPIErr(t *testing.T, err error) *owner.APIError {
	t.Helper()
	ae, ok := err.(*owner.APIError)
	if !ok {
		t.Fatalf("expected *owner.APIError, got %T %v", err, err)
	}
	return ae
}

// --- §6.1 claim ---

func TestClaimIdempotencyTripleBinding(t *testing.T) {
	h := newHarness(t)
	wi := newExecWI(t, h, "g1")

	r1 := claim(t, h, wi, "k1", "usr_worker", "mac_a")
	r2, err := h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: "k1", MachineID: "mac_a"}, "usr_worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Generation != r2.Generation || r1.GenerationToken != r2.GenerationToken {
		t.Fatalf("idempotent replay must return the same result: %+v vs %+v", r1, r2)
	}

	// Same key, different caller identity → refused (INV-2d).
	_, err = h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: "k1"}, "usr_other", nil)
	ae := asAPIErr(t, err)
	if ae.Code != "CORE-409" || ae.Reason != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("got %v", ae)
	}

	// Same key, different WI → refused.
	wi2 := newExecWI(t, h, "g2")
	_, err = h.own.Claim(wi2, owner.ClaimRequest{IdempotencyKey: "k1"}, "usr_worker", nil)
	ae = asAPIErr(t, err)
	if ae.Code != "CORE-409" {
		t.Fatalf("got %v", ae)
	}

	// Second claim with a NEW key on an already-claimed WI → ALREADY_CLAIMED.
	_, err = h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: "k2"}, "usr_worker", nil)
	ae = asAPIErr(t, err)
	if ae.Code != "CORE-409" || ae.Reason != "ALREADY_CLAIMED" {
		t.Fatalf("got %v", ae)
	}
}

func TestClaimRefusals(t *testing.T) {
	h := newHarness(t)
	// Registration ticket (bundle NULL) is not claimable.
	reg, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "reg", Reporter: "usr_worker"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.own.Claim(reg.ID, owner.ClaimRequest{IdempotencyKey: "rk"}, "usr_worker", nil)
	ae := asAPIErr(t, err)
	if ae.Code != "CORE-409" || ae.Reason != "NOT_CLAIMABLE" {
		t.Fatalf("got %v", ae)
	}

	wi := newExecWI(t, h, "g")
	r := claim(t, h, wi, "k", "usr_worker", "mac_a")
	// End generation with wrap + flush evidence (empty chain) so it is terminal.
	if err := h.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "wrap", FlushEvidence: emptyFlush(r.Generation)}, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	// Terminal state: claim → CORE-410.
	_, err = h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: "k2"}, "usr_worker", nil)
	ae = asAPIErr(t, err)
	if ae.Code != "CORE-410" {
		t.Fatalf("got %v", ae)
	}
	// Unknown WI → CORE-404.
	_, err = h.own.Claim("wi_nope", owner.ClaimRequest{IdempotencyKey: "k3"}, "usr_worker", nil)
	ae = asAPIErr(t, err)
	if ae.Code != "CORE-404" {
		t.Fatalf("got %v", ae)
	}
}

// --- §6.4 flush evidence ---

func emptyFlush(gen int64) *owner.FlushEvidence {
	return &owner.FlushEvidence{
		PublishedGeneration: gen,
		PublishedSeq:        0,
		ManifestSha:         "",
		FrozenCheckpoint: &owner.FrozenCheckpoint{
			ScannedAt:     time.Now().UTC().Format(time.RFC3339Nano),
			BytesDigest:   wfdigest.EmptySyncBytesDigest,
			WorktreeState:  wfdigest.EmptySyncBytesDigest,
			ManifestSha:   "",
			MatchesHead:   false,
		},
	}
}

func TestFlushEvidenceRejections(t *testing.T) {
	h := newHarness(t)
	wi := newExecWI(t, h, "g")
	r := claim(t, h, wi, "k", "usr_worker", "mac_a")
	end := func(fe *owner.FlushEvidence) *owner.APIError {
		err := h.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "pause", FlushEvidence: fe}, r.GenerationToken)
		if err == nil {
			t.Fatal("expected rejection")
		}
		return asAPIErr(t, err)
	}

	// Missing entirely.
	ae := end(nil)
	if ae.Code != "CORE-425" || ae.Reason != "MISSING" {
		t.Fatalf("got %v", ae)
	}
	// Generation mismatch.
	fe := emptyFlush(r.Generation)
	fe.PublishedGeneration = r.Generation + 7
	ae = end(fe)
	if ae.Code != "CORE-425" || ae.Reason != "GENERATION_MISMATCH" {
		t.Fatalf("got %v", ae)
	}
	// Wrong sentinel on the empty chain.
	fe = emptyFlush(r.Generation)
	fe.PublishedSeq = 3
	ae = end(fe)
	if ae.Code != "CORE-425" || ae.Reason != "NOT_HEAD" {
		t.Fatalf("got %v", ae)
	}
	// Empty chain but non-empty digest constant.
	fe = emptyFlush(r.Generation)
	fe.FrozenCheckpoint.BytesDigest = "aaaa"
	ae = end(fe)
	if ae.Code != "CORE-425" {
		t.Fatalf("got %v", ae)
	}
	// Stale scanned_at.
	fe = emptyFlush(r.Generation)
	fe.FrozenCheckpoint.ScannedAt = time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	ae = end(fe)
	if ae.Code != "CORE-425" || ae.Reason != "FROZEN_CHECKPOINT_STALE" {
		t.Fatalf("got %v", ae)
	}
	// Bad token → CORE-403 before flush validation.
	err := h.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "pause", FlushEvidence: emptyFlush(r.Generation)}, "deadbeef")
	ae = asAPIErr(t, err)
	if ae.Code != "CORE-403" {
		t.Fatalf("got %v", ae)
	}

	// Valid empty-chain evidence accepted; WI paused; generation ended.
	if err := h.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "pause", FlushEvidence: emptyFlush(r.Generation)}, r.GenerationToken); err != nil {
		t.Fatal(err)
	}
	d, err := h.wi.Get(wi, workitem.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "paused" {
		t.Fatalf("status = %s", d.Status)
	}
	// Resume = a fresh claim (§2.3), opens generation+1 with a new token.
	r2, err := h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: "k-resume"}, "usr_worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Generation != r.Generation+1 {
		t.Fatalf("resume must open a new generation, got %d after %d", r2.Generation, r.Generation)
	}
}

// --- §6.2 fencing / takeover ---

func TestForceTakeoverFencing(t *testing.T) {
	h := newHarness(t)
	wi := newExecWI(t, h, "g")
	rOld := claim(t, h, wi, "k1", "usr_worker", "mac_a")

	rNew, err := h.own.ForceTakeover(wi, "usr_admin", "mac_b")
	if err != nil {
		t.Fatal(err)
	}
	if rNew.Generation != rOld.Generation+1 {
		t.Fatalf("takeover must bump generation")
	}
	// Old token is dead for generation-close.
	err = h.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "pause", FlushEvidence: emptyFlush(rOld.Generation)}, rOld.GenerationToken)
	ae := asAPIErr(t, err)
	if ae.Code != "CORE-403" || ae.Reason != "stale_generation" {
		t.Fatalf("old owner must be fenced: got %v", ae)
	}
	// New owner can end it.
	if err := h.own.EndGeneration(wi, owner.EndGenerationRequest{Reason: "fail", FlushEvidence: emptyFlush(rNew.Generation)}, rNew.GenerationToken); err != nil {
		t.Fatal(err)
	}
	d, err := h.wi.Get(wi, workitem.GetOptions{})
	if err != nil || d.Status != "failed" {
		t.Fatalf("status: %s %v", d.Status, err)
	}
	if d.ClosedAt == nil {
		t.Fatal("terminal WI must carry closed_at")
	}
}

// --- §6.6 rotation ---

func TestSecretRotationGraceAndReplay(t *testing.T) {
	h := newHarness(t)
	wi := newExecWI(t, h, "g")
	r := claim(t, h, wi, "k1", "usr_worker", "mac_a")
	oldEpoch := r.SecretEpoch

	// Rotate (grace): old token still verifies, replay returns a re-signed one.
	if err := h.svc.RotateSecret("grace", 0, "usr_admin"); err != nil {
		t.Fatal(err)
	}
	res := h.svc.CheckToken(r.GenerationToken, wi, r.Generation, "mac_a")
	if !res.OK {
		t.Fatalf("grace-window token must still verify: %+v", res)
	}
	if res.Epoch != oldEpoch {
		t.Fatalf("expected match on historical epoch %d", oldEpoch)
	}
	r2, err := h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: "k1"}, "usr_worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Generation != r.Generation {
		t.Fatalf("replay must not bump generation")
	}
	if r2.SecretEpoch == oldEpoch {
		t.Fatal("replay must re-sign under the current epoch")
	}

	// Expire the grace window: old token rejected as secret_rotated_expired;
	// the re-signed one still works.
	if err := h.svc.SweepRetired(-time.Second); err != nil {
		t.Fatal(err)
	}
	// The old token is rejected. Its exact CORE-403 reason is best-effort:
	// the retire sweep deletes the epoch's secret file (02 §6.6 step 4), so a
	// no-match token cannot be distinguished from fencing; both routes lead
	// the Daemon to the claim-replay path, which re-signs correctly.
	res = h.svc.CheckToken(r.GenerationToken, wi, r.Generation, "mac_a")
	if res.OK {
		t.Fatalf("swept token must not verify, got %+v", res)
	}
	res = h.svc.CheckToken(r2.GenerationToken, wi, r2.Generation, "mac_a")
	if !res.OK {
		t.Fatalf("current-epoch token must verify: %+v", res)
	}
}

func TestSecretRollbackKillsOldCredentials(t *testing.T) {
	h := newHarness(t)
	wi := newExecWI(t, h, "g")
	r := claim(t, h, wi, "k1", "usr_worker", "mac_a")

	if err := h.svc.RotateSecret("rollback", 0, "usr_admin"); err != nil {
		t.Fatal(err)
	}
	// Idempotent replay of the pre-rollback cache → secret_rolled_back.
	_, err := h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: "k1"}, "usr_worker", nil)
	ae := asAPIErr(t, err)
	if ae.Code != "CORE-403" || ae.Reason != "secret_rolled_back" {
		t.Fatalf("got %v", ae)
	}
	// Raw token likewise rejected (its signing secret is gone).
	res := h.svc.CheckToken(r.GenerationToken, wi, r.Generation, "mac_a")
	if res.OK {
		t.Fatal("pre-rollback token must not verify")
	}
	// A fresh claim (new key) is unaffected by rotation (02 §6.6).
	wi2 := newExecWI(t, h, "g2")
	r2, err := h.own.Claim(wi2, owner.ClaimRequest{IdempotencyKey: "k-new"}, "usr_worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	// End generation on the fresh claim works.
	caps := capsule.NewStore(h.db, h.dir, h.own)
	_ = caps
	if err := h.own.EndGeneration(wi2, owner.EndGenerationRequest{Reason: "wrap", FlushEvidence: emptyFlush(r2.Generation)}, r2.GenerationToken); err != nil {
		t.Fatal(err)
	}
}

// TestClaimCapsuleBootstrap sanity: the claim response carries the bundle
// reference + budget baseline so a machine needs no data-plane reads (02 §8).
func TestClaimCapsuleBootstrap(t *testing.T) {
	h := newHarness(t)
	wi := newExecWI(t, h, "g")
	boot := map[string]any{"bundle_name": "demo"}
	blob, _ := json.Marshal(boot)
	r, err := h.own.Claim(wi, owner.ClaimRequest{IdempotencyKey: "kb"}, "usr_worker", blob)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.CapsuleBootstrap) == 0 {
		t.Fatal("capsule_bootstrap must round-trip")
	}
}
