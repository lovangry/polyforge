package daemon

// ship_ledger_test.go: the ship bridge's ledger recording pins
// (polyforge#123, P1b) — a successful ship records each pushed (and each
// re-used) repo's branch-top SHA through the hub face, keyed by WI; a
// ledger recording failure fails the ship op with the typed DAEMON-514
// (the ledger is accounting, not best-effort).

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/cli"
)

// shipLedgerFixture is TestShip's sealed-capsule setup, reused: one task
// repo with its own origin, wrapped so the WI is shippable.
func shipLedgerFixture(t *testing.T, td *testDaemon) (origin string, head string) {
	t.Helper()
	td.submitClaim(t, "wi_ledger")
	capDir := filepath.Join(td.cfg.Mapping.Workspaces()[0], "capsules", "wi_ledger")

	origin = filepath.Join(t.TempDir(), "task-origin.git")
	if out, err := exec.Command("git", "init", "--bare", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare origin: %v: %s", err, out)
	}
	repo := filepath.Join(capDir, "repos", "app")
	if out, err := exec.Command("git", "clone", origin, repo).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "work"}, {"push", "origin", "HEAD:refs/heads/main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head = string(out[:len(out)-1])

	if _, err := td.client.Wrap("wi_ledger", "wrap"); err != nil {
		t.Fatalf("wrap: %v", err)
	}
	waitGone(t, td, "wi_ledger")
	return origin, head
}

func TestShipRecordsLedgerPerRepo(t *testing.T) {
	td := newTestDaemon(t, nil)
	_, head := shipLedgerFixture(t, td)

	raw, err := td.client.Ship("wi_ledger")
	if err != nil {
		t.Fatalf("ship: %v", err)
	}
	var sr cli.ShipResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		t.Fatalf("ship undecodable: %v", err)
	}
	// The hub face carries one ledger entry per shipped repo, with the
	// branch top ship pushed.
	entries := td.hub.shipLedgers["wi_ledger"]
	if len(entries) != 1 {
		t.Fatalf("ship recorded %d ledger entries, want 1 (%+v)", len(entries), td.hub.shipLedgerCalls)
	}
	if entries[0].Repo != "app" || entries[0].BranchTopSHA != head {
		t.Fatalf("ledger entry = %+v, want repo app at %s", entries[0], head)
	}

	// Idempotent re-ship: the reused repo is recorded again (latest
	// delivery wins on the aihub side).
	if _, err := td.client.Ship("wi_ledger"); err != nil {
		t.Fatalf("re-ship: %v", err)
	}
	if len(td.hub.shipLedgers["wi_ledger"]) != 2 {
		t.Fatalf("re-ship recorded %d entries total, want 2", len(td.hub.shipLedgers["wi_ledger"]))
	}
}

func TestShipLedgerFailureFailsShipTyped(t *testing.T) {
	td := newTestDaemon(t, nil)
	shipLedgerFixture(t, td)
	td.hub.mu.Lock()
	td.hub.ledgerErr = &api.Error{Code: api.ErrCoreInternal, Detail: "fixture: ledger down"}
	td.hub.mu.Unlock()

	_, err := td.client.Ship("wi_ledger")
	if err == nil {
		t.Fatal("ship succeeded while the ledger recording failed; want DAEMON-514")
	}
	ae, ok := err.(*api.Error)
	if !ok || ae.Code != api.ErrDaemonShipLedger {
		t.Fatalf("want typed %s, got %v", api.ErrDaemonShipLedger, err)
	}
}
