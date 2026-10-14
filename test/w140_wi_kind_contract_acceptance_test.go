package test

// w140_wi_kind_contract_acceptance_test.go: the polyforge#140
// (split a: aihub side + design docs) acceptance suite — authored
// against the SPEC (capsule artifacts/spec.md's Requirements /
// Scenario / Acceptance-criteria blocks, mirrored 1:1 in
// artifacts/e2e-scenarios.md) BEFORE the implementation lands. It
// drives the spec'd behavior through the project's real e2e surfaces
// only: the real aihub binary on a loopback port, its HTTP API via the
// real aihubclient, the real `pf` CLI — plus (for the doc scenarios)
// the real design-doc tree. It never references implementation
// internals (no core/ symbols beyond the wire contract already
// exported today).
//
// Suite posture (see artifacts/e2e-scenarios.md for the full mapping):
//   - New-behavior bars are RED on the pre-implementation tree
//     (e2e-author's baseline run): assertion-level reds, never build reds — this
//     file compiles/vets against today's tree, and every failure is
//     an HTTP-status / JSON-shape / typed-error-reason / filesystem
//     assertion. They go green when the change lands the spec.
//   - Kept-face guards are green BOTH before and after the change
//     (NOT_CLAIMABLE claim gate, bound-parent create refusal, old
//     queue/CLI shape, race invariants that already hold).
//
// Name-agnostic derived-shape assertions: spec Assumption 2 leaves the
// derived epic/parent-completion field names and the pending-bundle
// segment key to the implementation ("response shape only additive").
// The suite therefore discovers NEW response keys relative to the
// frozen pre-change key set instead of pinning literals, and asserts
// the spec'd SEMANTICS (value transitions, segment membership) on
// whatever those new keys are.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// ---------------------------------------------------------------------------
// Small helpers (all w140-prefixed; the shared v1-harness helpers —
// newV1Install, sessionClaimReq, strPtr, w113JSONDocOf — are reused as-is).
// ---------------------------------------------------------------------------

// w140Setup boots the real aihub and one fresh scratch project.
func w140Setup(t *testing.T, name string) *v1Install {
	t.Helper()
	inst := newV1Install(t)
	if _, err := inst.client.CreateProject(aihubclient.ProjectCreateRequest{
		Name: name, Description: "w140 acceptance scratch",
	}); err != nil {
		t.Fatalf("create project %s: %v", name, err)
	}
	return inst
}

// w140Create files one work item (bundle/parent optional) and returns id.
func w140Create(t *testing.T, inst *v1Install, project, goal string, bundle, parent *string) string {
	t.Helper()
	raw, err := inst.client.CreateWorkItem(aihubclient.CreateWorkItemRequest{
		Project:    project,
		Goal:       goal,
		BundleName: bundle,
		ParentID:   parent,
	})
	if err != nil {
		t.Fatalf("create work item (%s): %v", goal, err)
	}
	var wi struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &wi); err != nil {
		t.Fatalf("create work item answer: %v (%s)", err, raw)
	}
	return wi.ID
}

// w140CreateRaw files one work item from a raw body map (for fields the
// typed client request lacks today, e.g. an explicit bundle_digest).
func w140CreateRaw(t *testing.T, inst *v1Install, body map[string]any) string {
	t.Helper()
	var raw json.RawMessage
	if err := inst.client.Do("POST", "/api/v1/work-items", body, &raw); err != nil {
		t.Fatalf("create work item (raw): %v", err)
	}
	var wi struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &wi); err != nil {
		t.Fatalf("create work item (raw) answer: %v (%s)", err, raw)
	}
	return wi.ID
}

// w140View fetches the WI detail record as a generic map.
func w140View(t *testing.T, inst *v1Install, id string) map[string]any {
	t.Helper()
	raw, err := inst.client.GetWorkItemRaw(id)
	if err != nil {
		t.Fatalf("get work item %s: %v", id, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("get work item %s answer: %v (%s)", id, err, raw)
	}
	return m
}

// w140Patch attempts a PATCH and returns the wire error (nil = accepted).
func w140Patch(inst *v1Install, id string, fields map[string]any) error {
	_, err := inst.client.PatchWorkItem(id, fields)
	return err
}

// w140Queue fetches the ready queue as a generic map.
func w140Queue(t *testing.T, inst *v1Install, project string) map[string]any {
	t.Helper()
	raw, err := inst.client.ReadyQueueRaw(project)
	if err != nil {
		t.Fatalf("ready queue %s: %v", project, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("ready queue %s answer: %v (%s)", project, err, raw)
	}
	return m
}

// w140Typed returns the error as *api.Error, or nil if not typed.
func w140Typed(err error) *api.Error {
	ae, _ := err.(*api.Error)
	return ae
}

// w140Reason extracts the typed refusal reason tag ("" when absent).
func w140Reason(err error) string {
	ae := w140Typed(err)
	if ae == nil {
		return ""
	}
	var r struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(ae.Extra, &r)
	return r.Reason
}

// w140ClaimEnd claims the WI with a fresh session and ends it with the
// given terminal status (wrapped|failed).
func w140ClaimEnd(t *testing.T, inst *v1Install, id, status string) {
	t.Helper()
	req := sessionClaimReq(id)
	if _, err := inst.client.SessionClaim(req); err != nil {
		t.Fatalf("claim %s: %v", id, err)
	}
	if err := inst.client.SessionEnd(id, api.SessionEndRequest{
		SessionID: req.SessionID, Status: status,
	}); err != nil {
		t.Fatalf("session-end %s (%s): %v", id, status, err)
	}
}

// w140AddDep adds a `blocks` dependency: blocked is held by blocking.
func w140AddDep(t *testing.T, inst *v1Install, blocked, blocking string) {
	t.Helper()
	err := inst.client.Do("POST", "/api/v1/work-items/"+url.PathEscape(blocked)+"/dependencies",
		map[string]any{"blocking_wi_id": blocking, "kind": "blocks"}, nil)
	if err != nil {
		t.Fatalf("add dependency %s blocked-by %s: %v", blocked, blocking, err)
	}
}

// w140SegIDs lists the WI ids of one queue segment ("" entries skipped).
func w140SegIDs(m map[string]any, key string) []string {
	arr, ok := m[key].([]any)
	if !ok {
		return nil
	}
	var ids []string
	for _, e := range arr {
		if obj, ok := e.(map[string]any); ok {
			if id, ok := obj["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// w140QueueOldKeys are the pre-change ready-queue top-level keys.
var w140QueueOldKeys = map[string]bool{"project": true, "items": true, "running": true, "paused": true}

// w140NewSegs returns the NEW queue segments (post-change, additive):
// top-level keys absent from the old key set whose values are arrays.
func w140NewSegs(m map[string]any) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		if w140QueueOldKeys[k] {
			continue
		}
		if _, ok := v.([]any); ok {
			out[k] = w140SegIDs(m, k)
		}
	}
	return out
}

// w140NewSegIDs is the union of ids across all new segments.
func w140NewSegIDs(m map[string]any) []string {
	var ids []string
	for _, seg := range w140NewSegs(m) {
		ids = append(ids, seg...)
	}
	return ids
}

// w140HasID reports membership.
func w140HasID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// w140FrozenViewKeys is the pre-change WI detail key set (api.WorkItem
// + clean_pause + repo_inventory): the additive-only baseline the
// derived-epic fields must be added on top of.
var w140FrozenViewKeys = map[string]bool{
	"id": true, "slug": true, "seq": true, "project_id": true, "project": true,
	"goal": true, "content": true, "parent_id": true, "status": true,
	"priority": true, "milestone": true, "labels": true, "attrs": true,
	"reporter": true, "assignee": true, "waiting_on": true,
	"bundle_name": true,
	"created_at":  true, "updated_at": true, "closed_at": true,
	"session_fingerprint": true, "session_machine": true,
	"session_started_at": true, "clean_pause": true, "repo_inventory": true,
}

// w140DerivedKeys lists the view keys beyond the frozen pre-change set.
func w140DerivedKeys(v map[string]any) []string {
	var out []string
	for k := range v {
		if !w140FrozenViewKeys[k] {
			out = append(out, k)
		}
	}
	return out
}

// w140JSONEq compares two decoded JSON values by canonical re-marshalling.
func w140JSONEq(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

// w140Race runs two operations concurrently (one barrier release) and
// returns their errors.
func w140Race(doA, doB func() error) (error, error) {
	start := make(chan struct{})
	var ea, eb error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; ea = doA() }()
	go func() { defer wg.Done(); <-start; eb = doB() }()
	close(start)
	wg.Wait()
	return ea, eb
}

// w140Pf runs the real pf CLI (no daemon face) and returns output+code.
func w140Pf(t *testing.T, inst *v1Install, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(inst.pfPath, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return out.String(), ee.ExitCode()
	}
	t.Fatalf("run pf %v: %v", args, err)
	return "", 1
}

// ---------------------------------------------------------------------------
// AC 1 + 2 (create/PATCH share the bundle persist path; R2 three-way).
// ---------------------------------------------------------------------------

// TestW140BundlePersistSharedPath: the bundle reference is the NAME only
// (the digest chain is deleted, polyforge#149) on BOTH create and PATCH
// (shared path ⇒ same observable rule); a payload or PATCH body carrying
// a digest field is simply not persisted / refused unpatchable; the R2
// window allows NULL→X, X→Y and X→NULL (unbind clears the reference).
func TestW140BundlePersistSharedPath(t *testing.T) {
	inst := w140Setup(t, "w140-persist")

	// create with a name only → the name is the reference.
	a := w140Create(t, inst, "w140-persist", "w140 named reference at create", strPtr("w140-alpha"), nil)
	va := w140View(t, inst, a)
	if got := va["bundle_name"]; got != "w140-alpha" {
		t.Fatalf("create: bundle_name = %v, want w140-alpha", got)
	}
	if got, ok := va["bundle_digest"]; ok {
		t.Fatalf("create: bundle_digest = %v (ok=%v), want the key gone", got, ok)
	}
	// create with a digest field in the payload → not persisted, never
	// served back (the field does not exist).
	b := w140CreateRaw(t, inst, map[string]any{
		"project": "w140-persist", "goal": "w140 digest field in the payload",
		"bundle_name": "w140-beta", "bundle_digest": strings.Repeat("ab", 32),
	})
	vb := w140View(t, inst, b)
	if got := vb["bundle_name"]; got != "w140-beta" {
		t.Fatalf("create with digest field: bundle_name = %v, want w140-beta", got)
	}
	if got, ok := vb["bundle_digest"]; ok {
		t.Fatalf("create with digest field: bundle_digest = %v (ok=%v), want the key gone", got, ok)
	}

	// PATCH X→Y inside the R2 window → the rename.
	if err := w140Patch(inst, a, map[string]any{"bundle_name": "w140-gamma"}); err != nil {
		t.Fatalf("PATCH bundle_name X→Y in R2 window (queued, never claimed, no children): %v", err)
	}
	va = w140View(t, inst, a)
	if got := va["bundle_name"]; got != "w140-gamma" {
		t.Fatalf("PATCH X→Y: bundle_name = %v, want w140-gamma", got)
	}
	// PATCH carrying a digest field → refused CORE-400 (unpatchable key),
	// nothing bound.
	if err := w140Patch(inst, a, map[string]any{
		"bundle_name": "w140-delta", "bundle_digest": strings.Repeat("cd", 32),
	}); err == nil {
		t.Fatalf("PATCH with bundle_digest: want the unpatchable-field refusal")
	} else if ae, ok := err.(*api.Error); !ok || ae.Code != api.ErrCoreBadInput {
		t.Fatalf("PATCH with bundle_digest: want CORE-400, got %v", err)
	}
	va = w140View(t, inst, a)
	if got := va["bundle_name"]; got != "w140-gamma" {
		t.Fatalf("refused PATCH changed bundle_name = %v, want w140-gamma", got)
	}

	// PATCH X→NULL (unbind) → the reference clears.
	if err := w140Patch(inst, a, map[string]any{"bundle_name": nil}); err != nil {
		t.Fatalf("PATCH bundle_name X→NULL (unbind) in R2 window: %v", err)
	}
	va = w140View(t, inst, a)
	if got, ok := va["bundle_name"]; !ok || got != nil {
		t.Fatalf("unbind: bundle_name = %v (ok=%v), want JSON null", got, ok)
	}
	if got := va["status"]; got != "queued" {
		t.Fatalf("unbind: status = %v, want queued (no state written)", got)
	}
}

// ---------------------------------------------------------------------------
// AC 3 (claimed / terminal tickets refuse bundle PATCH).
// ---------------------------------------------------------------------------

// TestW140R2WindowClaimedAndTerminal: a once-claimed ticket (running,
// failed or any other post-claim state, terminal included) refuses
// PATCH bundle_name with the IMMUTABLE/TERMINAL_STATE vocabulary and
// writes nothing.
func TestW140R2WindowClaimedAndTerminal(t *testing.T) {
	inst := w140Setup(t, "w140-r2")

	// running (claimed, held).
	a := w140Create(t, inst, "w140-r2", "w140 claimed running", strPtr("w140-r2-a"), nil)
	req := sessionClaimReq(a)
	if _, err := inst.client.SessionClaim(req); err != nil {
		t.Fatalf("claim %s: %v", a, err)
	}
	if err := w140Patch(inst, a, map[string]any{"bundle_name": "w140-r2-x"}); err == nil {
		t.Fatalf("PATCH bundle on claimed (running) ticket: accepted, want refusal")
	} else if ae := w140Typed(err); ae == nil {
		t.Fatalf("PATCH bundle on claimed ticket: refusal is not a typed error: %v", err)
	} else if r := w140Reason(err); r != "IMMUTABLE" && r != "TERMINAL_STATE" {
		t.Fatalf("PATCH bundle on claimed ticket: reason = %q, want IMMUTABLE/TERMINAL_STATE family (spec Assumption 1)", r)
	}
	va := w140View(t, inst, a)
	if got := va["bundle_name"]; got != "w140-r2-a" {
		t.Fatalf("refused PATCH wrote: bundle_name = %v, want w140-r2-a", got)
	}
	if got := va["status"]; got != "running" {
		t.Fatalf("refused PATCH wrote: status = %v, want running", got)
	}

	// failed (terminal, was claimed).
	b := w140Create(t, inst, "w140-r2", "w140 failed terminal", strPtr("w140-r2-b"), nil)
	w140ClaimEnd(t, inst, b, "failed")
	if err := w140Patch(inst, b, map[string]any{"bundle_name": "w140-r2-x"}); err == nil {
		t.Fatalf("PATCH bundle on failed (terminal) ticket: accepted, want refusal")
	} else if r := w140Reason(err); r != "IMMUTABLE" && r != "TERMINAL_STATE" {
		t.Fatalf("PATCH bundle on failed ticket: reason = %q, want IMMUTABLE/TERMINAL_STATE family", r)
	}
	if got := w140View(t, inst, b)["bundle_name"]; got != "w140-r2-b" {
		t.Fatalf("refused PATCH wrote: bundle_name = %v, want w140-r2-b", got)
	}

	// canceled (terminal, never claimed).
	c := w140Create(t, inst, "w140-r2", "w140 canceled terminal", strPtr("w140-r2-c"), nil)
	if _, err := inst.client.CancelWorkItem(c); err != nil {
		t.Fatalf("cancel %s: %v", c, err)
	}
	if err := w140Patch(inst, c, map[string]any{"bundle_name": "w140-r2-x"}); err == nil {
		t.Fatalf("PATCH bundle on canceled (terminal) ticket: accepted, want refusal")
	} else if r := w140Reason(err); r != "IMMUTABLE" && r != "TERMINAL_STATE" {
		t.Fatalf("PATCH bundle on canceled ticket: reason = %q, want IMMUTABLE/TERMINAL_STATE family", r)
	}
}

// ---------------------------------------------------------------------------
// AC 4 (waiting_on does not refuse the bundle PATCH on its own).
// ---------------------------------------------------------------------------

// TestW140WaitingOnNotRefused: a queued, never-claimed, childless
// ticket with waiting_on set still accepts the bundle PATCH — the
// window looks only at queued ∧ never-claimed.
func TestW140WaitingOnNotRefused(t *testing.T) {
	inst := w140Setup(t, "w140-wait")

	a := w140Create(t, inst, "w140-wait", "w140 waiting_on non-empty", nil, nil)
	if err := w140Patch(inst, a, map[string]any{"waiting_on": "owner"}); err != nil {
		t.Fatalf("PATCH waiting_on (patchable today): %v", err)
	}
	if err := w140Patch(inst, a, map[string]any{"bundle_name": "w140-wait-b"}); err != nil {
		t.Fatalf("PATCH bundle_name with waiting_on non-empty (queued, never claimed, no children): %v\n(the R2 window must not refuse on waiting_on alone)", err)
	}
	va := w140View(t, inst, a)
	if got := va["bundle_name"]; got != "w140-wait-b" {
		t.Fatalf("bundle_name = %v, want w140-wait-b", got)
	}
	if got := va["waiting_on"]; got != "owner" {
		t.Fatalf("waiting_on = %v, want owner (untouched)", got)
	}
}

// ---------------------------------------------------------------------------
// AC 5 + 6 (symmetric parent/bundle rules).
// ---------------------------------------------------------------------------

// TestW140SymmetricRules: a ticket WITH children never gains a bundle
// (PATCH side, children in any status); a BOUND ticket is never a
// parent (create side, PARENT_NOT_REGISTRY semantics — literal left to
// the implementation per spec Assumption 1, so only the semantics and
// the no-write outcome are asserted); a bound child under an unbound
// parent is legitimate; an unbound parent accepts new children.
func TestW140SymmetricRules(t *testing.T) {
	inst := w140Setup(t, "w140-sym")

	// PATCH side: parent with children cannot bind.
	p := w140Create(t, inst, "w140-sym", "w140 parent", nil, nil)
	c1 := w140Create(t, inst, "w140-sym", "w140 child one", nil, strPtr(p))
	if err := w140Patch(inst, p, map[string]any{"bundle_name": "w140-sym-p"}); err == nil {
		t.Fatalf("PATCH bundle on parent with children: accepted, want refusal")
	}
	// ... and a terminal child still counts as a child.
	if _, err := inst.client.CancelWorkItem(c1); err != nil {
		t.Fatalf("cancel child %s: %v", c1, err)
	}
	if err := w140Patch(inst, p, map[string]any{"bundle_name": "w140-sym-p"}); err == nil {
		t.Fatalf("PATCH bundle on parent whose (only) child is terminal: accepted, want refusal (children count in any status)")
	}
	if got := w140View(t, inst, p)["bundle_name"]; got != nil {
		t.Fatalf("refused PATCH wrote: bundle_name = %v, want null", got)
	}

	// create side: a bound child under the unbound parent is fine.
	c2 := w140Create(t, inst, "w140-sym", "w140 bound child under unbound parent", strPtr("w140-sym-c2"), strPtr(p))
	if got := w140View(t, inst, c2)["parent_id"]; got != p {
		t.Fatalf("bound child parent_id = %v, want %s", got, p)
	}
	// ... but the parent NOW has children again, so it still cannot bind.
	if err := w140Patch(inst, p, map[string]any{"bundle_name": "w140-sym-p"}); err == nil {
		t.Fatalf("PATCH bundle on parent with the new bound child: accepted, want refusal")
	}

	// create side: a bound ticket is refused as a parent.
	q := w140Create(t, inst, "w140-sym", "w140 bound parent", strPtr("w140-sym-q"), nil)
	before := w140ProjectIDs(t, inst, "w140-sym")
	if _, err := inst.client.CreateWorkItem(aihubclient.CreateWorkItemRequest{
		Project: "w140-sym", Goal: "w140 child under bound parent", ParentID: strPtr(q),
	}); err == nil {
		t.Fatalf("create child under bundle-bound parent: accepted, want refusal (PARENT_NOT_REGISTRY semantics)")
	} else if ae := w140Typed(err); ae == nil {
		t.Fatalf("create child under bound parent: refusal is not a typed error: %v", err)
	}
	after := w140ProjectIDs(t, inst, "w140-sym")
	if len(after) != len(before) {
		t.Fatalf("refused create persisted a row: %d work items before, %d after", len(before), len(after))
	}

	// create side: an unbound parent accepts children (parent_id lands).
	r := w140Create(t, inst, "w140-sym", "w140 unbound parent", nil, nil)
	rc := w140Create(t, inst, "w140-sym", "w140 child under unbound parent", nil, strPtr(r))
	if got := w140View(t, inst, rc)["parent_id"]; got != r {
		t.Fatalf("child parent_id = %v, want %s", got, r)
	}
}

// w140ProjectIDs lists the project's work item ids (list endpoint).
func w140ProjectIDs(t *testing.T, inst *v1Install, project string) []string {
	t.Helper()
	raw, err := inst.client.ListWorkItemsRaw(project, "", 0)
	if err != nil {
		t.Fatalf("list work items %s: %v", project, err)
	}
	var doc struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("list work items %s answer: %v (%s)", project, err, raw)
	}
	var ids []string
	for _, it := range doc.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

// ---------------------------------------------------------------------------
// AC 7 (+ the unbind tail of AC 2): ready queue pending-bundle segment.
// ---------------------------------------------------------------------------

// TestW140ReadyQueuePendingBundleSegment: a NEW segment (name left to
// the implementation — additive-only) holds exactly the queued,
// never-blocked, childless, bundle-less tickets; the executable segment
// and every entry's old fields survive unchanged; epics appear in NO
// segment; an unbound ticket returns to the pending segment.
func TestW140ReadyQueuePendingBundleSegment(t *testing.T) {
	inst := w140Setup(t, "w140-queue")

	x := w140Create(t, inst, "w140-queue", "w140 executable bound", strPtr("w140-q-x"), nil)
	tt := w140Create(t, inst, "w140-queue", "w140 pending candidate", nil, nil)
	w := w140Create(t, inst, "w140-queue", "w140 waiting_on excluded", nil, nil)
	if err := w140Patch(inst, w, map[string]any{"waiting_on": "owner"}); err != nil {
		t.Fatalf("PATCH waiting_on: %v", err)
	}
	bq := w140Create(t, inst, "w140-queue", "w140 blocker", strPtr("w140-q-bq"), nil)
	d := w140Create(t, inst, "w140-queue", "w140 dependency-blocked excluded", nil, nil)
	w140AddDep(t, inst, d, bq)
	e := w140Create(t, inst, "w140-queue", "w140 epic parent", nil, nil)
	ec := w140Create(t, inst, "w140-queue", "w140 epic bound child", strPtr("w140-q-ec"), strPtr(e))
	v := w140Create(t, inst, "w140-queue", "w140 later unbound", strPtr("w140-q-v"), nil)

	q := w140Queue(t, inst, "w140-queue")

	// kept face: old segments and their membership.
	if got := q["project"]; got != "w140-queue" {
		t.Fatalf("queue project = %v, want w140-queue", got)
	}
	items := w140SegIDs(q, "items")
	for _, id := range []string{x, bq, ec, v} {
		if !w140HasID(items, id) {
			t.Fatalf("executable segment lost %s (old membership must survive: got %v)", id, items)
		}
	}
	for _, id := range []string{tt, w, d, e} {
		if w140HasID(items, id) {
			t.Fatalf("executable segment gained %s (must stay excluded)", id)
		}
	}

	// new face: the pending-bundle segment exists (additive) and holds
	// exactly the qualifying tickets.
	segs := w140NewSegs(q)
	if len(segs) == 0 {
		t.Fatalf("ready queue has no new segment beyond {project,items,running,paused}: the 待选 bundle segment is missing (spec: response shape only additive)")
	}
	pending := w140NewSegIDs(q)
	if !w140HasID(pending, tt) {
		t.Fatalf("pending-bundle segment does not contain the qualifying ticket %s (segments %v)", tt, segs)
	}
	for _, id := range []string{w, d, e, x, bq, ec, v} {
		if w140HasID(pending, id) {
			t.Fatalf("pending-bundle segment contains %s, which must be excluded (waiting_on/dependency/children/bundle rules)", id)
		}
	}
	// the epic appears in NO segment at all.
	for _, id := range []string{e} {
		if w140HasID(items, id) || w140HasID(w140SegIDs(q, "running"), id) ||
			w140HasID(w140SegIDs(q, "paused"), id) || w140HasID(pending, id) {
			t.Fatalf("epic %s appears in a queue segment, want no segment", id)
		}
	}

	// additivity: every entry (old and new segments) keeps the old fields.
	for key := range segs {
		for _, entry := range w140SegEntries(t, q, key) {
			w140RequireOldFields(t, key, entry)
		}
	}
	for _, entry := range w140SegEntries(t, q, "items") {
		w140RequireOldFields(t, "items", entry)
	}

	// unbind tail: X→NULL returns the ticket to the pending segment.
	if err := w140Patch(inst, v, map[string]any{"bundle_name": nil}); err != nil {
		t.Fatalf("unbind %s in R2 window: %v", v, err)
	}
	q2 := w140Queue(t, inst, "w140-queue")
	if w140HasID(w140SegIDs(q2, "items"), v) {
		t.Fatalf("unbound ticket %s still in the executable segment", v)
	}
	if !w140HasID(w140NewSegIDs(q2), v) {
		t.Fatalf("unbound ticket %s did not return to the pending-bundle segment", v)
	}
}

// w140SegEntries returns the object entries of one queue segment.
func w140SegEntries(t *testing.T, q map[string]any, key string) []map[string]any {
	t.Helper()
	arr, ok := q[key].([]any)
	if !ok {
		t.Fatalf("queue segment %q is not an array", key)
	}
	var out []map[string]any
	for _, e := range arr {
		obj, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("queue segment %q entry is not an object: %v", key, e)
		}
		out = append(out, obj)
	}
	return out
}

// w140RequireOldFields asserts a queue entry keeps the pre-change fields.
func w140RequireOldFields(t *testing.T, seg string, entry map[string]any) {
	t.Helper()
	for _, k := range []string{"id", "slug", "status", "goal", "priority", "bundle_name"} {
		if _, ok := entry[k]; !ok {
			t.Fatalf("queue segment %q entry lost the old field %q (additive-only): %v", seg, k, entry)
		}
	}
}

// ---------------------------------------------------------------------------
// AC 8: derived parent completion status (additive, never a written state).
// ---------------------------------------------------------------------------

// TestW140DerivedParentStatus: the WI view of a parent gains derived
// keys (beyond the frozen pre-change set) whose values track the
// children: they change when all children go terminal (≥1 wrapped =
// complete), and change again when a new child lands (auto back to
// in-progress) — while the stored `status` stays queued throughout,
// and the parent appears in no queue segment.
func TestW140DerivedParentStatus(t *testing.T) {
	inst := w140Setup(t, "w140-derive")

	p := w140Create(t, inst, "w140-derive", "w140 derive parent", nil, nil)
	c1 := w140Create(t, inst, "w140-derive", "w140 derive child one", strPtr("w140-d-c1"), strPtr(p))
	c2 := w140Create(t, inst, "w140-derive", "w140 derive child two", strPtr("w140-d-c2"), strPtr(p))

	// stage 0: children queued → in progress.
	v0 := w140View(t, inst, p)
	for _, k := range []string{"id", "status", "bundle_name", "parent_id"} {
		if _, ok := v0[k]; !ok {
			t.Fatalf("stage 0: view lost the old field %q", k)
		}
	}

	// stage 1: all children terminal with ≥1 wrapped → complete.
	w140ClaimEnd(t, inst, c1, "wrapped")
	w140ClaimEnd(t, inst, c2, "wrapped")
	v1 := w140View(t, inst, p)
	if got := v1["status"]; got != "queued" {
		t.Fatalf("stage 1: parent status = %v, want queued (no terminal state is ever written)", got)
	}
	derived := w140DerivedKeys(v1)
	if len(derived) == 0 {
		t.Fatalf("stage 1: WI view gained no derived keys beyond the frozen field set (spec: derived epic/parent status exposed additively)")
	}
	changed := false
	for _, k := range derived {
		if !w140JSONEq(v0[k], v1[k]) {
			changed = true
		}
	}
	if !changed {
		t.Fatalf("stage 1: no derived key changed when all children became terminal (derived status must react)")
	}

	// stage 2: a new child lands → auto back to in-progress, no reopen op.
	c3 := w140Create(t, inst, "w140-derive", "w140 derive child three", strPtr("w140-d-c3"), strPtr(p))
	v2 := w140View(t, inst, p)
	if got := v2["status"]; got != "queued" {
		t.Fatalf("stage 2: parent status = %v, want queued", got)
	}
	for _, k := range derived {
		if _, ok := v2[k]; !ok {
			t.Fatalf("stage 2: derived key %q disappeared (fields must not be removed)", k)
		}
	}
	reverted := false
	for _, k := range derived {
		if !w140JSONEq(v1[k], v2[k]) {
			reverted = true
		}
	}
	if !reverted {
		t.Fatalf("stage 2: no derived key changed back when child %s landed (auto return to in-progress, no reopen op)", c3)
	}

	// the parent (epic, queued, unbound) appears in no queue segment.
	q := w140Queue(t, inst, "w140-derive")
	if w140HasID(w140SegIDs(q, "items"), p) || w140HasID(w140NewSegIDs(q), p) {
		t.Fatalf("derived parent %s appears in a queue segment, want none", p)
	}
}

// ---------------------------------------------------------------------------
// AC 9: the two concurrency races (consistent final state; a refused side
// is an explicit typed refusal that wrote nothing).
// ---------------------------------------------------------------------------

// TestW140RacePatchBundleVsClaim: PATCH bundle vs session Claim on one
// queued, never-claimed, childless ticket. The claim always succeeds; the
// PATCH succeeds iff it was serialized before the claim (which then froze
// the renamed bundle) and is otherwise a typed refusal that wrote nothing.
// The end state is consistent either way and the bundle stays frozen.
func TestW140RacePatchBundleVsClaim(t *testing.T) {
	inst := w140Setup(t, "w140-race1")
	const rounds = 6
	patchWins, claimFirst := 0, 0
	for i := 0; i < rounds; i++ {
		id := w140Create(t, inst, "w140-race1", fmt.Sprintf("w140 race1 ticket %d", i), strPtr(fmt.Sprintf("w140-r1-%d", i)), nil)
		patchErr, claimErr := w140Race(
			func() error {
				return w140Patch(inst, id, map[string]any{"bundle_name": fmt.Sprintf("w140-r1-%d-y", i)})
			},
			func() error {
				_, err := inst.client.SessionClaim(sessionClaimReq(id))
				return err
			},
		)
		if claimErr != nil {
			t.Fatalf("round %d: a concurrent bundle PATCH must never refuse the claim: %v", i, claimErr)
		}
		v := w140View(t, inst, id)
		if got := v["status"]; got != "running" {
			t.Fatalf("round %d: status = %v, want running", i, got)
		}
		want := fmt.Sprintf("w140-r1-%d", i)
		if patchErr == nil {
			patchWins++
			want = fmt.Sprintf("w140-r1-%d-y", i)
		} else {
			claimFirst++
			if ae := w140Typed(patchErr); ae == nil {
				t.Fatalf("round %d: a PATCH serialized after the claim was not a typed refusal: %v", i, patchErr)
			}
		}
		if got := v["bundle_name"]; got != want {
			t.Fatalf("round %d: bundle_name = %v, want %s (patchErr=%v)", i, got, want, patchErr)
		}
		// frozen at the claim: any later bundle PATCH is refused.
		if err := w140Patch(inst, id, map[string]any{"bundle_name": fmt.Sprintf("w140-r1-%d-late", i)}); err == nil {
			t.Fatalf("round %d: bundle PATCH after the claim succeeded; the bundle must be frozen", i)
		}
	}
	t.Logf("w140 race1: PATCH serialized first %d, claim first %d of %d rounds", patchWins, claimFirst, rounds)
}

// TestW140RaceBindBundleVsNewChild: PATCH bundle (NULL→X) vs create of a
// child under the same ticket — exactly one wins, and the final state
// never shows children ∧ bound bundle at once.
func TestW140RaceBindBundleVsNewChild(t *testing.T) {
	inst := w140Setup(t, "w140-race2")
	const rounds = 6
	patchWins, createWins := 0, 0
	for i := 0; i < rounds; i++ {
		id := w140Create(t, inst, "w140-race2", fmt.Sprintf("w140 race2 parent %d", i), nil, nil)
		newBundle := fmt.Sprintf("w140-r2-%d-x", i)
		patchErr, createErr := w140Race(
			func() error {
				return w140Patch(inst, id, map[string]any{"bundle_name": newBundle})
			},
			func() error {
				_, err := inst.client.CreateWorkItem(aihubclient.CreateWorkItemRequest{
					Project: "w140-race2", Goal: fmt.Sprintf("w140 race2 child %d", i), ParentID: strPtr(id),
				})
				return err
			},
		)
		if (patchErr == nil) == (createErr == nil) {
			t.Fatalf("round %d: exactly one of bind-bundle / new-child must win (patchErr=%v createErr=%v)", i, patchErr, createErr)
		}
		v := w140View(t, inst, id)
		children := 0
		for _, wid := range w140ProjectIDs(t, inst, "w140-race2") {
			if w140View(t, inst, wid)["parent_id"] == id {
				children++
			}
		}
		bound := v["bundle_name"] != nil
		if createErr == nil {
			createWins++
			if ae := w140Typed(patchErr); ae == nil {
				t.Fatalf("round %d: losing PATCH was not a typed refusal: %v", i, patchErr)
			}
			if bound {
				t.Fatalf("round %d: create won but the parent is ALSO bundle-bound (children ∧ bundle must not coexist)", i)
			}
			if children == 0 {
				t.Fatalf("round %d: create won but no child row landed", i)
			}
		} else {
			patchWins++
			if ae := w140Typed(createErr); ae == nil {
				t.Fatalf("round %d: losing create was not a typed refusal: %v", i, createErr)
			}
			if !bound {
				t.Fatalf("round %d: PATCH won but the parent has no bundle", i)
			}
			if children != 0 {
				t.Fatalf("round %d: PATCH won but a child landed anyway (children ∧ bundle must not coexist)", i)
			}
		}
	}
	t.Logf("w140 race2: PATCH won %d, create won %d of %d rounds", patchWins, createWins, rounds)
}

// ---------------------------------------------------------------------------
// AC 10 (kept face): claim of a bundle-less ticket stays NOT_CLAIMABLE.
// ---------------------------------------------------------------------------

// TestW140ClaimBundlelessRefused: registration tickets (no bundle)
// stay unclaimable through the existing NOT_CLAIMABLE gate.
func TestW140ClaimBundlelessRefused(t *testing.T) {
	inst := w140Setup(t, "w140-claim")
	a := w140Create(t, inst, "w140-claim", "w140 bundleless claim refusal", nil, nil)
	_, err := inst.client.SessionClaim(sessionClaimReq(a))
	if err == nil {
		t.Fatalf("claim of bundle-less ticket %s: accepted, want NOT_CLAIMABLE refusal", a)
	}
	if r := w140Reason(err); r != "NOT_CLAIMABLE" {
		t.Fatalf("claim of bundle-less ticket: reason = %q, want NOT_CLAIMABLE (kept behavior)", r)
	}
}

// ---------------------------------------------------------------------------
// AC 11 (kept face): the old pf CLI verbs keep working against aihub.
// ---------------------------------------------------------------------------

// TestW140OldPfVerbsAgainstAihub: the pre-change aihub-facing pf verbs
// (queue list / queue ready / help) run without error against the local
// aihub and keep the old response shapes (additive-only afterwards).
func TestW140OldPfVerbsAgainstAihub(t *testing.T) {
	inst := w140Setup(t, "w140-cli")
	w140Create(t, inst, "w140-cli", "w140 cli bound ticket", strPtr("w140-cli-b"), nil)
	w140Create(t, inst, "w140-cli", "w140 cli unbound ticket", nil, nil)

	// queue --ready (the ready-queue passthrough).
	out, code := w140Pf(t, inst, "--json", "--server", inst.url, "--api-key", inst.adminKey,
		"queue", "--ready", "--project", "w140-cli")
	if code != 0 {
		t.Fatalf("pf --json queue --ready: exit %d (old verbs must keep working):\n%s", code, out)
	}
	var ready map[string]any
	if err := json.Unmarshal([]byte(w113JSONDocOf(out)), &ready); err != nil {
		t.Fatalf("pf --json queue --ready output is not one JSON doc: %v\n%s", err, out)
	}
	for _, k := range []string{"project", "items", "running", "paused"} {
		if _, ok := ready[k]; !ok {
			t.Fatalf("pf queue --ready lost the old key %q (additive-only):\n%s", k, out)
		}
	}

	// queue list mode.
	out, code = w140Pf(t, inst, "--json", "--server", inst.url, "--api-key", inst.adminKey,
		"queue", "--project", "w140-cli")
	if code != 0 {
		t.Fatalf("pf --json queue: exit %d (old verbs must keep working):\n%s", code, out)
	}
	var list map[string]any
	if err := json.Unmarshal([]byte(w113JSONDocOf(out)), &list); err != nil {
		t.Fatalf("pf --json queue output is not one JSON doc: %v\n%s", err, out)
	}
	for _, k := range []string{"items", "total"} {
		if _, ok := list[k]; !ok {
			t.Fatalf("pf queue list lost the old key %q:\n%s", k, out)
		}
	}

	// help (no wire call).
	if _, code := w140Pf(t, inst, "help"); code != 0 {
		t.Fatalf("pf help: exit %d, want 0", code)
	}
}
