package core

// §4.6 repo-inventory tests: the WI repo-reference triple (name/url/
// subdir + frozen base sha) must never persist half-empty. Every
// refusal is typed (INVENTORY_BASE_MISSING names the offending repo;
// set/count mismatches are INVENTORY_MISMATCH) and fails whole with
// nothing persisted; a well-formed create freezes and serves the
// triple; a well-formed patch is refused immutable (no UPDATE path).

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

// invCreate is a create request carrying the given inventory pair.
func invCreate(env *testEnv, inv []RepoDecl, bases []RepoBaseDecl) CreateRequest {
	return CreateRequest{
		Project: "alpha", Goal: "repo-backed wi", Reporter: env.UserID,
		RepoInventory: inv, RepoInventoryBase: bases,
	}
}

func decl(name, url string) RepoDecl { return RepoDecl{Name: name, URL: url} }

func baseDecl(name, base string) RepoBaseDecl { return RepoBaseDecl{Name: name, Base: base} }

// wantTypedRefusal asserts the typed envelope and that nothing persisted.
func wantTypedRefusal(t *testing.T, env *testEnv, err error, code, reason, nameSub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s %s refusal, got success", code, reason)
	}
	if !IsCode(err, code) {
		t.Fatalf("want code %s, got %v", code, err)
	}
	if got := ErrorReason(err); got != reason {
		t.Fatalf("want reason %s, got %q (%v)", reason, got, err)
	}
	if nameSub != "" && !strings.Contains(err.Error(), nameSub) {
		t.Fatalf("refusal must name the offending repo %q, got %v", nameSub, err)
	}
	wis, lerr := env.WIs.List(ListFilters{Project: "alpha"})
	if lerr != nil {
		t.Fatalf("list: %v", lerr)
	}
	if len(wis) != 0 {
		t.Fatalf("refused create must not persist; found %d work item(s)", len(wis))
	}
}

func TestWIRepoInventoryBlankBaseRefused(t *testing.T) {
	env := newEnv(t)
	// Blank base string.
	_, err := env.WIs.Create(invCreate(env,
		[]RepoDecl{decl("polyforge", "https://example.com/polyforge.git")},
		[]RepoBaseDecl{baseDecl("polyforge", "")}))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "INVENTORY_BASE_MISSING", "polyforge")
	// Whole base array absent.
	_, err = env.WIs.Create(invCreate(env,
		[]RepoDecl{decl("polyforge", "https://example.com/polyforge.git")}, nil))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "INVENTORY_BASE_MISSING", "polyforge")
	// Base row for a different name: the declared entry's base is missing.
	_, err = env.WIs.Create(invCreate(env,
		[]RepoDecl{decl("polyforge", "https://example.com/polyforge.git")},
		[]RepoBaseDecl{baseDecl("other", "abc123")}))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "INVENTORY_BASE_MISSING", "polyforge")
}

func TestWIRepoInventoryMismatchRefused(t *testing.T) {
	env := newEnv(t)
	// Duplicate declaration name.
	_, err := env.WIs.Create(invCreate(env,
		[]RepoDecl{decl("r1", "u1"), decl("r1", "u2")},
		[]RepoBaseDecl{baseDecl("r1", "abc"), baseDecl("r1", "abc")}))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "INVENTORY_MISMATCH", "r1")
	// Duplicate base entry.
	_, err = env.WIs.Create(invCreate(env,
		[]RepoDecl{decl("r1", "u1")},
		[]RepoBaseDecl{baseDecl("r1", "abc"), baseDecl("r1", "def")}))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "INVENTORY_MISMATCH", "r1")
	// Extra base entry with no declaration.
	_, err = env.WIs.Create(invCreate(env,
		[]RepoDecl{decl("r1", "u1")},
		[]RepoBaseDecl{baseDecl("r1", "abc"), baseDecl("r2", "def")}))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "INVENTORY_MISMATCH", "")
}

func TestWIRepoInventoryTooManyRefused(t *testing.T) {
	env := newEnv(t)
	var inv []RepoDecl
	var bases []RepoBaseDecl
	for i := 0; i < 9; i++ {
		name := "r" + strings.Repeat("x", i) // distinct names
		inv = append(inv, decl(name, "u"))
		bases = append(bases, baseDecl(name, "abc"))
	}
	_, err := env.WIs.Create(invCreate(env, inv, bases))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "", "")
}

func TestWIRepoInventoryWellFormedCreatePersistsAndServes(t *testing.T) {
	env := newEnv(t)
	sub := "tools/inner"
	wi, err := env.WIs.Create(invCreate(env,
		[]RepoDecl{decl("polyforge", "https://example.com/polyforge.git"),
			{Name: "other", URL: "https://example.com/other.git", Subdir: &sub}},
		[]RepoBaseDecl{baseDecl("polyforge", "abc123def"), baseDecl("other", " 456xyz789 ")}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Rows frozen in the store, in declaration order.
	inv, err := env.WIs.repoInventoryOf(wi.ID)
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if len(inv) != 2 {
		t.Fatalf("want 2 frozen entries, got %d", len(inv))
	}
	if inv[0].Name != "polyforge" || inv[0].URL != "https://example.com/polyforge.git" ||
		inv[0].Base != "abc123def" || inv[0].Position != 1 || inv[0].Subdir != nil {
		t.Fatalf("entry 0 mismatch: %+v", inv[0])
	}
	if inv[1].Name != "other" || inv[1].Subdir == nil || *inv[1].Subdir != sub || inv[1].Position != 2 {
		t.Fatalf("entry 1 mismatch: %+v", inv[1])
	}
	if inv[1].Base != "456xyz789" {
		t.Fatalf("base must be trimmed and frozen verbatim otherwise, got %q", inv[1].Base)
	}
	// Served on the WI view (base included, E2: not silently discarded).
	view, err := env.WIs.View(wi)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if len(view.RepoInventory) != 2 || view.RepoInventory[0].Base != "abc123def" {
		t.Fatalf("view inventory mismatch: %+v", view.RepoInventory)
	}
	// And visible in the marshalled view JSON.
	raw, _ := json.Marshal(view)
	if !strings.Contains(string(raw), `"repo_inventory":[`) || !strings.Contains(string(raw), "abc123def") {
		t.Fatalf("view JSON must carry repo_inventory with base, got %s", raw)
	}
	// A WI without an inventory omits the key.
	plain := mustCreateWI(t, env, "no inventory", nil, "")
	pv, err := env.WIs.View(&api.WorkItem{ID: plain})
	if err != nil {
		t.Fatalf("plain view: %v", err)
	}
	if len(pv.RepoInventory) != 0 {
		t.Fatalf("plain WI must serve no inventory, got %+v", pv.RepoInventory)
	}
}

func TestWIRepoInventoryPatchRefusals(t *testing.T) {
	env := newEnv(t)
	id := mustCreateWI(t, env, "patch carrier", nil, "")
	raw := func(v any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	// Half-empty triple: base-naming refusal, carrier unmutated.
	_, err := env.WIs.Patch(id, map[string]json.RawMessage{
		"repo_inventory":      raw([]RepoDecl{decl("polyforge", "https://example.com/polyforge.git")}),
		"repo_inventory_base": raw([]RepoBaseDecl{baseDecl("polyforge", "")}),
	})
	if !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "INVENTORY_BASE_MISSING" {
		t.Fatalf("want INVENTORY_BASE_MISSING, got %v", err)
	}
	// Well-formed triple: immutable (§4.6 has no UPDATE path).
	_, err = env.WIs.Patch(id, map[string]json.RawMessage{
		"repo_inventory":      raw([]RepoDecl{decl("polyforge", "https://example.com/polyforge.git")}),
		"repo_inventory_base": raw([]RepoBaseDecl{baseDecl("polyforge", "abc123")}),
	})
	if !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMMUTABLE" {
		t.Fatalf("want IMMUTABLE for well-formed inventory patch, got %v", err)
	}
	// Base-only key is part of the frozen surface.
	_, err = env.WIs.Patch(id, map[string]json.RawMessage{
		"repo_inventory_base": raw([]RepoBaseDecl{baseDecl("polyforge", "abc123")}),
	})
	if !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMMUTABLE" {
		t.Fatalf("want IMMUTABLE for repo_inventory_base patch, got %v", err)
	}
	// Nothing from the refused patches applied: the carrier WI is
	// unmutated and carries no inventory rows.
	wi, err := env.WIs.Get(id, GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if wi.Goal != "patch carrier" {
		t.Fatalf("refused patch must not mutate the WI; goal=%q", wi.Goal)
	}
	inv, err := env.WIs.repoInventoryOf(id)
	if err != nil || len(inv) != 0 {
		t.Fatalf("refused patches must not freeze inventory rows, got %+v (%v)", inv, err)
	}
}
