package workitem_test

// Tests for creation transaction, parent/inventory constraints, PATCH
// immutability, cancel, ready queue, budget CAS and dependency edges
// (02 §4.2/§4.2a/§4.5/§4.6/§5.1b/§6.5/§6.7; test categories 02 §10).

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/core/store"
	"github.com/silgrid/polyforge/internal/core/testsupport"
	"github.com/silgrid/polyforge/internal/core/workitem"
)

type h struct {
	db  *store.DB
	wi  *workitem.Manager
	dir string
}

func setup(t *testing.T) *h {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := testsupport.SeedBase(db); err != nil {
		t.Fatal(err)
	}
	wfDir := filepath.Join(dir, "workflows")
	if err := testsupport.MakeDemoBundle(dir); err != nil {
		t.Fatal(err)
	}
	return &h{db: db, wi: workitem.NewManager(db, wfDir), dir: dir}
}

func ae(t *testing.T, err error) *workitem.APIError {
	t.Helper()
	e, ok := err.(*workitem.APIError)
	if !ok {
		t.Fatalf("expected *workitem.APIError, got %T %v", err, err)
	}
	return e
}

func TestCreateRegistryVsExecution(t *testing.T) {
	h := setup(t)
	reg, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "registry", Reporter: "usr_worker"})
	if err != nil {
		t.Fatal(err)
	}
	if reg.BundleName != nil || reg.BundleDigest != nil || string(reg.BudgetBaseline) != "null" {
		t.Fatalf("registry ticket must have NULL workflow columns: %+v", reg.WorkItem)
	}
	if reg.Slug == "" || reg.Status != "queued" {
		t.Fatalf("bad row: %+v", reg.WorkItem)
	}

	ex, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "exec", Reporter: "usr_worker", BundleName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if ex.BundleName == nil || *ex.BundleName != "demo" {
		t.Fatalf("execution ticket: %+v", ex.WorkItem)
	}
	var bl map[string]any
	if err := json.Unmarshal(ex.BudgetBaseline, &bl); err != nil {
		t.Fatal(err)
	}
	if v, ok := bl["requests"].(float64); !ok || v != 100 {
		t.Fatalf("baseline抄录 wrong: %s", ex.BudgetBaseline)
	}

	// Frontmatter failure carries the rule id.
	_, err = h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "bad", Reporter: "usr_worker", BundleName: "missing-bundle"})
	e := ae(t, err)
	if e.Code != "CORE-400" || e.Reason != "FRONTMATTER_INVALID" {
		t.Fatalf("got %v", e)
	}
}

func TestCreateParentConstraints(t *testing.T) {
	h := setup(t)
	parent, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "epic", Reporter: "usr_worker"})
	if err != nil {
		t.Fatal(err)
	}
	kid, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "kid", Reporter: "usr_worker", ParentID: &parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Depth limit 1: kid cannot be a parent.
	_, err = h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "grandkid", Reporter: "usr_worker", ParentID: &kid.ID})
	e := ae(t, err)
	if e.Code != "CORE-400" || e.Reason != "PARENT_DEPTH" {
		t.Fatalf("got %v", e)
	}
	// Execution ticket cannot be a parent.
	ex, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "ex", Reporter: "usr_worker", BundleName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "kid2", Reporter: "usr_worker", ParentID: &ex.ID})
	e = ae(t, err)
	if e.Code != "CORE-400" || e.Reason != "PARENT_NOT_REGISTRY" {
		t.Fatalf("got %v", e)
	}
}

func TestCreateInventoryFreeze(t *testing.T) {
	h := setup(t)
	sub := "sub"
	base := []workitem.RepoBaseDecl{{Name: "r1", Base: "sha1"}, {Name: "r2", Base: "sha2"}}
	inv := []workitem.RepoDecl{{Name: "r1", URL: "u1"}, {Name: "r2", URL: "u2", Subdir: &sub}}

	d, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "inv", Reporter: "usr_worker", BundleName: "demo",
		RepoInventory: inv, RepoInventoryBase: base})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := h.wi.Inventory(d.ID)
	if len(got) != 2 || got[0].Name != "r1" || got[0].Base != "sha1" || *got[1].Subdir != "sub" {
		t.Fatalf("inventory: %+v", got)
	}

	// Mismatch: base set differs from inventory set.
	_, err = h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "inv2", Reporter: "usr_worker", BundleName: "demo",
		RepoInventory: inv, RepoInventoryBase: base[:1]})
	e := ae(t, err)
	if e.Code != "CORE-400" || e.Reason != "INVENTORY_MISMATCH" {
		t.Fatalf("got %v", e)
	}
	// Missing base value.
	_, err = h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "inv3", Reporter: "usr_worker", BundleName: "demo",
		RepoInventory: inv, RepoInventoryBase: []workitem.RepoBaseDecl{{Name: "r1", Base: "x"}, {Name: "r2", Base: ""}}})
	e = ae(t, err)
	if e.Code != "CORE-400" || e.Reason != "INVENTORY_BASE_MISSING" {
		t.Fatalf("got %v", e)
	}
}

func TestAttrsBlacklistRecursive(t *testing.T) {
	h := setup(t)
	bad := map[string]any{"custom": map[string]any{"current_step": "code"}}
	_, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "attrs", Reporter: "usr_worker", Attrs: bad})
	e := ae(t, err)
	if e.Code != "CORE-400" {
		t.Fatalf("INV-2c recursive scan failed: %v", e)
	}
	// Array-nested too.
	_, err = h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "attrs2", Reporter: "usr_worker",
		Attrs: map[string]any{"list": []any{map[string]any{"budget_consumed": 1}}}})
	e = ae(t, err)
	if e.Code != "CORE-400" {
		t.Fatalf("INV-2c array scan failed: %v", e)
	}
}

func TestPatchImmutability(t *testing.T) {
	h := setup(t)
	d, err := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "g", Reporter: "usr_worker", BundleName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.wi.Patch(d.ID, map[string]json.RawMessage{"bundle_name": json.RawMessage(`"other"`)})
	e := ae(t, err)
	if e.Code != "CORE-400" || e.Reason != "IMMUTABLE" {
		t.Fatalf("got %v", e)
	}
	_, err = h.wi.Patch(d.ID, map[string]json.RawMessage{"parent_id": json.RawMessage(`"wi_x"`), "status": json.RawMessage(`"running"`)})
	e = ae(t, err)
	if e.Code != "CORE-400" {
		t.Fatalf("got %v", e)
	}
	p, err := h.wi.Patch(d.ID, map[string]json.RawMessage{"goal": json.RawMessage(`"new goal"`), "labels": json.RawMessage(`["a"]`)})
	if err != nil || p.Goal != "new goal" || len(p.Labels) != 1 {
		t.Fatalf("patch failed: %v %+v", err, p)
	}
}

func TestCancel(t *testing.T) {
	h := setup(t)
	queued, _ := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "q", Reporter: "usr_worker"})
	c, err := h.wi.Cancel(queued.ID, "usr_worker")
	if err != nil || c.Status != "cancelled" || c.ClosedAt == nil {
		t.Fatalf("cancel: %v", err)
	}
	// Terminal: cancel again → 409 (not one of the three legal statuses).
	_, err = h.wi.Cancel(queued.ID, "usr_worker")
	e := ae(t, err)
	if e.Code != "CORE-409" {
		t.Fatalf("got %v", e)
	}
}

func TestReadyQueue(t *testing.T) {
	h := setup(t)
	low, _ := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "low", Reporter: "usr_worker", Priority: "low", BundleName: "demo"})
	urgent, _ := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "urgent", Reporter: "usr_worker", Priority: "urgent", BundleName: "demo"})
	reg, _ := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "registry", Reporter: "usr_worker"})
	_ = low

	q, err := h.wi.ReadyQueue("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Executable) != 2 {
		t.Fatalf("executable = %+v", q.Executable)
	}
	if q.Executable[0].ID != urgent.ID {
		t.Fatalf("priority ordering broken: %+v", q.Executable)
	}
	for _, e := range q.Executable {
		if e.ID == reg.ID {
			t.Fatal("registration tickets must never appear in executable")
		}
	}

	// blocks edge pushes the queued ticket into blocked with waiting_on.
	if err := h.wi.AddDependency(urgent.ID, low.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	q, err = h.wi.ReadyQueue("p1")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range q.Blocked {
		if e.ID == low.ID {
			found = true
			if e.WaitingOn == nil || *e.WaitingOn != "dependency:"+urgent.ID {
				t.Fatalf("waiting_on not materialized: %+v", e)
			}
			if len(e.BlockedBy) != 1 || e.BlockedBy[0] != urgent.ID {
				t.Fatalf("blocked_by: %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("dependency-waiting ticket missing from blocked: %+v", q.Blocked)
	}
	// Removing the edge restores it.
	if err := h.wi.RemoveDependency(urgent.ID, low.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	q, _ = h.wi.ReadyQueue("p1")
	if len(q.Executable) != 2 {
		t.Fatalf("executable after edge removal = %+v", q.Executable)
	}
}

func TestDependencyCycleDetection(t *testing.T) {
	h := setup(t)
	a, _ := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "a", Reporter: "usr_worker", BundleName: "demo"})
	b, _ := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "b", Reporter: "usr_worker", BundleName: "demo"})
	c, _ := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "c", Reporter: "usr_worker", BundleName: "demo"})
	if err := h.wi.AddDependency(a.ID, b.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := h.wi.AddDependency(b.ID, c.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	err := h.wi.AddDependency(c.ID, a.ID, "blocks")
	e := ae(t, err)
	if e.Code != "CORE-409" || e.Reason != "DEPENDENCY_CYCLE" {
		t.Fatalf("got %v", e)
	}
	// Self edge.
	err = h.wi.AddDependency(a.ID, a.ID, "blocks")
	if e := ae(t, err); e.Code != "CORE-400" {
		t.Fatalf("got %v", e)
	}
}

func TestBudgetCAS(t *testing.T) {
	h := setup(t)
	d, _ := h.wi.Create(workitem.CreateRequest{Project: "p1", Goal: "b", Reporter: "usr_worker", BundleName: "demo"})

	b0, err := h.wi.GetBudget(d.ID)
	if err != nil || b0.MaxSeq != 0 {
		t.Fatalf("budget: %v", err)
	}
	if b0.Baseline["requests"] == nil || *b0.Baseline["requests"] != 100 {
		t.Fatalf("baseline: %+v", b0.Baseline)
	}

	// Two concurrent increments on the same based_on_seq: exactly one wins.
	note := "n"
	b1, err := h.wi.AddBudgetIncrement(d.ID, "requests", 10, note, 0, "usr_admin")
	if err != nil {
		t.Fatal(err)
	}
	if *b1.Effective["requests"] != 110 {
		t.Fatalf("effective: %+v", b1.Effective)
	}
	_, err = h.wi.AddBudgetIncrement(d.ID, "tokens", 5, "", 0, "usr_admin")
	e := ae(t, err)
	if e.Code != "CORE-409" {
		t.Fatalf("expected CAS conflict, got %v", e)
	}
	// seq is global across dimensions: next winner must use based_on_seq=1.
	b2, err := h.wi.AddBudgetIncrement(d.ID, "tokens", 5, "", 1, "usr_admin")
	if err != nil {
		t.Fatal(err)
	}
	if b2.MaxSeq != 2 {
		t.Fatalf("global seq: %+v", b2)
	}
	// Baseline is never rewritten (INV-2e).
	row, _ := h.wi.Get(d.ID, workitem.GetOptions{})
	if string(row.BudgetBaseline) != `{"requests":100,"tokens":null,"wall_minutes":null}` {
		t.Fatalf("baseline mutated: %s", row.BudgetBaseline)
	}
	_ = time.Now
}
