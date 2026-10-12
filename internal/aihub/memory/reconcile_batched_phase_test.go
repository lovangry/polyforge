package memory

// reconcile_batched_phase_test.go (polyforge#129 rewind): regression
// guards for the NON-steady-state (batched) phase of the reconcile — the
// full-rebuild pass a pre-#129 database degrades to on first boot after
// the upgrade, plus index repair. The phase is the
// bulk of every such pass (root-cause E1: ~99% of the old pass was the
// two FTS rewrite loops), and the first fix attempt left two blocking
// gaps in exactly this phase:
//
//   - ruling c: it emitted no progress — report() fired only in the walk
//     and at the final line, so a full-rebuild pass went silent for
//     minutes (the 'indistinguishable from a hang' pathology behind one
//     operator misjudgment and one wrongful SIGQUIT kill). Per batch
//     rotation the phase now reports scanned/reindexed/elapsed through
//     the same ReconcileProgress callback.
//   - ruling b①: one batch was one write transaction holding Core's ONLY
//     connection (SetMaxOpenConns(1)) for ftsRebuildBatch entries' worth
//     of index writes, so every concurrent request — read or write —
//     queued for the whole batch. The batch is now bounded by a wall-time
//     hold budget (ftsRebuildHold) besides the entry count, so a request
//     arriving mid-batch waits hundreds of milliseconds, never batch
//     count × per-entry cost.
//
// Both tests use the NULL-stamp corpus — the exact shape of the upgrade
// boot (every row queued for the batched index write), the same corpus
// shape as TestReconcileUnstampedRowsFullPassOnce scaled up.

import (
	"fmt"
	"testing"
	"time"
)

// nullAllStamps clears every row's freshness stamp: the pre-#129
// database shape, so the next pass runs the full batched one.
func nullAllStamps(t *testing.T, e *env) {
	t.Helper()
	if _, err := e.db.Exec(`UPDATE memories SET fs_mtime_ns=NULL, fs_size=NULL`); err != nil {
		t.Fatalf("clear stamps: %v", err)
	}
}

// TestReconcileBatchedPhaseReportsProgress: a pass with queued entries
// (the upgrade boot) must report progress DURING the batched index-write
// phase — per batch rotation, Reindexed growing with the writes — not
// only before and after it (polyforge#129 ruling c).
func TestReconcileBatchedPhaseReportsProgress(t *testing.T) {
	e, _ := stampTestEnv(t)
	const corpus = 96 // > 2 batches at ftsRebuildBatch, < progressEvery: no walk-phase reports
	stampSeed(t, e, corpus)
	nullAllStamps(t, e)

	type obs struct {
		p  ProgressReport
		at time.Time
	}
	var reports []obs // ReconcileProgress fires inline on the pass goroutine; no concurrency
	e.store.ReconcileProgress = func(p ProgressReport) {
		reports = append(reports, obs{p: p, at: time.Now()})
	}

	n, err := e.store.RebuildIndex()
	if err != nil {
		t.Fatalf("batched pass: %v", err)
	}
	if n != corpus {
		t.Fatalf("batched pass brought %d current, want %d", n, corpus)
	}
	if len(reports) == 0 {
		t.Fatal("no progress reports at all for a full batched pass")
	}
	final := reports[len(reports)-1].p
	if final.Reindexed != corpus || final.Scanned != corpus {
		t.Fatalf("final progress report says scanned %d / reindexed %d, want %d/%d",
			final.Scanned, final.Reindexed, corpus, corpus)
	}

	// Mid-phase reports: everything before the final line. The corpus is
	// smaller than progressEvery, so the walk emits none — every mid
	// report with Reindexed>0 comes from inside the batched phase, after
	// a batch rotation committed.
	var mid []obs
	for _, r := range reports[:len(reports)-1] {
		if r.p.Reindexed > 0 {
			mid = append(mid, r)
		}
	}
	if len(mid) < 2 {
		t.Fatalf("only %d progress report(s) observed DURING the batched phase "+
			"(%d reports total) — the phase that dominates a full-rebuild pass "+
			"went silent (polyforge#129 ruling c: per-batch progress required)", len(mid), len(reports))
	}
	for i := 1; i < len(mid); i++ {
		if mid[i].p.Reindexed <= mid[i-1].p.Reindexed {
			t.Fatalf("mid-phase progress not advancing: reindexed %d then %d — "+
				"the batched phase must report growth as its batches commit",
				mid[i-1].p.Reindexed, mid[i].p.Reindexed)
		}
	}
	if span := mid[len(mid)-1].at.Sub(mid[0].at); span < 30*time.Millisecond {
		t.Fatalf("mid-phase reports bunched within %v — progress must be paced "+
			"with the batch writes, not emitted once at the phase boundary", span)
	}
	searchOne(t, e, "Tok000003") // the pass stays correct underneath the reporting
}

// TestReconcileBatchedPassKeepsRequestsResponsive: while the batched
// pass writes, a concurrent DB request — read (get/search) AND write
// (pf remember) — must complete within the pinned few-hundred-
// millisecond bound (polyforge#129 ruling b①), never queue for a whole
// batch of index writes.
func TestReconcileBatchedPassKeepsRequestsResponsive(t *testing.T) {
	e, _ := stampTestEnv(t)
	const corpus = 64 // ≥ 2 batches at ftsRebuildBatch: the phase outlasts the sampling
	ids := stampSeed(t, e, corpus)
	nullAllStamps(t, e)

	reportCh := make(chan ProgressReport, 128)
	e.store.ReconcileProgress = func(p ProgressReport) { reportCh <- p }

	done := make(chan error, 1)
	var brought int
	go func() {
		n, err := e.store.RebuildIndex()
		if err != nil {
			done <- err
			return
		}
		brought = n
		close(done)
	}()

	// Sample the serving path while the pass runs: a read every tick,
	// and a write too once the batched phase is underway (the first
	// Reindexed>0 report proves the walk is done — a Create must not
	// race it).
	var maxLat time.Duration
	samples, writes := 0, 0
	phaseSeen := false
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
sample:
	for samples < 64 {
		select {
		case <-done:
			break sample
		case p := <-reportCh:
			if p.Reindexed > 0 {
				phaseSeen = true
			}
		case <-tick.C:
			t0 := time.Now()
			if _, err := e.store.Get(ids[samples%len(ids)]); err != nil {
				t.Fatalf("concurrent get during batched pass: %v", err)
			}
			if phaseSeen && samples%2 == 0 {
				if _, err := e.store.Create(CreateRequest{
					Project: "alpha", Type: "experience.debug", Visibility: "project",
					Content: fmt.Sprintf("concurrent write during the batched pass, unique token CcWrite%06d.", writes),
				}, e.user); err != nil {
					t.Fatalf("concurrent create during batched pass: %v", err)
				}
				writes++
			}
			if lat := time.Since(t0); lat > maxLat {
				maxLat = lat
			}
			samples++
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("batched pass: %v", err)
	}
	if brought != corpus {
		t.Fatalf("batched pass brought %d current, want %d", brought, corpus)
	}
	if !phaseSeen {
		t.Fatal("no batched-phase progress observed — the pass never reported (ruling c broken)")
	}
	if samples < 3 {
		t.Fatalf("only %d request samples taken — the batched phase was too short to measure", samples)
	}
	if maxLat < 15*time.Millisecond {
		t.Fatalf("slowest concurrent request took only %v — no sample ever queued behind "+
			"an open batch; the measurement is vacuous", maxLat)
	}
	// The pinned bound is "hundreds of milliseconds": the mechanism holds
	// a batch open for at most ftsRebuildHold plus one entry (~15ms at
	// this corpus shape) plus the yield/handoff. 1s leaves CI headroom
	// while still failing a regression to an unbounded batch (~3.8s of
	// hold at the old 256-row batch and this per-entry cost).
	if maxLat > time.Second {
		t.Fatalf("concurrent request during the batched pass took %v (worst of %d samples, %d writes) — "+
			"polyforge#129 ruling b① pins write-request latency at a few hundred ms, "+
			"not the batch duration", maxLat, samples, writes)
	}
	t.Logf("batched-pass responsiveness: %d samples (%d writes), worst request latency %v", samples, writes, maxLat)
}
