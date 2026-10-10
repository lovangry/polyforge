package core

// ship_ledger_test.go: the wi_ship_ledger manager pins (polyforge#123,
// P1b) — the recording shape, the per-(WI, repo) upsert (latest delivery
// wins), and the queryability face (one row per shipped repo, ordered by
// repo name; no recorded ship answers an empty ledger, never a
// fabricated row).

import (
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
)

func ledgerEnv(t *testing.T) (*testEnv, *ShipLedgerManager, string) {
	t.Helper()
	env := newEnv(t)
	m := NewShipLedgerManager(env.DB, env.WIs)
	wi, err := env.WIs.Create(CreateRequest{Project: "alpha", Goal: "shippable wi", Reporter: env.UserID})
	if err != nil {
		t.Fatalf("create wi: %v", err)
	}
	return env, m, wi.ID
}

func TestShipLedgerRecordAndQuery(t *testing.T) {
	_, m, wi := ledgerEnv(t)
	err := m.Record(wi, "polyforge/"+wi, []api.ShipLedgerEntry{
		{Repo: "polyforge", BranchTopSHA: "1111111111111111111111111111111111111111"},
		{Repo: "demo-src", BranchTopSHA: "2222222222222222222222222222222222222222"},
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	out, err := m.Ledger(wi)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if out.WorkItemID != wi || len(out.Repos) != 2 {
		t.Fatalf("ledger = %+v, want 2 rows for %s", out, wi)
	}
	// Ordered by repo name: demo-src before polyforge.
	if out.Repos[0].Repo != "demo-src" || out.Repos[1].Repo != "polyforge" {
		t.Fatalf("rows not repo-ordered: %+v", out.Repos)
	}
	if out.Repos[0].BranchTopSHA != "2222222222222222222222222222222222222222" ||
		out.Repos[0].Branch != "polyforge/"+wi || out.Repos[0].PushedAt == "" {
		t.Fatalf("row content: %+v", out.Repos[0])
	}
}

func TestShipLedgerUpsertLatestWins(t *testing.T) {
	_, m, wi := ledgerEnv(t)
	if err := m.Record(wi, "b", []api.ShipLedgerEntry{{Repo: "r", BranchTopSHA: "aaa"}}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := m.Record(wi, "b", []api.ShipLedgerEntry{{Repo: "r", BranchTopSHA: "bbb"}}); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	n, err := m.ledgerCount(wi)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ledger carries %d rows, want 1 (upsert, not append)", n)
	}
	out, err := m.Ledger(wi)
	if err != nil || len(out.Repos) != 1 || out.Repos[0].BranchTopSHA != "bbb" {
		t.Fatalf("latest top must win: %v %+v", err, out)
	}
}

func TestShipLedgerEmptyLedgerAnswersEmptyArray(t *testing.T) {
	_, m, wi := ledgerEnv(t)
	out, err := m.Ledger(wi)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if out == nil || out.Repos == nil || len(out.Repos) != 0 {
		t.Fatalf("no recorded ship must answer an empty array, got %+v", out)
	}
}

func TestShipLedgerRecordingRefusals(t *testing.T) {
	cases := []struct {
		name    string
		branch  string
		entries []api.ShipLedgerEntry
		wantSub string
	}{
		{"no repos", "b", nil, "no repos"},
		{"blank repo name", "b", []api.ShipLedgerEntry{{Repo: "  ", BranchTopSHA: "a"}}, "no repo name"},
		{"blank sha", "b", []api.ShipLedgerEntry{{Repo: "r", BranchTopSHA: " "}}, "no branch_top_sha"},
		{"duplicate repo", "b", []api.ShipLedgerEntry{{Repo: "r", BranchTopSHA: "a"}, {Repo: "r", BranchTopSHA: "b"}}, "twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, m, wi := ledgerEnv(t)
			err := m.Record(wi, c.branch, c.entries)
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("want a refusal naming %q, got %v", c.wantSub, err)
			}
			if !IsCode(err, api.ErrCoreBadInput) {
				t.Fatalf("want CORE-400, got %v", err)
			}
		})
	}
	t.Run("unknown WI refused", func(t *testing.T) {
		_, m, _ := ledgerEnv(t)
		err := m.Record("wi_nope", "b", []api.ShipLedgerEntry{{Repo: "r", BranchTopSHA: "a"}})
		if !IsCode(err, api.ErrCoreNotFound) {
			t.Fatalf("want CORE-404, got %v", err)
		}
	})
}
