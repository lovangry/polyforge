package main

import (
	"testing"

	"github.com/silgrid/polyforge/internal/aihub/core"
	"github.com/silgrid/polyforge/internal/aihub/git"
	"github.com/silgrid/polyforge/internal/testenv"
)

// TestMain isolates HOME/XDG/POLYFORGE_* so no test can touch the real install.
func TestMain(m *testing.M) { testenv.Main(m) }

// TestNewServerSharesOneMemoryStore pins repo review 2026-10-16 B-3: the
// background reconcile runs on the very store the HTTP face serves from,
// so reconcile and Create share one per-entry lock table.
func TestNewServerSharesOneMemoryStore(t *testing.T) {
	dir := t.TempDir()
	db, err := core.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	gitMgr, err := git.NewManager(dir, core.NewOwnerManager(db))
	if err != nil {
		t.Fatal(err)
	}
	srv, mems := newServer(db, dir, gitMgr)
	if mems == nil || mems != srv.Mems {
		t.Fatalf("reconcile store %p is not the serving store %p", mems, srv.Mems)
	}
	if mems.ReconcileProgress == nil {
		t.Fatal("reconcile progress logger not wired on the shared store")
	}
}
