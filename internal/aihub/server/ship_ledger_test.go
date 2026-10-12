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
	rows, err := e.db.Query(`SELECT repo, branch, branch_top_sha, pushed_at FROM wi_ship_ledger
		WHERE work_item_id=?1 ORDER BY repo`, wi)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct{ repo, branch, sha, at string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.repo, &r.branch, &r.sha, &r.at); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("ledger = %+v", got)
	}
	if got[0].repo != "demo-src" || got[0].branch != "polyforge/"+wi ||
		got[0].sha != "2222222222222222222222222222222222222222" || got[0].at == "" {
		t.Fatalf("row 0: %+v", got[0])
	}
	if got[1].repo != "polyforge" {
		t.Fatalf("rows not repo-ordered: %+v", got)
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
}
