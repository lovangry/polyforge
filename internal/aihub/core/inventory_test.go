package core

// §4.6 repo-inventory tests: the inventory freezes at create into
// work_items.repo_inventory (one array, name-only entries take their URL
// from the project), every refusal is typed and persists nothing, a
// frozen base is refused (not supported), the claim serves only the
// declared repos (re-claim included) and a WI without an inventory keeps
// getting every project repo.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

func decl(name, url string) RepoDecl { return RepoDecl{Name: name, URL: url} }

// invCreate is a create request on project alpha carrying inv.
func invCreate(env *testEnv, inv []RepoDecl) CreateRequest {
	return CreateRequest{
		Project: "alpha", Goal: "repo-backed wi", Reporter: env.UserID,
		BundleName: strPtrTest("demo-bundle"), RepoInventory: inv,
	}
}

// reposProject creates project bravo with three repos.
func reposProject(t *testing.T, env *testEnv) {
	t.Helper()
	if _, err := env.Admin.CreateProject(CreateProjectRequest{
		Name: "bravo",
		Repos: []api.ProjectRepo{
			{Name: "app", URL: "file:///srv/app.git"},
			{Name: "lib", URL: "file:///srv/lib.git"},
			{Name: "big", URL: "file:///srv/big.git"},
		},
	}); err != nil {
		t.Fatalf("project: %v", err)
	}
}

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
	for _, p := range []string{"alpha", "bravo"} {
		wis, lerr := env.WIs.List(ListFilters{Project: p})
		if lerr != nil && !IsCode(lerr, api.ErrCoreNotFound) {
			t.Fatalf("list: %v", lerr)
		}
		if len(wis) != 0 {
			t.Fatalf("refused create must not persist; found %d work item(s) in %s", len(wis), p)
		}
	}
}

func TestWIRepoInventoryBaseRefused(t *testing.T) {
	env := newEnv(t)
	inv := decl("polyforge", "https://example.com/polyforge.git")
	inv.Base = "0123456789abcdef"
	_, err := env.WIs.Create(invCreate(env, []RepoDecl{inv}))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "INVENTORY_BASE_UNSUPPORTED", "polyforge")
	// The removed second array of the pre-v2.5.0 shape is refused too,
	// never silently dropped.
	req := invCreate(env, []RepoDecl{decl("polyforge", "https://example.com/polyforge.git")})
	req.RepoInventoryBase = json.RawMessage(`[{"name":"polyforge","base":"abc"}]`)
	_, err = env.WIs.Create(req)
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "INVENTORY_BASE_UNSUPPORTED", "")
}

func TestWIRepoInventoryDuplicateRefused(t *testing.T) {
	env := newEnv(t)
	_, err := env.WIs.Create(invCreate(env, []RepoDecl{decl("r1", "u1"), decl("r1", "u2")}))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "INVENTORY_DUPLICATE_NAME", "r1")
}

func TestWIRepoInventoryTooManyRefused(t *testing.T) {
	env := newEnv(t)
	var inv []RepoDecl
	for i := 0; i < 9; i++ {
		inv = append(inv, decl("r"+strings.Repeat("x", i), "u")) // distinct names
	}
	_, err := env.WIs.Create(invCreate(env, inv))
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "", "")
}

func TestWIRepoInventoryUnknownProjectRepoRefused(t *testing.T) {
	env := newEnv(t)
	reposProject(t, env)
	req := invCreate(env, []RepoDecl{{Name: "app"}, {Name: "nope"}})
	req.Project = "bravo"
	_, err := env.WIs.Create(req)
	wantTypedRefusal(t, env, err, api.ErrCoreBadInput, "REPO_NOT_IN_PROJECT", "nope")
}

func TestWIRepoInventoryWellFormedCreatePersistsAndServes(t *testing.T) {
	env := newEnv(t)
	reposProject(t, env)
	sub := "tools/inner"
	req := invCreate(env, []RepoDecl{
		{Name: "lib"}, // name only: the URL comes from the project
		{Name: "other", URL: "https://example.com/other.git", Subdir: &sub},
	})
	req.Project = "bravo"
	wi, err := env.WIs.Create(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var stored string
	if err := env.DB.QueryRow(`SELECT repo_inventory FROM work_items WHERE id=?1`, wi.ID).Scan(&stored); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if want := `[{"name":"lib","url":"file:///srv/lib.git"},{"name":"other","url":"https://example.com/other.git","subdir":"tools/inner"}]`; stored != want {
		t.Fatalf("stored inventory\n got %s\nwant %s", stored, want)
	}
	view, err := env.WIs.View(wi)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	raw, _ := json.Marshal(view)
	if !strings.Contains(string(raw), `"repo_inventory":`+stored) {
		t.Fatalf("view JSON must carry the frozen inventory, got %s", raw)
	}
	// A WI without an inventory stores NULL and omits the key.
	plain := mustCreateWI(t, env, "no inventory", nil, "")
	var col *string
	if err := env.DB.QueryRow(`SELECT repo_inventory FROM work_items WHERE id=?1`, plain).Scan(&col); err != nil || col != nil {
		t.Fatalf("plain WI column = %v (%v), want NULL", col, err)
	}
	pv, err := env.WIs.View(&api.WorkItem{ID: plain})
	if err != nil {
		t.Fatalf("plain view: %v", err)
	}
	if raw, _ := json.Marshal(pv); strings.Contains(string(raw), "repo_inventory") {
		t.Fatalf("plain WI must omit repo_inventory, got %s", raw)
	}
}

func TestWIRepoInventoryPatchRefused(t *testing.T) {
	env := newEnv(t)
	id := mustCreateWI(t, env, "patch carrier", nil, "")
	_, err := env.WIs.Patch(id, map[string]json.RawMessage{
		"repo_inventory": json.RawMessage(`[{"name":"polyforge","url":"https://example.com/polyforge.git"}]`),
	})
	if !IsCode(err, api.ErrCoreBadInput) || ErrorReason(err) != "IMMUTABLE" {
		t.Fatalf("want IMMUTABLE for an inventory patch, got %v", err)
	}
	inv, err := repoInventoryOf(env.DB, id)
	if err != nil || len(inv) != 0 {
		t.Fatalf("refused patch must not freeze an inventory, got %+v (%v)", inv, err)
	}
}

// TestClaimServesDeclaredInventoryOnly: the claim answers exactly the
// declared repos — on the first claim and on the re-claim after a
// blocked→resume — while a WI without an inventory gets every project
// repo.
func TestClaimServesDeclaredInventoryOnly(t *testing.T) {
	env := newEnv(t)
	reposProject(t, env)
	req := invCreate(env, []RepoDecl{{Name: "lib"}})
	req.Project = "bravo"
	wi, err := env.WIs.Create(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m := NewSessionManager(env.DB)
	wantLib := func(res *api.SessionClaim) {
		t.Helper()
		if len(res.Repos) != 1 || res.Repos[0].Name != "lib" || res.Repos[0].URL != "file:///srv/lib.git" {
			t.Fatalf("claim repos = %+v, want only lib", res.Repos)
		}
	}
	secret := strings.Repeat("4", 32)
	res, err := m.Claim(wi.ID, api.SessionClaimRequest{SessionID: secret}, env.UserID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	wantLib(res)
	if err := m.End(wi.ID, api.SessionEndRequest{SessionID: secret, Status: api.StatusBlocked, Note: "x"}, env.UserID); err != nil {
		t.Fatalf("block: %v", err)
	}
	if _, err := env.WIs.ResumeBlocked(wi.ID, env.UserID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	res, err = m.Claim(wi.ID, api.SessionClaimRequest{SessionID: strings.Repeat("5", 32)}, env.UserID)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	wantLib(res)

	plain, err := env.WIs.Create(CreateRequest{Project: "bravo", Goal: "no inventory",
		Reporter: env.UserID, BundleName: strPtrTest("demo-bundle")})
	if err != nil {
		t.Fatalf("create plain: %v", err)
	}
	res, err = m.Claim(plain.ID, api.SessionClaimRequest{SessionID: strings.Repeat("6", 32)}, env.UserID)
	if err != nil {
		t.Fatalf("claim plain: %v", err)
	}
	if len(res.Repos) != 3 {
		t.Fatalf("a WI without an inventory must get every project repo, got %+v", res.Repos)
	}
}
