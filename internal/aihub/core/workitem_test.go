package core

// workitem_test.go — the w140 (polyforge#140 split a) unit tests for the
// bundle-reference write face: the shared create/PATCH persist path
// (plan step shared_bundle_path_r2_window) and the R2 window gate.
// Every refusal is typed; every acceptance re-derives the digest through
// bundleRef — the ONE persist path both entries share.

import (
	"encoding/json"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

// mustCreateBound creates a queued execution ticket with the given
// bundle (no explicit digest → stand-in) and returns its id.
func mustCreateBound(t *testing.T, env *testEnv, goal, bundle string) string {
	t.Helper()
	wi, err := env.WIs.Create(CreateRequest{
		Project: "alpha", Goal: goal, Reporter: env.UserID,
		BundleName: strPtrTest(bundle),
	})
	if err != nil {
		t.Fatalf("create bound wi (%s): %v", goal, err)
	}
	return wi.ID
}

// mustClaim claims a WI with a fresh session (core face).
func mustClaim(t *testing.T, env *testEnv, wiID string) string {
	t.Helper()
	secret := "w140-unit-session-secret"
	m := NewSessionManager(env.DB)
	if _, err := m.Claim(wiID, api.SessionClaimRequest{SessionID: secret}, env.UserID); err != nil {
		t.Fatalf("claim %s: %v", wiID, err)
	}
	return secret
}

// patchBundle is the typed PATCH helper for bundle fields.
func patchBundle(t *testing.T, env *testEnv, id string, name, digest *string) (*api.WorkItem, error) {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if name != nil {
		raw, _ := json.Marshal(*name)
		fields["bundle_name"] = raw
	} else {
		fields["bundle_name"] = json.RawMessage("null")
	}
	if digest != nil {
		raw, _ := json.Marshal(*digest)
		fields["bundle_digest"] = raw
	}
	return env.WIs.Patch(id, fields)
}

// TestPatchBundleR2ThreeWay walks NULL→X / X→Y / X→NULL inside the R2
// window: the digest follows the shared persist path (explicit pin wins,
// otherwise the stand-in of the NEW name), the unbind clears name AND
// digest together, and workflow_budget_baseline stays JSON null
// throughout.
func TestPatchBundleR2ThreeWay(t *testing.T) {
	env := newEnv(t)

	// NULL→X: stand-in digest of the new name.
	id := mustCreateWI(t, env, "w140 three-way target", nil, "")
	got, err := patchBundle(t, env, id, strPtrTest("w140-x"), nil)
	if err != nil {
		t.Fatalf("NULL→X refused inside the R2 window: %v", err)
	}
	if got.BundleName == nil || *got.BundleName != "w140-x" ||
		got.BundleDigest == nil || *got.BundleDigest != Sha256Hex("bundle-name:w140-x") {
		t.Fatalf("NULL→X: bundle pair = %v/%v, want w140-x/%s",
			got.BundleName, got.BundleDigest, Sha256Hex("bundle-name:w140-x"))
	}
	if got.BudgetBaseline != nil {
		t.Fatalf("NULL→X: workflow_budget_baseline = %s, want JSON null", got.BudgetBaseline)
	}

	// X→Y without an explicit digest: the stand-in re-derives.
	got, err = patchBundle(t, env, id, strPtrTest("w140-y"), nil)
	if err != nil {
		t.Fatalf("X→Y refused inside the R2 window: %v", err)
	}
	if got.BundleDigest == nil || *got.BundleDigest != Sha256Hex("bundle-name:w140-y") {
		t.Fatalf("X→Y: digest = %v, want stand-in of w140-y", got.BundleDigest)
	}

	// X→Y with an explicit digest: the request pin wins (same path).
	got, err = patchBundle(t, env, id, strPtrTest("w140-z"), strPtrTest("w140-explicit-pin"))
	if err != nil {
		t.Fatalf("X→Y with explicit digest refused: %v", err)
	}
	if got.BundleDigest == nil || *got.BundleDigest != "w140-explicit-pin" {
		t.Fatalf("X→Y explicit digest: digest = %v, want the request pin", got.BundleDigest)
	}

	// X→NULL: name AND digest clear together; status stays queued.
	got, err = patchBundle(t, env, id, nil, nil)
	if err != nil {
		t.Fatalf("X→NULL (unbind) refused inside the R2 window: %v", err)
	}
	if got.BundleName != nil || got.BundleDigest != nil {
		t.Fatalf("unbind left the reference half-set: %v/%v", got.BundleName, got.BundleDigest)
	}
	if got.Status != api.StatusQueued {
		t.Fatalf("unbind wrote a status: %s", got.Status)
	}
	if got.BudgetBaseline != nil {
		t.Fatalf("unbind: workflow_budget_baseline = %s, want JSON null", got.BudgetBaseline)
	}

	// digest-only and empty-name shapes are typed bad input.
	if _, err := env.WIs.Patch(id, map[string]json.RawMessage{"bundle_digest": json.RawMessage(`"x"`)}); !IsCode(err, api.ErrCoreBadInput) {
		t.Fatalf("digest without name: want CORE-400, got %v", err)
	}
	if _, err := patchBundle(t, env, id, strPtrTest(""), nil); !IsCode(err, api.ErrCoreBadInput) {
		t.Fatalf("empty bundle_name: want CORE-400, got %v", err)
	}
}

// TestPatchBundleWindowRefusals: outside the R2 window the PATCH is
// refused with the IMMUTABLE/TERMINAL_STATE family and writes nothing —
// running (live session), failed (terminal after a claim), canceled
// (terminal without a claim), and force-ended (the claim event outlives
// the cleared session row).
func TestPatchBundleWindowRefusals(t *testing.T) {
	env := newEnv(t)
	m := NewSessionManager(env.DB)

	// running (claimed, live session) → IMMUTABLE.
	running := mustCreateBound(t, env, "w140 running refusal", "w140-w-r")
	mustClaim(t, env, running)
	if _, err := patchBundle(t, env, running, strPtrTest("w140-w-r2"), nil); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMMUTABLE" {
		t.Fatalf("running: want CORE-400/IMMUTABLE, got %v", err)
	}

	// failed (terminal after a claim) → TERMINAL_STATE.
	failed := mustCreateBound(t, env, "w140 failed refusal", "w140-w-f")
	secret := mustClaim(t, env, failed)
	if err := m.End(failed, api.SessionEndRequest{SessionID: secret, Status: api.StatusFailed}, env.UserID); err != nil {
		t.Fatalf("fail %s: %v", failed, err)
	}
	if _, err := patchBundle(t, env, failed, strPtrTest("w140-w-f2"), nil); !IsCode(err, api.ErrCoreGone) || ErrorReason(err) != "TERMINAL_STATE" {
		t.Fatalf("failed: want CORE-410/TERMINAL_STATE, got %v", err)
	}

	// canceled (terminal, never claimed) → TERMINAL_STATE.
	canceled := mustCreateBound(t, env, "w140 canceled refusal", "w140-w-c")
	if _, err := env.WIs.Cancel(canceled, env.UserID); err != nil {
		t.Fatalf("cancel %s: %v", canceled, err)
	}
	if _, err := patchBundle(t, env, canceled, strPtrTest("w140-w-c2"), nil); !IsCode(err, api.ErrCoreGone) || ErrorReason(err) != "TERMINAL_STATE" {
		t.Fatalf("canceled: want CORE-410/TERMINAL_STATE, got %v", err)
	}

	// force-ended back to queued: the session row is cleared but the
	// claim event remains — the window stays closed (never-claimed means
	// no claim event, not merely no live session).
	requeued := mustCreateBound(t, env, "w140 force-end refusal", "w140-w-r3")
	mustClaim(t, env, requeued)
	if err := m.ForceEnd(requeued, "w140 unit force-end", env.UserID); err != nil {
		t.Fatalf("force-end %s: %v", requeued, err)
	}
	if _, err := patchBundle(t, env, requeued, strPtrTest("w140-w-r4"), nil); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMMUTABLE" {
		t.Fatalf("force-ended (claim event remains): want CORE-400/IMMUTABLE, got %v", err)
	}

	// every refusal above wrote nothing: the surviving references are
	// the create-time ones.
	for id, want := range map[string]string{running: "w140-w-r", failed: "w140-w-f", canceled: "w140-w-c", requeued: "w140-w-r3"} {
		wi, err := env.WIs.Get(id, GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if wi.BundleName == nil || *wi.BundleName != want {
			t.Fatalf("refused PATCH wrote on %s: bundle_name = %v, want %s", id, wi.BundleName, want)
		}
	}
}

// TestPatchBundleWaitingOnNotRefused: waiting_on non-empty does not
// refuse the bundle PATCH on its own — the window looks only at queued
// ∧ never-claimed — and the PATCH leaves waiting_on untouched.
func TestPatchBundleWaitingOnNotRefused(t *testing.T) {
	env := newEnv(t)
	id := mustCreateWI(t, env, "w140 waiting_on patch", nil, "")
	if _, err := env.WIs.Patch(id, map[string]json.RawMessage{"waiting_on": json.RawMessage(`"owner"`)}); err != nil {
		t.Fatalf("patch waiting_on: %v", err)
	}
	got, err := patchBundle(t, env, id, strPtrTest("w140-wait-b"), nil)
	if err != nil {
		t.Fatalf("bundle PATCH with waiting_on non-empty refused: %v", err)
	}
	if got.WaitingOn == nil || *got.WaitingOn != "owner" {
		t.Fatalf("waiting_on = %v, want owner (untouched)", got.WaitingOn)
	}
	if got.BundleName == nil || *got.BundleName != "w140-wait-b" {
		t.Fatalf("bundle_name = %v, want w140-wait-b", got.BundleName)
	}
}

// TestPatchBundleRepoInventoryUntouched: the shared persist path does
// not touch the §4.6 frozen repo triple — a WI created with an
// inventory keeps it verbatim through bundle binds and unbinds.
func TestPatchBundleRepoInventoryUntouched(t *testing.T) {
	env := newEnv(t)
	wi, err := env.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "w140 inventory untouched", Reporter: env.UserID,
		BundleName:        strPtrTest("w140-inv-a"),
		RepoInventory:     []RepoDecl{decl("polyforge", "https://example.com/polyforge.git")},
		RepoInventoryBase: []RepoBaseDecl{baseDecl("polyforge", "0123456789abcdef")},
	})
	if err != nil {
		t.Fatalf("create with inventory: %v", err)
	}
	before, err := env.WIs.repoInventoryOf(wi.ID)
	if err != nil {
		t.Fatalf("inventory read: %v", err)
	}
	if _, err := patchBundle(t, env, wi.ID, strPtrTest("w140-inv-b"), nil); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := patchBundle(t, env, wi.ID, nil, nil); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	after, err := env.WIs.repoInventoryOf(wi.ID)
	if err != nil {
		t.Fatalf("inventory read: %v", err)
	}
	if len(after) != len(before) || len(after) != 1 ||
		after[0].Name != before[0].Name || after[0].Base != before[0].Base {
		t.Fatalf("bundle PATCH mutated the frozen inventory: before %+v after %+v", before, after)
	}
}

// TestPatchBundleWithChildrenRefused: the symmetric children-side rule —
// a ticket WITH children never gains a bundle, children counting in any
// status (terminal included), and the refusal writes nothing.
func TestPatchBundleWithChildrenRefused(t *testing.T) {
	// The children side: file a real child under the parent.
	env2 := newEnv(t)
	p2, err := env2.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "w140 parent", Reporter: env2.UserID,
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	c2, err := env2.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "w140 child", Reporter: env2.UserID, ParentID: strPtrTest(p2.ID),
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if _, err := patchBundle(t, env2, p2.ID, strPtrTest("w140-sym-p"), nil); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "HAS_CHILDREN_NO_BUNDLE" {
		t.Fatalf("bind on parent with children: want CORE-400/HAS_CHILDREN_NO_BUNDLE, got %v", err)
	}
	// ... and a terminal child still counts as a child.
	if _, err := env2.WIs.Cancel(c2.ID, env2.UserID); err != nil {
		t.Fatalf("cancel child: %v", err)
	}
	if _, err := patchBundle(t, env2, p2.ID, strPtrTest("w140-sym-p"), nil); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "HAS_CHILDREN_NO_BUNDLE" {
		t.Fatalf("bind on parent whose child is terminal: want CORE-400/HAS_CHILDREN_NO_BUNDLE, got %v", err)
	}
	got, err := env2.WIs.Get(p2.ID, GetOptions{})
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if got.BundleName != nil {
		t.Fatalf("refused PATCH wrote: bundle_name = %v, want null", got.BundleName)
	}

	// The create-side mirror: a bundle-bound ticket is never a parent.
	q, err := env2.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "w140 bound ticket", Reporter: env2.UserID,
		BundleName: strPtrTest("w140-sym-q"),
	})
	if err != nil {
		t.Fatalf("create bound ticket: %v", err)
	}
	before, err := env2.WIs.List(ListFilters{Project: "alpha"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := env2.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "w140 child under bound parent", Reporter: env2.UserID,
		ParentID: strPtrTest(q.ID),
	}); !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "PARENT_NOT_REGISTRY" {
		t.Fatalf("create child under bound parent: want CORE-400/PARENT_NOT_REGISTRY, got %v", err)
	}
	after, err := env2.WIs.List(ListFilters{Project: "alpha"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("refused create persisted a row: %d before, %d after", len(before), len(after))
	}

	// ... and the unbound parent accepts children (parent_id lands).
	r, err := env2.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "w140 unbound parent", Reporter: env2.UserID,
	})
	if err != nil {
		t.Fatalf("create unbound parent: %v", err)
	}
	rc, err := env2.WIs.Create(CreateRequest{
		Project: "alpha", Goal: "w140 child under unbound parent", Reporter: env2.UserID,
		ParentID: strPtrTest(r.ID),
	})
	if err != nil {
		t.Fatalf("create child under unbound parent: %v", err)
	}
	if rc.ParentID == nil || *rc.ParentID != r.ID {
		t.Fatalf("child parent_id = %v, want %s", rc.ParentID, r.ID)
	}
}
