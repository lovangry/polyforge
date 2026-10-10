package server

// ship_ledger_test.go: the ship-ledger routes' pins (polyforge#123, P1b)
// — the auth face (no key / wrong key → the typed 401), the record →
// query round trip over real HTTP, the recording refusals (bad body, bad
// shape, unknown WI), and the empty-ledger answer shape.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

func ledgerSrvEnv(t *testing.T) (*srvEnv, string) {
	t.Helper()
	e := newSrvEnv(t)
	var created struct {
		ID string `json:"id"`
	}
	code, body := e.do(t, "POST", "/api/v1/work-items", e.key,
		map[string]string{"project": "alpha", "goal": "ship ledger"}, &created)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	return e, created.ID
}

func TestShipLedgerRouteRecordAndQuery(t *testing.T) {
	e, wi := ledgerSrvEnv(t)

	// No key on the recording face → the typed 401.
	code, _ := e.do(t, "POST", "/api/v1/work-items/"+wi+"/ship-ledger", "", nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("record with no key: %d", code)
	}
	// No key on the query face → the typed 401.
	code, _ = e.do(t, "GET", "/api/v1/work-items/"+wi+"/ship-ledger", "", nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("query with no key: %d", code)
	}

	// Record one ship run (branch + two repos), then query it back.
	code, body := e.do(t, "POST", "/api/v1/work-items/"+wi+"/ship-ledger", e.key,
		api.ShipLedgerRequest{
			Branch: "polyforge/" + wi,
			Repos: []api.ShipLedgerEntry{
				{Repo: "demo-src", BranchTopSHA: "2222222222222222222222222222222222222222"},
				{Repo: "polyforge", BranchTopSHA: "1111111111111111111111111111111111111111"},
			},
		}, nil)
	if code != http.StatusOK {
		t.Fatalf("record: %d %s", code, body)
	}
	var out api.ShipLedgerResponse
	code, body = e.do(t, "GET", "/api/v1/work-items/"+wi+"/ship-ledger", e.key, nil, &out)
	if code != http.StatusOK {
		t.Fatalf("query: %d %s", code, body)
	}
	if out.WorkItemID != wi || len(out.Repos) != 2 {
		t.Fatalf("ledger = %+v", out)
	}
	if out.Repos[0].Repo != "demo-src" || out.Repos[0].Branch != "polyforge/"+wi ||
		out.Repos[0].BranchTopSHA != "2222222222222222222222222222222222222222" ||
		out.Repos[0].PushedAt == "" {
		t.Fatalf("row 0: %+v", out.Repos[0])
	}
	if out.Repos[1].Repo != "polyforge" {
		t.Fatalf("rows not repo-ordered: %+v", out.Repos)
	}
}

func TestShipLedgerRouteRefusals(t *testing.T) {
	e, wi := ledgerSrvEnv(t)

	// Malformed body → CORE-400.
	code, body := e.do(t, "POST", "/api/v1/work-items/"+wi+"/ship-ledger", e.key,
		"{not json", nil)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "CORE-400") {
		t.Fatalf("malformed body: %d %s", code, body)
	}
	// Empty repo set → CORE-400.
	code, body = e.do(t, "POST", "/api/v1/work-items/"+wi+"/ship-ledger", e.key,
		api.ShipLedgerRequest{Repos: nil}, nil)
	if code != http.StatusBadRequest || !strings.Contains(string(body), "CORE-400") {
		t.Fatalf("empty repos: %d %s", code, body)
	}
	// Unknown WI → CORE-404 (hidden behind the ordinary not-found face).
	code, body = e.do(t, "POST", "/api/v1/work-items/wi_nope/ship-ledger", e.key,
		api.ShipLedgerRequest{Repos: []api.ShipLedgerEntry{{Repo: "r", BranchTopSHA: "a"}}}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown wi: %d %s", code, body)
	}
	// Unknown WI on the query face → CORE-404.
	code, body = e.do(t, "GET", "/api/v1/work-items/wi_nope/ship-ledger", e.key, nil, nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown wi query: %d %s", code, body)
	}
	// Empty ledger for a real WI: 200 with an empty repos array.
	var out api.ShipLedgerResponse
	code, body = e.do(t, "GET", "/api/v1/work-items/"+wi+"/ship-ledger", e.key, nil, &out)
	if code != http.StatusOK || out.Repos == nil || len(out.Repos) != 0 {
		t.Fatalf("empty ledger: %d %s %+v", code, body, out)
	}
}
